package provenance

import (
	"testing"
	"time"
)

func cand(field, value, source string, ageMinutes int) Candidate {
	return Candidate{
		Field:      field,
		Value:      value,
		Source:     source,
		Tier:       TierOf(source),
		ObservedAt: time.Now().Add(-time.Duration(ageMinutes) * time.Minute),
	}
}

// The precedence that the entire design rests on: what the user typed outranks
// what the origin published, which outranks what another tool guessed.
func TestTierPrecedence(t *testing.T) {
	got, ok := Resolve([]Candidate{
		cand("base_model", `"SD 1.5"`, SourceSwarmUI, 1),
		cand("base_model", `"SDXL"`, SourceCivitai, 1),
		cand("base_model", `"Anima 2B"`, SourceManual, 1),
	})
	if !ok {
		t.Fatal("Resolve returned nothing")
	}
	if got.Source != SourceManual || got.Value != `"Anima 2B"` {
		t.Fatalf("winner = %s/%s, want manual/Anima 2B", got.Source, got.Value)
	}
	if len(got.Losers) != 2 {
		t.Fatalf("%d losers recorded, want 2", len(got.Losers))
	}
	// Losers stay ordered so the UI can show the runner-up first.
	if got.Losers[0].Source != SourceCivitai {
		t.Fatalf("first loser = %s, want civitai", got.Losers[0].Source)
	}
}

// A freshly-scraped sidecar must not beat a better source just by being newer.
// Otherwise any tool that rescans on a timer wins every argument eventually.
func TestRecencyDoesNotBeatTrust(t *testing.T) {
	got, _ := Resolve([]Candidate{
		cand("name", `"scraped just now"`, SourceA1111, 0),
		cand("name", `"from civitai an hour ago"`, SourceCivitai, 60),
	})
	if got.Source != SourceCivitai {
		t.Fatalf("winner = %s, want civitai -- recency outranked tier", got.Source)
	}
}

// §7.1's same-tier rule: a fixed per-source trust order first, recency only as
// the tie-break within one source's own history.
func TestSameTierResolvesByTrustThenRecency(t *testing.T) {
	got, _ := Resolve([]Candidate{
		cand("base_model", `"from swarm"`, SourceSwarmUI, 0),
		cand("base_model", `"from stability matrix"`, SourceStabilityMatrix, 30),
	})
	if got.Source != SourceStabilityMatrix {
		t.Fatalf("winner = %s, want stability_matrix (higher trust despite being older)", got.Source)
	}

	// Equal trust falls through to recency.
	a := cand("name", `"older"`, SourceSwarmUI, 60)
	b := cand("name", `"newer"`, SourceSwarmUI, 1)
	got, _ = Resolve([]Candidate{a, b})
	if got.Value != `"newer"` {
		t.Fatalf("winner = %s, want the newer value at equal trust", got.Value)
	}
}

// A safetensors header is data the trainer embedded in the file itself. It
// travels with the bytes rather than sitting in a sidecar another tool may have
// rewritten, so it outranks third-party scrapes -- while still losing to Origin.
func TestHeaderOutranksScrapesButLosesToOrigin(t *testing.T) {
	got, _ := Resolve([]Candidate{
		cand("base_model", `"scrape"`, SourceComfyUILoRAMgr, 0),
		cand("base_model", `"header"`, SourceSafetensorsHeader, 0),
	})
	if got.Source != SourceSafetensorsHeader {
		t.Fatalf("winner = %s, want the embedded header", got.Source)
	}

	got, _ = Resolve([]Candidate{
		cand("base_model", `"header"`, SourceSafetensorsHeader, 0),
		cand("base_model", `"origin"`, SourceCivitai, 0),
	})
	if got.Source != SourceCivitai {
		t.Fatalf("winner = %s, want civitai", got.Source)
	}
}

// Resolution must not depend on the order SQLite happened to return rows in, or
// the materialized record would flap between identical inputs.
func TestResolutionIsDeterministic(t *testing.T) {
	now := time.Now()
	mk := func(source string) Candidate {
		return Candidate{Field: "name", Value: `"v"`, Source: source, Tier: TierOf(source), ObservedAt: now}
	}
	// Two sources at identical tier, trust and timestamp.
	a := mk(SourceSwarmUI)
	b := mk(SourceSwarmUI)
	b.Source = "swarmui_clone"
	b.Tier = TierOf(b.Source)

	first, _ := Resolve([]Candidate{a, b})
	second, _ := Resolve([]Candidate{b, a})
	if first.Source != second.Source {
		t.Fatalf("resolution depends on input order: %s vs %s", first.Source, second.Source)
	}
}

// An unclassified source must not be able to outrank a typed value just because
// nobody has added it to the table yet.
func TestUnknownSourceDefaultsToLowestTier(t *testing.T) {
	if TierOf("some-new-tool") != TierTool {
		t.Fatal("unknown source did not default to the tool tier")
	}
	got, _ := Resolve([]Candidate{
		cand("name", `"typed by hand"`, SourceManual, 100),
		cand("name", `"from nowhere"`, "some-new-tool", 0),
	})
	if got.Source != SourceManual {
		t.Fatalf("winner = %s, want manual", got.Source)
	}
}

