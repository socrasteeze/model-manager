package store

import (
	"errors"
	"testing"
)

// seedPulledModel puts a model_file row in place, since pulled_file has a
// foreign key to it.
func seedPulledModel(t *testing.T, s *Store, sha string) {
	t.Helper()
	run, err := s.BeginScanRun("/models")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertFileAndPath(
		ModelFile{SHA256: sha, ProbeSHA256: "p", Size: 100, Format: "safetensors"},
		FilePath{SHA256: sha, Path: "/models/" + sha + ".safetensors", Root: "/models",
			Device: 1, Inode: 1, Size: 100, MtimeNs: 1, Present: true, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
}

// One row per copy. Keyed on (sha256, upstream) alone, the second call
// overwrote the first's row and the first file became permanently unevictable:
// its path matched no recorded copy, so the guard refused it as "not the copy
// this daemon pulled" -- about a file this daemon had pulled.
func TestPulledFileIsKeyedPerPath(t *testing.T) {
	s := openTemp(t)
	sha := "aa11"
	seedPulledModel(t, s, sha)

	for _, p := range []string{"/a/model.safetensors", "/b/model.safetensors"} {
		if err := s.PutPulledCopy(PulledCopy{
			SHA256: sha, Upstream: "http://nas:8737", Path: p, Root: "/", SizeBytes: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	copies, err := s.PulledCopies(sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("copies = %d, want one row per path: %+v", len(copies), copies)
	}

	// A re-pull to the same path is the same copy, not a third row.
	if err := s.PutPulledCopy(PulledCopy{
		SHA256: sha, Upstream: "http://nas:8737", Path: "/a/model.safetensors",
		Root: "/", SizeBytes: 200,
	}); err != nil {
		t.Fatal(err)
	}
	if copies, _ = s.PulledCopies(sha); len(copies) != 2 {
		t.Fatalf("copies after a same-path re-pull = %d, want 2: %+v", len(copies), copies)
	}
}

// The path predicate on MarkPulledEvicted. Without it, evicting one copy marks
// every copy of the model evicted -- and the library would offer to re-pull a
// file that is still sitting on disk.
func TestMarkPulledEvictedTouchesOnlyTheNamedCopy(t *testing.T) {
	s := openTemp(t)
	sha := "bb22"
	seedPulledModel(t, s, sha)

	for _, p := range []string{"/a/model.safetensors", "/b/model.safetensors"} {
		if err := s.PutPulledCopy(PulledCopy{
			SHA256: sha, Upstream: "http://nas:8737", Path: p, Root: "/", SizeBytes: 100,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.MarkPulledEvicted(sha, "http://nas:8737", "/a/model.safetensors"); err != nil {
		t.Fatal(err)
	}

	copies, err := s.PulledCopies(sha)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range copies {
		switch c.Path {
		case "/a/model.safetensors":
			if c.Resident() {
				t.Error("the named copy was not marked evicted")
			}
		case "/b/model.safetensors":
			if !c.Resident() {
				t.Error("an unnamed copy was marked evicted; it is still on disk")
			}
		}
	}

	// Resident listings must follow: the survivor is still reclaimable space.
	resident, err := s.ResidentPullsFor(sha, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(resident) != 1 || resident[0].Path != "/b/model.safetensors" {
		t.Fatalf("resident = %+v, want only the surviving copy", resident)
	}

	// Naming a path nothing recorded is not a silent no-op: it means the caller
	// believes something this table does not, and eviction acts on that belief.
	if err := s.MarkPulledEvicted(sha, "http://nas:8737", "/c/nope.safetensors"); !errors.Is(err, ErrNoPulledCopy) {
		t.Errorf("marking an unrecorded path evicted returned %v, want ErrNoPulledCopy", err)
	}
}

// Two shapes of ambiguity need two messages, because they need two different
// flags to resolve. Saying "pulled from 2 upstreams" for two copies from one
// upstream was a false statement about the data pointing at a flag that could
// not narrow it.
func TestResidentPullDistinguishesTheTwoAmbiguities(t *testing.T) {
	s := openTemp(t)
	sha := "cc33"
	seedPulledModel(t, s, sha)

	put := func(upstream, path string) {
		t.Helper()
		if err := s.PutPulledCopy(PulledCopy{
			SHA256: sha, Upstream: upstream, Path: path, Root: "/", SizeBytes: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	put("http://nas:8737", "/a/model.safetensors")
	put("http://nas:8737", "/b/model.safetensors")
	_, err := s.ResidentPull(sha, "")
	if err == nil {
		t.Fatal("two copies from one upstream resolved to one")
	}
	if !contains(err.Error(), "name the path") {
		t.Errorf("error = %q, want it to ask for a path", err)
	}
	if contains(err.Error(), "upstreams") {
		t.Errorf("error = %q, still claims several upstreams", err)
	}

	put("http://other:8737", "/c/model.safetensors")
	_, err = s.ResidentPull(sha, "")
	if err == nil {
		t.Fatal("three copies across two upstreams resolved to one")
	}
	if !contains(err.Error(), "upstreams") {
		t.Errorf("error = %q, want it to ask for an upstream", err)
	}

	// Naming the upstream narrows it to that upstream's copies, which is still
	// two -- so it must then ask for the path rather than picking.
	if _, err = s.ResidentPull(sha, "http://other:8737"); err != nil {
		t.Errorf("naming an upstream with one copy failed: %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
