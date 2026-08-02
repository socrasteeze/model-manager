package comfy

// Making somebody else's workflow render *this* model.
//
// The obvious design was to have the user hand-edit {{placeholders}} into every
// graph. That is work nobody should do: ComfyUI already ships a working
// template per architecture, and the user's own saved workflows already render
// correctly. What is wrong with them is only that they name one hardcoded
// model.
//
// So instead of asking for a parameterized graph, this finds the handful of
// inputs that need to differ per model and rewrites them -- on a copy, at
// render time, leaving the file on disk untouched. Point the app at an
// unmodified ComfyUI template and it works.
//
// What is rewritten, and what deliberately is not:
//
//	lora        always      -- the entire point; without it every thumbnail in
//	                           the library is the same picture
//	seed        always      -- otherwise every model shares one composition
//	prompt      when known  -- the model's trigger words, or what the caller typed
//	negative    when given
//	checkpoint  when set    -- only if a checkpoint is configured for the family.
//	                           A stock template names a model the user probably
//	                           has; overwriting it uninvited would break a graph
//	                           that was working.
//
// Every rewrite is reported. A graph where no lora input was found is a warning
// the user can see, not a thumbnail that is quietly of the wrong thing.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Substitution is one input this pass changed.
type Substitution struct {
	Node  string `json:"node"`
	Class string `json:"class"`
	Input string `json:"input"`
	Was   any    `json:"was"`
	Now   any    `json:"now"`
}

// RewireResult is a graph ready to queue, and what was done to it.
type RewireResult struct {
	Graph         json.RawMessage `json:"graph"`
	Substitutions []Substitution  `json:"substitutions"`
	Warnings      []Warning       `json:"warnings"`
}

// A graph is decoded as plain maps rather than into a struct, so every key of
// every node survives the round trip. ComfyUI carries `_meta` today and will
// carry something else tomorrow, and a rewrite pass that silently dropped
// fields it did not recognise would corrupt graphs it was only meant to adjust.
type graphNodes map[string]map[string]any

// decodeGraph reads a graph with numbers kept as their original text.
//
// Without UseNumber every JSON number becomes a float64, and re-encoding turns
// a seed like 156680208700286 into 1.56680208700286e+14. That is still valid
// JSON, but it reaches ComfyUI as a float where a node expects an integer -- so
// a pass meant to change three inputs would quietly rewrite every large number
// in the graph. Numbers this code does not touch must come out byte-identical.
func decodeGraph(raw json.RawMessage) (graphNodes, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	var nodes graphNodes
	if err := dec.Decode(&nodes); err != nil {
		return nil, fmt.Errorf("comfy: workflow is not an API-format graph: %w", err)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("comfy: workflow is empty")
	}
	return nodes, nil
}

func classOf(n map[string]any) string {
	s, _ := n["class_type"].(string)
	return s
}

func inputsOf(n map[string]any) map[string]any {
	in, _ := n["inputs"].(map[string]any)
	return in
}

// nodeIDs returns the ids in a stable order.
//
// Go randomises map iteration, and without this the sampler that tracePrompts
// happens to find -- and therefore which node gets the prompt -- would differ
// between runs. Reproducibility is the same reason the seed is derived rather
// than random.
func nodeIDs(nodes graphNodes) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, errA := strconv.Atoi(ids[i])
		b, errB := strconv.Atoi(ids[j])
		if errA == nil && errB == nil {
			return a < b
		}
		return ids[i] < ids[j]
	})
	return ids
}

