package store

import (
	"path/filepath"
	"testing"

	"github.com/socrasteeze/model-manager/internal/provenance"
)

func searchStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "master.db"), Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type seedSpec struct {
	sha       string
	path      string
	format    string
	size      int64
	name      string
	typ       string
	baseModel string
	triggers  []string
	tags      []string
	present   bool
}

func seedSearch(t *testing.T, s *Store, spec seedSpec) {
	t.Helper()
	run, err := s.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	format := spec.format
	if format == "" {
		format = "safetensors"
	}
	size := spec.size
	if size == 0 {
		size = 1000
	}
	if err := s.UpsertFileAndPath(
		ModelFile{SHA256: spec.sha, ProbeSHA256: "p" + spec.sha, Size: size, Format: format},
		FilePath{SHA256: spec.sha, Path: spec.path, Root: "/models",
			Device: 1, Inode: fnv(spec.sha), Size: size, MtimeNs: 1, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}

	var obs []FieldObservation
	if spec.name != "" {
		obs = append(obs, FieldObservation{Field: provenance.FieldName, Value: spec.name})
	}
	if spec.typ != "" {
		obs = append(obs, FieldObservation{Field: provenance.FieldType, Value: spec.typ})
	}
	if spec.baseModel != "" {
		obs = append(obs, FieldObservation{Field: provenance.FieldBaseModel, Value: spec.baseModel})
	}
	if len(spec.triggers) > 0 {
		obs = append(obs, FieldObservation{Field: provenance.FieldTriggerWords, Value: spec.triggers})
	}
	if len(obs) > 0 {
		if err := s.RecordObservations(spec.sha, provenance.SourceCivitai, obs); err != nil {
			t.Fatal(err)
		}
	}
	if len(spec.tags) > 0 {
		if err := s.SetTags(spec.sha, provenance.SourceCivitai, spec.tags); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ResolveModel(spec.sha); err != nil {
		t.Fatal(err)
	}
	if !spec.present {
		if _, err := s.db.Exec(`UPDATE model_file_path SET present = 0 WHERE sha256 = ?`, spec.sha); err != nil {
			t.Fatal(err)
		}
	}
}

func fnv(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

func library(t *testing.T) *Store {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{
		sha: "a", path: "/models/loras/cinematic_style_v2.safetensors",
		name: "Cinematic Style", typ: "lora", baseModel: "SDXL",
		triggers: []string{"cinelight"}, tags: []string{"style", "lighting"},
		size: 200 << 20, present: true,
	})
	seedSearch(t, s, seedSpec{
		sha: "b", path: "/models/checkpoints/juggernaut.safetensors",
		name: "Juggernaut XL", typ: "checkpoint", baseModel: "SDXL",
		tags: []string{"photoreal"}, size: 6 << 30, present: true,
	})
	seedSearch(t, s, seedSpec{
		sha: "c", path: "/models/loras/anime_flux.safetensors",
		name: "Anime Flux", typ: "lora", baseModel: "Flux",
		triggers: []string{"animestyle"}, tags: []string{"style"},
		size: 100 << 20, present: true,
	})
	seedSearch(t, s, seedSpec{
		sha: "d", path: "/models/loras/gone.safetensors",
		name: "Vanished", typ: "lora", baseModel: "SDXL", present: false,
	})
	return s
}

func shas(res *SearchResults) []string {
	out := make([]string, 0, len(res.Hits))
	for _, h := range res.Hits {
		out = append(out, h.SHA256)
	}
	return out
}

func TestSearchByName(t *testing.T) {
	s := library(t)
	res, err := s.Search(SearchQuery{Text: "cinematic"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].SHA256 != "a" {
		t.Fatalf("hits = %v, want [a]", shas(res))
	}
}

// Local models are named cinematic_style_v2.safetensors, so a search for a word
// inside the filename has to work or use case 1 fails on the majority of a
// library that has no metadata yet.
func TestSearchMatchesInsideFilenames(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{
		sha: "x", path: "/models/loras/my_special_thing_v3.safetensors", present: true,
	})
	res, err := s.Search(SearchQuery{Text: "special"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("hits = %v, want the filename match", shas(res))
	}
}

func TestSearchByTriggerWord(t *testing.T) {
	s := library(t)
	res, _ := s.Search(SearchQuery{Text: "cinelight"})
	if len(res.Hits) != 1 || res.Hits[0].SHA256 != "a" {
		t.Fatalf("hits = %v", shas(res))
	}
}

// Search should feel responsive as it is typed, so the final token is a prefix.
func TestSearchIsPrefixMatchedOnTheLastToken(t *testing.T) {
	s := library(t)
	res, _ := s.Search(SearchQuery{Text: "jugger"})
	if len(res.Hits) != 1 || res.Hits[0].SHA256 != "b" {
		t.Fatalf("hits = %v, want the prefix match", shas(res))
	}
}

// FTS5 has its own operator syntax. Raw user input would make a stray quote or a
// bare AND into a syntax error rather than a search.
func TestSearchSurvivesFTSMetacharacters(t *testing.T) {
	s := library(t)
	for _, text := range []string{
		`"`, `AND`, `OR`, `NOT`, `-`, `*`, `^`, `cinematic OR`, `foo:bar`,
		`"unbalanced`, `a AND (b`, `NEAR(a b)`,
	} {
		if _, err := s.Search(SearchQuery{Text: text}); err != nil {
			t.Errorf("Search(%q) errored: %v", text, err)
		}
	}
}

func TestSearchFilters(t *testing.T) {
	s := library(t)

	res, _ := s.Search(SearchQuery{Types: []string{"lora"}})
	if res.Total != 3 {
		t.Errorf("type filter returned %d, want 3", res.Total)
	}

	res, _ = s.Search(SearchQuery{BaseModels: []string{"Flux"}})
	if res.Total != 1 || res.Hits[0].SHA256 != "c" {
		t.Errorf("base model filter = %v", shas(res))
	}

	res, _ = s.Search(SearchQuery{Types: []string{"lora"}, BaseModels: []string{"SDXL"}})
	if res.Total != 2 {
		t.Errorf("combined filters returned %d, want 2", res.Total)
	}
}

// Adding a second tag should narrow, not widen. Any-match would make tags
// useless for finding anything in a 19k library.
func TestTagFilterRequiresAllTags(t *testing.T) {
	s := library(t)

	res, _ := s.Search(SearchQuery{Tags: []string{"style"}})
	if res.Total != 2 {
		t.Fatalf("one tag returned %d, want 2", res.Total)
	}
	res, _ = s.Search(SearchQuery{Tags: []string{"style", "lighting"}})
	if res.Total != 1 || res.Hits[0].SHA256 != "a" {
		t.Fatalf("two tags returned %v, want only the model carrying both", shas(res))
	}
}

func TestPresentFilter(t *testing.T) {
	s := library(t)
	yes, no := true, false

	res, _ := s.Search(SearchQuery{Present: &yes})
	if res.Total != 3 {
		t.Errorf("present=true returned %d, want 3", res.Total)
	}
	res, _ = s.Search(SearchQuery{Present: &no})
	if res.Total != 1 || res.Hits[0].SHA256 != "d" {
		t.Errorf("present=false returned %v, want the vanished model", shas(res))
	}
}

// Use case 10: spot the gaps.
func TestNeedsAttentionFindsIncompleteRecords(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "full", path: "/models/a.safetensors",
		name: "Complete", typ: "lora", baseModel: "SDXL", present: true})
	seedSearch(t, s, seedSpec{sha: "bare", path: "/models/b.safetensors", present: true})

	res, _ := s.Search(SearchQuery{NeedsAttention: true})
	// Neither has a preview, so both qualify -- the point is that it is a
	// superset that always includes the genuinely empty record.
	found := false
	for _, h := range res.Hits {
		if h.SHA256 == "bare" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the record with no metadata was not flagged: %v", shas(res))
	}
}

func TestSortAndPaging(t *testing.T) {
	s := library(t)

	res, _ := s.Search(SearchQuery{Sort: "size", Desc: true, Limit: 1})
	if len(res.Hits) != 1 || res.Hits[0].SHA256 != "b" {
		t.Fatalf("largest = %v, want the 6GiB checkpoint", shas(res))
	}
	if res.Total != 4 {
		t.Fatalf("total = %d, want the unpaged count", res.Total)
	}

	// Paging must not repeat or skip rows.
	seen := map[string]bool{}
	for offset := 0; offset < 4; offset += 2 {
		page, _ := s.Search(SearchQuery{Sort: "name", Limit: 2, Offset: offset})
		for _, h := range page.Hits {
			if seen[h.SHA256] {
				t.Fatalf("%s appeared on two pages", h.SHA256)
			}
			seen[h.SHA256] = true
		}
	}
	if len(seen) != 4 {
		t.Fatalf("paging returned %d distinct rows, want 4", len(seen))
	}
}

// Ties on the sort column must not make paging non-deterministic.
func TestPagingIsStableAcrossTiedSortKeys(t *testing.T) {
	s := searchStore(t)
	for _, sha := range []string{"t1", "t2", "t3", "t4"} {
		seedSearch(t, s, seedSpec{
			sha: sha, path: "/models/" + sha + ".safetensors",
			name: "Identical Name", size: 100, present: true,
		})
	}
	first, _ := s.Search(SearchQuery{Sort: "name", Limit: 2, Offset: 0})
	second, _ := s.Search(SearchQuery{Sort: "name", Limit: 2, Offset: 2})

	overlap := map[string]bool{}
	for _, h := range first.Hits {
		overlap[h.SHA256] = true
	}
	for _, h := range second.Hits {
		if overlap[h.SHA256] {
			t.Fatalf("%s appeared on both pages despite identical sort keys", h.SHA256)
		}
	}
}

func TestSearchResultCarriesUsableFields(t *testing.T) {
	s := library(t)
	res, _ := s.Search(SearchQuery{Text: "cinematic"})
	h := res.Hits[0]

	if h.Filename != "cinematic_style_v2.safetensors" {
		t.Errorf("Filename = %q", h.Filename)
	}
	if len(h.TriggerWords) != 1 || h.TriggerWords[0] != "cinelight" {
		t.Errorf("TriggerWords = %v", h.TriggerWords)
	}
	if len(h.Tags) != 2 {
		t.Errorf("Tags = %v", h.Tags)
	}
	if !h.Present || h.PathCount != 1 {
		t.Errorf("presence = %v/%d", h.Present, h.PathCount)
	}
}

// Editing a value in the UI must make it findable immediately.
func TestIndexTracksResolvedChanges(t *testing.T) {
	s := library(t)

	if err := s.RecordObservations("a", provenance.SourceManual,
		[]FieldObservation{{Field: provenance.FieldName, Value: "Renamed Entirely"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveModel("a"); err != nil {
		t.Fatal(err)
	}

	res, _ := s.Search(SearchQuery{Text: "Renamed"})
	if len(res.Hits) != 1 || res.Hits[0].SHA256 != "a" {
		t.Fatalf("the renamed model was not findable: %v", shas(res))
	}
	res, _ = s.Search(SearchQuery{Text: "Cinematic Style"})
	for _, h := range res.Hits {
		if h.SHA256 == "a" && h.Name != "Renamed Entirely" {
			t.Fatal("stale name still indexed")
		}
	}
}

func TestReindexSearchRebuildsEverything(t *testing.T) {
	s := library(t)
	if _, err := s.db.Exec(`DELETE FROM model_search`); err != nil {
		t.Fatal(err)
	}
	if res, _ := s.Search(SearchQuery{Text: "cinematic"}); len(res.Hits) != 0 {
		t.Fatal("index was not actually cleared")
	}

	n, err := s.ReindexSearch(nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("reindexed %d, want 4", n)
	}
	if res, _ := s.Search(SearchQuery{Text: "cinematic"}); len(res.Hits) != 1 {
		t.Fatal("rebuild did not restore searchability")
	}
}

func TestFacetCounts(t *testing.T) {
	s := library(t)
	f, err := s.FacetCounts()
	if err != nil {
		t.Fatal(err)
	}
	if f.Types["lora"] != 3 || f.Types["checkpoint"] != 1 {
		t.Errorf("types = %v", f.Types)
	}
	if f.BaseModels["SDXL"] != 3 || f.BaseModels["Flux"] != 1 {
		t.Errorf("base models = %v", f.BaseModels)
	}
	if f.Tags["style"] != 2 {
		t.Errorf("tags = %v", f.Tags)
	}
	if f.Total != 4 {
		t.Errorf("total = %d", f.Total)
	}
}

func TestBuildFTSQuery(t *testing.T) {
	cases := map[string]string{
		"one":      `"one"*`,
		"one two":  `"one" AND "two"*`,
		`say "hi"`: `"say" AND "hi"*`,
		"a:b":      `"ab"*`,
		"   ":      `""`,
		"":         `""`,
	}
	for in, want := range cases {
		if got := buildFTSQuery(in); got != want {
			t.Errorf("buildFTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchLimitIsCapped(t *testing.T) {
	s := library(t)
	res, _ := s.Search(SearchQuery{Limit: 100000})
	if res.Limit > 500 {
		t.Fatalf("limit = %d, want it capped", res.Limit)
	}
}

func TestFilenameOfHandlesBothSeparators(t *testing.T) {
	cases := map[string]string{
		"/models/a/b.safetensors":   "b.safetensors",
		`C:\models\a\b.safetensors`: "b.safetensors",
		"bare.safetensors":          "bare.safetensors",
		"":                          "",
	}
	for in, want := range cases {
		if got := filenameOf(in); got != want {
			t.Errorf("filenameOf(%q) = %q, want %q", in, got, want)
		}
	}
}
