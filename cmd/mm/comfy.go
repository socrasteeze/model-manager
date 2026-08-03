package main

// mm comfy -- the terminal loop for getting a workflow working.
//
// Iterating on a graph through a browser means a render per attempt, and a
// render is tens of seconds of GPU. These subcommands answer the same questions
// without queueing anything: is this file usable, and what exactly would be
// sent for a given model.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/socrasteeze/model-manager/internal/basemodel"
	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/comfy"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/thumb"
)

const comfyUsage = `mm comfy <subcommand>

Prepare and check ComfyUI workflows for thumbnail rendering.

SUBCOMMANDS
    check <file.json>     Is this a usable workflow? Lints and reports.
    plan  <file.json>     Show what would be sent for a model, without rendering.
    adopt <image.png>     Print the API-format graph a ComfyUI render carries.

The graph is yours: point the app at an unmodified ComfyUI template and it
rewrites the lora, the seed and the prompt per model at render time. See
docs/comfyui-workflows.md.`

func cmdComfy(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(comfyUsage)
		return nil
	}
	switch args[0] {
	case "check":
		return cmdComfyCheck(args[1:])
	case "plan":
		return cmdComfyPlan(args[1:])
	case "adopt":
		return cmdComfyAdopt(args[1:])
	case "-h", "--help", "help":
		fmt.Print(comfyUsage)
		return nil
	}
	return fmt.Errorf("comfy: unknown subcommand %q", args[0])
}

// takeFile pulls a leading positional argument out before flag parsing.
//
// Go's flag package stops at the first non-flag argument, so `plan file.json
// --sha X` would leave the flags unparsed. Lifting the filename out first makes
// both orders work, which is what anyone would expect from a command whose
// usage line puts the file first.
func takeFile(args []string) (string, []string) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a, append(append([]string{}, args[:i]...), args[i+1:]...)
		}
		// Stop at the first flag: anything after it may be that flag's value,
		// and guessing which flags take values is how this goes wrong.
		break
	}
	return "", args
}

func readGraph(path string) (json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var probe json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %w", filepath.Base(path), err)
	}
	return probe, nil
}