// Rewire adapts a graph to render one specific model.
//
// `template` is the graph *before* Fill ran, and is what keeps the two passes
// from fighting. An input whose template value contained a placeholder is
// author-controlled: `"masterpiece, {{triggers}}"` becomes `"masterpiece, glow"`,
// and rewiring must not then flatten it to `"glow"` and throw the author's
// wrapping away. Comparing the filled value to the one rewiring would write is
// not enough, because the two are similar rather than equal.
//
// Pass nil when there was no template pass -- an adopted graph, say -- and every
// input is fair game.
func Rewire(graph, template json.RawMessage, v Vars) (*RewireResult, error) {
	nodes, err := decodeGraph(graph)
	if err != nil {
		return nil, err
	}
	authored := authoredInputs(template)

	res := &RewireResult{Substitutions: []Substitution{}, Warnings: []Warning{}}
	set := func(id, class, input string, now any) {
		if authored[id][input] {
			return // the author placed a placeholder here; it is theirs
		}
		inputs := inputsOf(nodes[id])
		if inputs == nil {
			return
		}
		was := inputs[input]
		if fmt.Sprint(was) == fmt.Sprint(now) {
			return // already right, usually because Fill put it there
		}
		inputs[input] = now
		res.Substitutions = append(res.Substitutions, Substitution{
			Node: id, Class: class, Input: input, Was: was, Now: now,
		})
	}

	loraFound := false
	for _, id := range nodeIDs(nodes) {
		n := nodes[id]
		inputs := inputsOf(n)
		if inputs == nil {
			continue
		}
		class := classOf(n)

		// A lora loader by any name: LoraLoader, LoraLoaderModelOnly, and the
		// several community variants that all keep the `lora_name` input.
		if v.Model != "" && strings.Contains(strings.ToLower(class), "loraloader") {
			if _, ok := inputs["lora_name"]; ok {
				set(id, class, "lora_name", v.Model)
				loraFound = true
			}
		}

		// The checkpoint input differs per architecture -- ckpt_name for the
		// SDXL family's CheckpointLoaderSimple, unet_name for the Flux family's
		// UNETLoader -- so it is matched by input name rather than class.
		if v.Checkpoint != "" {
			for _, field := range []string{"ckpt_name", "unet_name"} {
				if _, ok := inputs[field]; ok {
					set(id, class, field, v.Checkpoint)
				}
			}
		}

		if v.Seed != 0 {
			for _, field := range []string{"seed", "noise_seed"} {
				if cur, ok := inputs[field]; ok {
					// Only numeric seeds. A `seed` that is a link to another
					// node is somebody's deliberate wiring, not a widget value.
					if _, isLink := cur.([]any); !isLink {
						set(id, class, field, v.Seed)
					}
				}
			}
		}
	}

	// Prompts are traced through the sampler rather than guessed from node
	// order. "The first CLIPTextEncode is the positive one" is wrong about half
	// the time, and a swapped prompt pair produces a thumbnail that is actively
	// misleading rather than merely wrong.
	positive, negative := tracePrompts(nodes)
	if prompt := promptText(v); prompt != "" && positive != "" {
		if _, has := inputsOf(nodes[positive])["text"]; has {
			set(positive, classOf(nodes[positive]), "text", prompt)
		}
	}
	if v.Negative != "" && negative != "" {
		if _, has := inputsOf(nodes[negative])["text"]; has {
			set(negative, classOf(nodes[negative]), "text", v.Negative)
		}
	}

	if v.Model != "" && !loraFound {
		res.Warnings = append(res.Warnings, Warning{
			Code: WarnNoLoraInput,
			Message: "This workflow has no lora loader, so the model being previewed " +
				"is never loaded and every thumbnail it renders will look the same. " +
				"That is only correct if you are previewing checkpoints.",
		})
	}

	out, err := json.Marshal(nodes)
	if err != nil {
		return nil, err
	}
	res.Graph = out
	return res, nil
}

