package comfy

import (
	"bytes"
	"encoding/json"
	"testing"
)

// stockSDXL is shaped like a ComfyUI template as saved from the canvas: node
// ids out of order, a hardcoded lora and checkpoint, a fixed seed, and -- the
// part that matters -- the negative encoder declared *before* the positive one.
// A pass that assumed "the first CLIPTextEncode is positive" would swap them.
const stockSDXL = `{
  "4": {"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"someone_elses.safetensors"}},
  "7": {"class_type":"CLIPTextEncode","inputs":{"text":"ugly, deformed","clip":["10",1]}},
  "6": {"class_type":"CLIPTextEncode","inputs":{"text":"a cat in a hat","clip":["10",1]}},
  "10": {"class_type":"LoraLoader","inputs":{
     "lora_name":"their_lora.safetensors","strength_model":1,"strength_clip":1,
     "model":["4",0],"clip":["4",1]}},
  "5": {"class_type":"EmptyLatentImage","inputs":{"width":1024,"height":1024,"batch_size":1}},
  "3": {"class_type":"KSampler","inputs":{
     "seed":123456,"steps":20,"cfg":8,"sampler_name":"euler","scheduler":"normal","denoise":1,
     "model":["10",0],"positive":["6",0],"negative":["7",0],"latent_image":["5",0]}},
  "8": {"class_type":"VAEDecode","inputs":{"samples":["3",0],"vae":["4",2]}},
  "9": {"class_type":"SaveImage","inputs":{"filename_prefix":"ComfyUI","images":["8",0]}}
}`

func decode(t *testing.T, raw json.RawMessage) graphNodes {
	t.Helper()
	var g graphNodes
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatalf("result is not a graph: %v", err)
	}
	return g
}

func input(t *testing.T, g graphNodes, id, field string) any {
	t.Helper()
	in := inputsOf(g[id])
	if in == nil {
		t.Fatalf("node %s has no inputs", id)
	}
	return in[field]
}

// The whole point of the feature: an unmodified template renders *this* model.
func TestAnUnmodifiedTemplateIsRewiredToTheModel(t *testing.T) {
	res, err := Rewire(json.RawMessage(stockSDXL), nil, Vars{
		Model: "neon-ink.safetensors", Seed: 4242,
	})
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, res.Graph)

	if got := input(t, g, "10", "lora_name"); got != "neon-ink.safetensors" {
		t.Errorf("lora_name = %v; the template's own lora survived", got)
	}
	if got := input(t, g, "3", "seed"); got != float64(4242) {
		t.Errorf("seed = %v, want 4242", got)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %+v", res.Warnings)
	}
}

// The check that motivates tracing rather than guessing. In `stockSDXL` the
// negative encoder (node 7) comes first in the document; only following the
// sampler's `positive` link finds node 6.
func TestPromptsAreTracedThroughTheSamplerNotGuessed(t *testing.T) {
	res, err := Rewire(json.RawMessage(stockSDXL), nil, Vars{
		Model:        "x.safetensors",
		TriggerWords: []string{"neon ink", "glowing"},
		Negative:     "blurry",
		Seed:         1,
	})
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, res.Graph)

	if got := input(t, g, "6", "text"); got != "neon ink, glowing" {
		t.Errorf("positive node 6 = %v", got)
	}
	if got := input(t, g, "7", "text"); got != "blurry" {
		t.Errorf("negative node 7 = %v", got)
	}
}