func cmdComfyCheck(args []string) error {
	fs := newFlagSet("comfy check", `mm comfy check <file.json>

Reports whether a workflow can be used to render thumbnails, and what looks
wrong about it. Exits non-zero only when the file cannot be used at all --
warnings are advice, not failures.`)
	file, rest := takeFile(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if file == "" {
		file = fs.Arg(0)
	}
	if file == "" {
		return errors.New("comfy check: one workflow file is required")
	}

	graph, err := readGraph(file)
	if err != nil {
		return err
	}
	if err := comfy.CheckAPIFormat(graph); err != nil {
		fmt.Printf("UNUSABLE  %s\n\n%v\n", file, err)
		os.Exit(1)
	}

	warnings := comfy.Lint(graph)
	fmt.Printf("OK        %s\n", file)
	if len(warnings) == 0 {
		fmt.Println("\nNothing to flag.")
		return nil
	}
	fmt.Println()
	for _, warn := range warnings {
		fmt.Printf("  [%s] %s\n", warn.Code, warn.Message)
	}
	return nil
}

func cmdComfyPlan(args []string) error {
	fs := newFlagSet("comfy plan", `mm comfy plan <file.json> --sha SHA256

Shows exactly what would be queued for one model: which inputs get rewritten,
from what to what. Renders nothing and contacts nothing.`)

	dbPath := fs.String("db", defaultDBPath(), "path to the master database")
	sha := fs.String("sha", "", "model to plan for (SHA256)")
	checkpoint := fs.String("checkpoint", "", "base checkpoint to substitute")
	prompt := fs.String("prompt", "", "prompt to substitute")
	showGraph := fs.Bool("graph", false, "print the resulting graph")
	allowNetDB := fs.Bool("allow-network-db", false, "permit a database on a network filesystem")
	file, rest := takeFile(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if file == "" {
		file = fs.Arg(0)
	}
	if file == "" {
		return errors.New("comfy plan: one workflow file is required")
	}

	graph, err := readGraph(file)
	if err != nil {
		return err
	}

	vars := comfy.Vars{Checkpoint: *checkpoint, Prompt: *prompt}
	if *sha != "" {
		st, err := openStore(*dbPath, *allowNetDB)
		if err != nil {
			return err
		}
		defer st.Close()

		vars.Seed = comfy.SeedFor(*sha)
		rec, err := st.GetModelRecord(*sha)
		if err == nil && rec != nil {
			vars.Name = rec.Name
			vars.BaseModel = basemodel.Normalize(rec.BaseModel)
			vars.TriggerWords = rec.TriggerWords
		}
		paths, err := st.PathsFor(*sha)
		if err == nil {
			for _, p := range paths {
				if p.Present {
					vars.Model = store.FilenameOf(p.Path)
					break
				}
			}
			// Same fallback the render endpoint uses: no path is currently
			// present, but the plan should still show what a render would
			// name, not refuse to show anything.
			if vars.Model == "" && len(paths) > 0 {
				vars.Model = store.FilenameOf(paths[0].Path)
			}
		}
		if vars.Model == "" {
			return fmt.Errorf("comfy plan: no present file for %s", *sha)
		}
		// The per-family configured checkpoint, same as a real render would
		// use, unless the caller passed --checkpoint.
		if vars.Checkpoint == "" {
			if raw, err := st.GetSetting(store.SettingComfyCheckpoint); err == nil {
				vars.Checkpoint = comfy.CheckpointForFamily(raw, vars.BaseModel)
			}
		}
	} else {
		// Without a model, the plan still shows the shape -- which inputs would
		// be touched -- using an obvious stand-in.
		vars.Model = "<the model being previewed>"
		vars.Seed = 1
	}

	filled, err := comfy.Fill(graph, vars)
	if err != nil {
		return err
	}
	res, err := comfy.Rewire(filled, graph, vars)
	if err != nil {
		return err
	}

	fmt.Printf("workflow   %s\n", file)
	fmt.Printf("model      %s\n", vars.Model)
	if vars.BaseModel != "" {
		fmt.Printf("family     %s\n", vars.BaseModel)
	}
	if vars.Checkpoint != "" {
		fmt.Printf("checkpoint %s\n", vars.Checkpoint)
	} else {
		fmt.Printf("checkpoint (not set — the workflow's own is kept)\n")
	}
	fmt.Printf("seed       %d\n\n", vars.Seed)

	if len(res.Substitutions) == 0 {
		fmt.Println("Nothing would be rewritten. That usually means the workflow has no")
		fmt.Println("lora loader, so every model would get the same picture.")
	} else {
		fmt.Println("Would rewrite:")
		for _, sub := range res.Substitutions {
			fmt.Printf("  node %-4s %-24s %s\n", sub.Node, sub.Class, sub.Input)
			fmt.Printf("           %v\n        -> %v\n", sub.Was, sub.Now)
		}
	}
	for _, warn := range comfy.MergeWarnings(res.Warnings, comfy.Lint(graph)) {
		fmt.Printf("\n  [%s] %s\n", warn.Code, warn.Message)
	}
	if *showGraph {
		pretty, _ := json.MarshalIndent(json.RawMessage(res.Graph), "", "  ")
		fmt.Printf("\n%s\n", pretty)
	}
	return nil
}

func cmdComfyAdopt(args []string) error {
	fs := newFlagSet("comfy adopt", `mm comfy adopt <image.png>

Prints the API-format graph a ComfyUI render carries in its PNG metadata, so it
can be saved as a workflow file. Redirect it:

    mm comfy adopt render.png > flux2-preview.json`)

	file, rest := takeFile(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if file == "" {
		file = fs.Arg(0)
	}
	if file == "" {
		return errors.New("comfy adopt: one image is required")
	}

	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if !blobstore.IsImage(data) {
		return fmt.Errorf("%s is not an image", file)
	}

	chunk, workflow, err := thumb.ExtractWorkflow(data)
	if err != nil {
		return fmt.Errorf("%s carries no ComfyUI workflow", filepath.Base(file))
	}
	if err := comfy.CheckAPIFormat(workflow); err != nil {
		return fmt.Errorf("%s carries the %q chunk, which is the editor format: %w",
			filepath.Base(file), chunk, err)
	}

	pretty, err := json.MarshalIndent(json.RawMessage(workflow), "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(pretty))

	if warnings := comfy.Lint(workflow); len(warnings) > 0 {
		for _, warn := range warnings {
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", warn.Code, warn.Message)
		}
	}
	return nil
}
