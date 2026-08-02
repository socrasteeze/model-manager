package comfy

// Checking a workflow before it is used for a thumbnail.
//
// ComfyUI will happily run a graph that is valid and useless for this purpose:
// one the model being previewed can never get into, or one that renders
// beautifully and saves nothing. Both produce a "successful" render that either
// shows the wrong thing or comes back empty.
//
// What is deliberately *not* warned about: a hardcoded checkpoint, a hardcoded
// seed, a hardcoded prompt. Those are the normal state of a workflow exported
// from ComfyUI, and rewiring handles all three at render time. Warning about
// them would be telling the user to fix something that is already working.
//
// These are warnings, not refusals. A workflow with no lora loader is odd but
// might be deliberate -- previewing checkpoints has no separate model to load --
// and this app does not get to overrule that. It just has to say so.

import (
	"encoding/json"
	"strings"
)

// Warning is one thing that looks wrong about a workflow.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Warning codes, so callers can dedupe and the docs can name them.
const (
	WarnNotAPIFormat = "not_api_format"
	WarnNoLoraInput  = "no_lora_input"
	WarnNoSaveNode   = "no_save_node"
)

// Lint reports what would make a graph a poor thumbnail workflow.
func Lint(template json.RawMessage) []Warning {
	// Shape first: everything below assumes an API-format graph, and saying
	// "this is the canvas format" is more useful than several warnings that all
	// follow from it.
	if err := checkAPIFormat(template); err != nil {
		return []Warning{{Code: WarnNotAPIFormat, Message: err.Error()}}
	}

	nodes, err := decodeGraph(template)
	if err != nil {
		return []Warning{{Code: WarnNotAPIFormat, Message: err.Error()}}
	}

	// Non-nil, so a JSON caller gets `[]` rather than `null` for "nothing wrong".
	// A null there reads as "not checked", which is a different claim.
	out := []Warning{}
	text := string(template)

	// Can the model being previewed get into this graph at all? Either through
	// a lora loader that rewiring can find, or through a {{model}} placeholder
	// the author placed by hand. Neither means every thumbnail this workflow
	// renders will be the same picture.
	loadsModel := strings.Contains(text, "{{model}}")
	saves := false
	for _, n := range nodes {
		class := strings.ToLower(classOf(n))
		if strings.Contains(class, "loraloader") {
			if _, ok := inputsOf(n)["lora_name"]; ok {
				loadsModel = true
			}
		}
		// PreviewImage also surfaces in /history outputs, so either counts.
		switch classOf(n) {
		case "SaveImage", "PreviewImage":
			saves = true
		}
	}

	if !loadsModel {
		out = append(out, Warning{
			Code: WarnNoLoraInput,
			Message: "This workflow has no lora loader, so the model being previewed " +
				"is never loaded and every thumbnail it renders will look the same. " +
				"That is only correct if you are previewing checkpoints.",
		})
	}
	if !saves {
		out = append(out, Warning{
			Code: WarnNoSaveNode,
			Message: "This workflow has no SaveImage or PreviewImage node, so it will " +
				"run and produce nothing to attach.",
		})
	}
	return out
}

// MergeWarnings concatenates warning lists, keeping the first of each code.
//
// Lint and Rewire both notice a missing lora loader -- one statically, one while
// actually looking for the input -- and reporting it twice would read as two
// problems.
func MergeWarnings(lists ...[]Warning) []Warning {
	seen := map[string]bool{}
	out := []Warning{}
	for _, list := range lists {
		for _, w := range list {
			if seen[w.Code] {
				continue
			}
			seen[w.Code] = true
			out = append(out, w)
		}
	}
	return out
}