// A stock template names a checkpoint the user probably has. Overwriting it
// uninvited would break a graph that was working.
func TestCheckpointIsLeftAloneUnlessOneIsConfigured(t *testing.T) {
	res, err := Rewire(json.RawMessage(stockSDXL), nil, Vars{Model: "x.safetensors", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, res.Graph)
	if got := input(t, g, "4", "ckpt_name"); got != "someone_elses.safetensors" {
		t.Errorf("ckpt_name = %v; it should have been left alone", got)
	}

	res, err = Rewire(json.RawMessage(stockSDXL), nil, Vars{
		Model: "x.safetensors", Checkpoint: "mine.safetensors", Seed: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	g = decode(t, res.Graph)
	if got := input(t, g, "4", "ckpt_name"); got != "mine.safetensors" {
		t.Errorf("ckpt_name = %v; a configured checkpoint should win", got)
	}
}

// Flux-shaped graphs put the checkpoint in unet_name and route conditioning
// through a guidance node before the sampler sees it.
func TestFluxShapedGraphIsRewiredToo(t *testing.T) {
	const fluxish = `{
      "1": {"class_type":"UNETLoader","inputs":{"unet_name":"theirs.safetensors","weight_dtype":"default"}},
      "2": {"class_type":"DualCLIPLoader","inputs":{"clip_name1":"clip_l.safetensors","clip_name2":"t5xxl.safetensors","type":"flux"}},
      "3": {"class_type":"LoraLoaderModelOnly","inputs":{"lora_name":"theirs_lora.safetensors","strength_model":1,"model":["1",0]}},
      "4": {"class_type":"CLIPTextEncode","inputs":{"text":"a landscape","clip":["2",0]}},
      "5": {"class_type":"FluxGuidance","inputs":{"guidance":3.5,"conditioning":["4",0]}},
      "6": {"class_type":"KSampler","inputs":{"seed":7,"model":["3",0],"positive":["5",0],"negative":["5",0],"latent_image":["7",0]}},
      "7": {"class_type":"EmptySD3LatentImage","inputs":{"width":1024,"height":1024,"batch_size":1}},
      "9": {"class_type":"SaveImage","inputs":{"images":["6",0]}}
    }`

	res, err := Rewire(json.RawMessage(fluxish), nil, Vars{
		Model: "portrait.safetensors", Checkpoint: "flux2-klein.safetensors",
		TriggerWords: []string{"cinematic"}, Seed: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, res.Graph)

	if got := input(t, g, "3", "lora_name"); got != "portrait.safetensors" {
		t.Errorf("lora_name = %v", got)
	}
	if got := input(t, g, "1", "unet_name"); got != "flux2-klein.safetensors" {
		t.Errorf("unet_name = %v", got)
	}
	// Traced *through* FluxGuidance to the encoder behind it.
	if got := input(t, g, "4", "text"); got != "cinematic" {
		t.Errorf("prompt did not reach the encoder behind the guidance node: %v", got)
	}
	// The CLIP and VAE filenames are per-family constants, not per-model, and
	// must survive untouched.
	if got := input(t, g, "2", "clip_name2"); got != "t5xxl.safetensors" {
		t.Errorf("a text-encoder filename was rewritten: %v", got)
	}
}

// A graph with no lora loader renders the same picture for every model. Worth
// saying, not worth refusing -- previewing checkpoints is legitimate.
func TestNoLoraLoaderWarnsRatherThanFailing(t *testing.T) {
	const noLora = `{
      "1": {"class_type":"CheckpointLoaderSimple","inputs":{"ckpt_name":"base.safetensors"}},
      "9": {"class_type":"SaveImage","inputs":{"images":["1",0]}}
    }`
	res, err := Rewire(json.RawMessage(noLora), nil, Vars{Model: "x.safetensors", Seed: 1})
	if err != nil {
		t.Fatalf("a checkpoint-only graph should still work: %v", err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != "no_lora_input" {
		t.Errorf("warnings = %+v", res.Warnings)
	}
}

// Placeholders are the precise-control path and must win: Fill runs first, and
// rewiring must not undo what it did.
func TestRewiringDoesNotFightPlaceholders(t *testing.T) {
	tmpl := json.RawMessage(`{
      "1": {"class_type":"LoraLoader","inputs":{"lora_name":"{{model}}"}},
      "2": {"class_type":"CLIPTextEncode","inputs":{"text":"masterpiece, {{triggers}}"}},
      "3": {"class_type":"KSampler","inputs":{"seed":{{seed}},"positive":["2",0]}},
      "9": {"class_type":"SaveImage","inputs":{"images":["3",0]}}
    }`)
	vars := Vars{Model: "mine.safetensors", TriggerWords: []string{"glow"}, Seed: 5}

	filled, err := Fill(tmpl, vars)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Rewire(filled, tmpl, vars)
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, res.Graph)

	// The author's hand-written wrapping around the trigger words survives:
	// rewiring only replaces what a placeholder did not already set.
	if got := input(t, g, "2", "text"); got != "masterpiece, glow" {
		t.Errorf("the author's prompt wrapping was overwritten: %v", got)
	}
	for _, sub := range res.Substitutions {
		if sub.Node == "1" && sub.Input == "lora_name" {
			t.Errorf("a value Fill already set was substituted again: %+v", sub)
		}
	}
}

// Unknown node fields must survive. ComfyUI carries `_meta` today and will
// carry something else tomorrow; a rewrite that dropped what it did not
// recognise would corrupt graphs it was only meant to adjust.
func TestUnknownNodeFieldsSurvive(t *testing.T) {
	const withMeta = `{
      "1": {"class_type":"LoraLoader","inputs":{"lora_name":"a.safetensors"},
            "_meta":{"title":"Load LoRA"},"future_field":{"x":1}},
      "9": {"class_type":"SaveImage","inputs":{"images":["1",0]}}
    }`
	res, err := Rewire(json.RawMessage(withMeta), nil, Vars{Model: "b.safetensors", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, res.Graph)

	meta, ok := g["1"]["_meta"].(map[string]any)
	if !ok || meta["title"] != "Load LoRA" {
		t.Errorf("_meta was dropped: %+v", g["1"])
	}
	if _, ok := g["1"]["future_field"]; !ok {
		t.Errorf("an unrecognised field was dropped: %+v", g["1"])
	}
}

// A seed wired from another node is somebody's deliberate graph, not a widget
// value to stomp on.
func TestALinkedSeedIsNotOverwritten(t *testing.T) {
	const linked = `{
      "1": {"class_type":"RandomNoise","inputs":{"noise_seed":["2",0]}},
      "2": {"class_type":"SomeSeedNode","inputs":{"value":7}},
      "9": {"class_type":"SaveImage","inputs":{"images":["1",0]}}
    }`
	res, err := Rewire(json.RawMessage(linked), nil, Vars{Seed: 999})
	if err != nil {
		t.Fatal(err)
	}
	g := decode(t, res.Graph)
	if _, stillLinked := input(t, g, "1", "noise_seed").([]any); !stillLinked {
		t.Errorf("a linked seed was replaced with a literal: %v", input(t, g, "1", "noise_seed"))
	}
}

// Rewiring must be reproducible: the same graph and model give the same result
// every time, or a re-render would silently produce a different picture.
func TestRewiringIsDeterministic(t *testing.T) {
	vars := Vars{Model: "x.safetensors", TriggerWords: []string{"a"}, Negative: "b", Seed: 3}
	first, err := Rewire(json.RawMessage(stockSDXL), nil, vars)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := Rewire(json.RawMessage(stockSDXL), nil, vars)
		if err != nil {
			t.Fatal(err)
		}
		if string(again.Graph) != string(first.Graph) {
			t.Fatalf("run %d differed:\n%s\n%s", i, first.Graph, again.Graph)
		}
	}
}

func TestRewireRejectsGraphsItCannotRead(t *testing.T) {
	if _, err := Rewire(json.RawMessage(`[]`), nil, Vars{}); err == nil {
		t.Error("accepted a JSON array as a graph")
	}
	if _, err := Rewire(json.RawMessage(`{}`), nil, Vars{}); err == nil {
		t.Error("accepted an empty graph")
	}
}

// Numbers this pass does not touch must come out byte-identical. Without
// UseNumber, a seed like 156680208700286 round-trips through float64 and comes
// back as 1.56680208700286e+14 -- still valid JSON, but a float where ComfyUI
// expects an integer. A pass meant to change two inputs would quietly rewrite
// every large number in the graph.
func TestUntouchedNumbersAreNotReformatted(t *testing.T) {
	const bigNumbers = `{
      "1": {"class_type":"LoraLoader","inputs":{"lora_name":"a.safetensors"}},
      "2": {"class_type":"SomeNode","inputs":{
         "big": 156680208700286, "precise": 1.0000001, "small": 8, "zero": 0}},
      "9": {"class_type":"SaveImage","inputs":{"images":["1",0]}}
    }`
	res, err := Rewire(json.RawMessage(bigNumbers), nil, Vars{Model: "b.safetensors"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"156680208700286", "1.0000001", `"small":8`, `"zero":0`} {
		if !bytes.Contains(res.Graph, []byte(want)) {
			t.Errorf("%s was reformatted:\n%s", want, res.Graph)
		}
	}
}

// The template's own negative prompt is its business, exactly like its
// checkpoint: it was tuned for that architecture and the graph worked. Only a
// negative the caller actually asked for replaces it.
func TestTheTemplatesNegativeSurvivesUnlessOneIsGiven(t *testing.T) {
	res, err := Rewire(json.RawMessage(stockSDXL), nil, Vars{Model: "x.safetensors", Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got := input(t, decode(t, res.Graph), "7", "text"); got != "ugly, deformed" {
		t.Errorf("negative = %v; it should have been left alone", got)
	}

	res, err = Rewire(json.RawMessage(stockSDXL), nil, Vars{
		Model: "x.safetensors", Negative: "mine", Seed: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := input(t, decode(t, res.Graph), "7", "text"); got != "mine" {
		t.Errorf("negative = %v; a supplied one should win", got)
	}
}

// `null` and `[]` are different claims to a JSON caller: one reads as "not
// checked", the other as "checked, nothing wrong".
func TestEmptyResultsAreEmptyListsNotNull(t *testing.T) {
	res, err := Rewire(json.RawMessage(stockSDXL), nil, Vars{Model: "x.safetensors"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(`"warnings":null`)) ||
		bytes.Contains(encoded, []byte(`"substitutions":null`)) {
		t.Errorf("null where an empty list belongs: %s", encoded)
	}
	if encoded, err := json.Marshal(Lint(json.RawMessage(stockSDXL))); err != nil {
		t.Fatal(err)
	} else if string(encoded) != "[]" {
		t.Errorf("Lint with nothing to say encoded as %s", encoded)
	}
}