func TestResolveEmpty(t *testing.T) {
	if _, ok := Resolve(nil); ok {
		t.Fatal("Resolve(nil) reported a winner")
	}
}

// Manual wins, but a manual value that is simply wrong must not become invisible
// and permanent -- the disagreement is surfaced instead (§7.1).
func TestFindDisagreementsRaisesOriginVsManual(t *testing.T) {
	ds := FindDisagreements([]Candidate{
		cand("base_model", `"Anima 2B"`, SourceManual, 0),
		cand("base_model", `"SDXL"`, SourceCivitai, 0),
	})
	if len(ds) != 1 {
		t.Fatalf("%d disagreements, want 1", len(ds))
	}
	if ds[0].ManualValue != `"Anima 2B"` || ds[0].SuggestedValue != `"SDXL"` {
		t.Fatalf("disagreement carried the wrong values: %+v", ds[0])
	}
}

// A tool scrape contradicting a typed value is the normal state of the world.
// Surfacing those would bury the ones that matter.
func TestToolDisagreementIsNotSurfaced(t *testing.T) {
	ds := FindDisagreements([]Candidate{
		cand("base_model", `"Anima 2B"`, SourceManual, 0),
		cand("base_model", `"SD 1.5"`, SourceSwarmUI, 0),
	})
	if len(ds) != 0 {
		t.Fatalf("%d disagreements raised for a tool scrape, want 0", len(ds))
	}
}

func TestNoDisagreementWithoutManual(t *testing.T) {
	ds := FindDisagreements([]Candidate{
		cand("base_model", `"SDXL"`, SourceCivitai, 0),
		cand("base_model", `"SD 1.5"`, SourceSwarmUI, 0),
	})
	if len(ds) != 0 {
		t.Fatalf("%d disagreements without a manual value, want 0", len(ds))
	}
}

// Re-encoded JSON that means the same thing must not read as a conflict, or
// every re-ingest would generate suggestions nobody needs to act on.
func TestSemanticallyEqualValuesDoNotDisagree(t *testing.T) {
	cases := [][2]string{
		{`["a","b"]`, `["a","b"]`},
		{`{"x":1,"y":2}`, `{"y":2,"x":1}`},
		{`1`, `1.0`},
	}
	for _, c := range cases {
		ds := FindDisagreements([]Candidate{
			{Field: "f", Value: c[0], Source: SourceManual, Tier: TierManual},
			{Field: "f", Value: c[1], Source: SourceCivitai, Tier: TierOrigin},
		})
		if len(ds) != 0 {
			t.Errorf("%s vs %s raised a spurious disagreement", c[0], c[1])
		}
	}

	// A real difference still registers.
	ds := FindDisagreements([]Candidate{
		{Field: "f", Value: `["a"]`, Source: SourceManual, Tier: TierManual},
		{Field: "f", Value: `["b"]`, Source: SourceCivitai, Tier: TierOrigin},
	})
	if len(ds) != 1 {
		t.Error("a genuine difference was swallowed as equal")
	}
}

// Sidecars in the wild are not type-disciplined: a weight arrives as "0.8", an
// nsfw flag as 1, a single trigger word as a bare string.
func TestDecodersTolerateSloppyTypes(t *testing.T) {
	if s, ok := DecodeString(`"hello"`); !ok || s != "hello" {
		t.Errorf("DecodeString(string) = %q, %v", s, ok)
	}
	if s, ok := DecodeString(`42`); !ok || s != "42" {
		t.Errorf("DecodeString(number) = %q, %v", s, ok)
	}

	for _, in := range []string{`0.8`, `"0.8"`} {
		if f, ok := DecodeFloat(in); !ok || f != 0.8 {
			t.Errorf("DecodeFloat(%s) = %v, %v", in, f, ok)
		}
	}

	truthy := []string{`true`, `"true"`, `"True"`, `1`, `"yes"`}
	for _, in := range truthy {
		if b, ok := DecodeBool(in); !ok || !b {
			t.Errorf("DecodeBool(%s) = %v, %v; want true", in, b, ok)
		}
	}
	falsy := []string{`false`, `"false"`, `0`, `"no"`}
	for _, in := range falsy {
		if b, ok := DecodeBool(in); !ok || b {
			t.Errorf("DecodeBool(%s) = %v, %v; want false", in, b, ok)
		}
	}

	if got, ok := DecodeStringSlice(`["a","b"]`); !ok || len(got) != 2 {
		t.Errorf("DecodeStringSlice(array) = %v, %v", got, ok)
	}
	// A bare string where a list belongs is common enough to accept.
	if got, ok := DecodeStringSlice(`"solo trigger"`); !ok || len(got) != 1 || got[0] != "solo trigger" {
		t.Errorf("DecodeStringSlice(string) = %v, %v", got, ok)
	}
}