// authoredInputs maps node id -> input name for every input a placeholder
// controls, and which is therefore the author's rather than this pass's to
// rewrite.
//
// It cannot simply parse the template and look for "{{": a template is not
// valid JSON before Fill runs, because a numeric placeholder like
// `"seed": {{seed}}` sits where a number belongs and quoting it would break
// ComfyUI. So instead the template is filled twice with two entirely different
// sets of values and the results compared. Any input that differs between the
// two passes is placeholder-driven, whatever its type and wherever it sits.
//
// This cannot false-positive on literal text, and it needs no knowledge of
// where placeholders are allowed to appear.
func authoredInputs(template json.RawMessage) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if len(template) == 0 {
		return out
	}

	probeA := Vars{
		Model: "\x00a", Checkpoint: "\x00b", Name: "\x00c", BaseModel: "\x00d",
		TriggerWords: []string{"\x00e"}, Prompt: "\x00f", Negative: "\x00g", Seed: 11,
	}
	probeB := Vars{
		Model: "\x00h", Checkpoint: "\x00i", Name: "\x00j", BaseModel: "\x00k",
		TriggerWords: []string{"\x00l"}, Prompt: "\x00m", Negative: "\x00n", Seed: 22,
	}

	a, errA := Fill(template, probeA)
	b, errB := Fill(template, probeB)
	if errA != nil || errB != nil {
		// A template that will not fill is not a reason to fail here; it just
		// means nothing is known to be authored, and the caller will surface
		// the real error.
		return out
	}

	nodesA, errPA := decodeGraph(a)
	nodesB, errPB := decodeGraph(b)
	if errPA != nil || errPB != nil {
		return out
	}

	for id, n := range nodesA {
		inA := inputsOf(n)
		inB := inputsOf(nodesB[id])
		for field, valA := range inA {
			if fmt.Sprint(valA) != fmt.Sprint(inB[field]) {
				if out[id] == nil {
					out[id] = map[string]bool{}
				}
				out[id][field] = true
			}
		}
	}
	return out
}

// promptText picks what the positive prompt should say.
//
// The caller's text wins; otherwise the model's trigger words, which are the
// thing most likely to make a lora actually show up in its own thumbnail.
func promptText(v Vars) string {
	if strings.TrimSpace(v.Prompt) != "" {
		return v.Prompt
	}
	if len(v.TriggerWords) > 0 {
		return strings.Join(v.TriggerWords, ", ")
	}
	return ""
}

// tracePrompts follows a sampler's positive and negative inputs to the node ids
// that feed them.
//
// Returns ("", "") when there is no sampler to trace from, which is a real case
// -- some graphs use SamplerCustomAdvanced with a separate guider -- and is
// handled by simply not touching the prompts rather than by guessing.
func tracePrompts(nodes graphNodes) (positive, negative string) {
	// A conditioning link is ["<node id>", <output index>].
	linkTarget := func(v any) string {
		arr, ok := v.([]any)
		if !ok || len(arr) == 0 {
			return ""
		}
		id, _ := arr[0].(string)
		return id
	}

	for _, id := range nodeIDs(nodes) {
		inputs := inputsOf(nodes[id])
		if inputs == nil {
			continue
		}
		p := linkTarget(inputs["positive"])
		neg := linkTarget(inputs["negative"])
		if p == "" && neg == "" {
			continue
		}
		// Follow through a guidance node, which sits between the encoder and
		// the sampler on Flux-shaped graphs and carries the conditioning
		// forward under the same name.
		positive = resolveConditioning(nodes, p, 0)
		negative = resolveConditioning(nodes, neg, 0)
		return positive, negative
	}
	return "", ""
}

// resolveConditioning walks back from a conditioning input to the node that
// actually holds the text.
func resolveConditioning(nodes graphNodes, id string, depth int) string {
	if id == "" || depth > 8 {
		return ""
	}
	inputs := inputsOf(nodes[id])
	if inputs == nil {
		return ""
	}
	if _, has := inputs["text"]; has {
		return id
	}
	// Not a text encoder: something like FluxGuidance or ConditioningZeroOut
	// wrapping one. Follow its conditioning input.
	for _, field := range []string{"conditioning", "conditioning_1", "input"} {
		if arr, ok := inputs[field].([]any); ok && len(arr) > 0 {
			if next, ok := arr[0].(string); ok {
				return resolveConditioning(nodes, next, depth+1)
			}
		}
	}
	return ""
}
