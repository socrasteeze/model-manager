package report

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/socrasteeze/model-manager/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type placement struct {
	path        string
	device, ino uint64
	provisional bool
}

func seed(t *testing.T, s *store.Store, run int64, sha, format string, size int64, weights string, places ...placement) {
	t.Helper()
	f := store.ModelFile{
		SHA256:        sha,
		WeightsSHA256: weights,
		ProbeSHA256:   "probe-" + sha,
		Size:          size,
		Format:        format,
	}
	for _, p := range places {
		err := s.UpsertFileAndPath(f, store.FilePath{
			SHA256: sha, Path: p.path, Root: "/models",
			Device: p.device, Inode: p.ino, Size: size, MtimeNs: 1,
			Provisional: p.provisional, ScanRunID: run,
		})
		if err != nil {
			t.Fatalf("seeding %s: %v", p.path, err)
		}
	}
}

func TestTotalsCountDistinctModels(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")

	seed(t, s, run, "aaa", "safetensors", 1000, "wa", placement{path: "/models/a", device: 1, ino: 10})
	// Same content in two places: one model, two files, 1000 bytes duplicated.
	seed(t, s, run, "bbb", "safetensors", 2000, "wb",
		placement{path: "/models/b1", device: 1, ino: 20},
		placement{path: "/models/b2", device: 1, ino: 21})

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	tot := rep.Totals

	if tot.DistinctModels != 2 {
		t.Errorf("DistinctModels = %d, want 2", tot.DistinctModels)
	}
	if tot.FileInstances != 3 {
		t.Errorf("FileInstances = %d, want 3", tot.FileInstances)
	}
	if tot.PathsPresent != 3 {
		t.Errorf("PathsPresent = %d, want 3", tot.PathsPresent)
	}
	if tot.BytesOnDisk != 5000 {
		t.Errorf("BytesOnDisk = %d, want 5000", tot.BytesOnDisk)
	}
	if tot.BytesDistinct != 3000 {
		t.Errorf("BytesDistinct = %d, want 3000", tot.BytesDistinct)
	}
	if tot.BytesDuplicated != 2000 {
		t.Errorf("BytesDuplicated = %d, want 2000", tot.BytesDuplicated)
	}
}

// A hardlink is two names for one inode: one file, occupying one file's space.
// Counting it as duplication would invent savings that deleting a name would not
// actually reclaim.
func TestHardlinkIsNotDuplication(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")

	seed(t, s, run, "aaa", "safetensors", 4096, "wa",
		placement{path: "/models/name1", device: 1, ino: 99},
		placement{path: "/models/name2", device: 1, ino: 99}) // same inode

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if rep.Totals.PathsPresent != 2 {
		t.Errorf("PathsPresent = %d, want 2", rep.Totals.PathsPresent)
	}
	if rep.Totals.FileInstances != 1 {
		t.Errorf("FileInstances = %d, want 1 -- one inode is one file", rep.Totals.FileInstances)
	}
	if rep.Totals.BytesDuplicated != 0 {
		t.Errorf("BytesDuplicated = %d, want 0 for a hardlink", rep.Totals.BytesDuplicated)
	}
	if len(rep.Duplicates) != 0 {
		t.Errorf("a hardlink was listed as a duplicate group: %+v", rep.Duplicates)
	}
}

func TestDuplicatesRankByWastedBytes(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")

	// Small file in three places: 2 x 100 wasted.
	seed(t, s, run, "small", "safetensors", 100, "ws",
		placement{path: "/models/s1", device: 1, ino: 1},
		placement{path: "/models/s2", device: 1, ino: 2},
		placement{path: "/models/s3", device: 1, ino: 3})
	// Big file in two places: 1 x 10000 wasted.
	seed(t, s, run, "big", "safetensors", 10000, "wb",
		placement{path: "/models/b1", device: 1, ino: 4},
		placement{path: "/models/b2", device: 1, ino: 5})

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(rep.Duplicates) != 2 {
		t.Fatalf("%d duplicate groups, want 2", len(rep.Duplicates))
	}
	if rep.Duplicates[0].SHA256 != "big" {
		t.Errorf("top duplicate = %s, want big (ranked by wasted bytes, not copy count)",
			rep.Duplicates[0].SHA256)
	}
	if rep.Duplicates[0].WastedBytes != 10000 {
		t.Errorf("WastedBytes = %d, want 10000", rep.Duplicates[0].WastedBytes)
	}
	if rep.Duplicates[1].WastedBytes != 200 {
		t.Errorf("second WastedBytes = %d, want 200", rep.Duplicates[1].WastedBytes)
	}
}

// An absent path is history, not inventory. It must not inflate any current
// count.
func TestAbsentPathsAreExcludedFromTotals(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")
	seed(t, s, run, "aaa", "safetensors", 500, "wa",
		placement{path: "/models/here", device: 1, ino: 1},
		placement{path: "/models/gone", device: 1, ino: 2})

	// A later run observes only one of them.
	run2, _ := s.BeginScanRun("/models")
	seed(t, s, run2, "aaa", "safetensors", 500, "wa", placement{path: "/models/here", device: 1, ino: 1})
	if _, err := s.SweepAbsentPaths("/models", run2); err != nil {
		t.Fatal(err)
	}

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if rep.Totals.PathsPresent != 1 {
		t.Errorf("PathsPresent = %d, want 1", rep.Totals.PathsPresent)
	}
	if rep.Totals.BytesOnDisk != 500 {
		t.Errorf("BytesOnDisk = %d, want 500", rep.Totals.BytesOnDisk)
	}
	if rep.Health.AbsentPaths != 1 {
		t.Errorf("AbsentPaths = %d, want 1", rep.Health.AbsentPaths)
	}
	if len(rep.Duplicates) != 0 {
		t.Error("an absent path was counted as a duplicate")
	}
}

func TestFormatBreakdownSeparatesExpectedNullWeights(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")

	seed(t, s, run, "st1", "safetensors", 100, "w1", placement{path: "/models/a", device: 1, ino: 1})
	// A pickle file: NULL weights hash is expected, not a defect.
	seed(t, s, run, "ck1", "ckpt", 200, "", placement{path: "/models/b", device: 1, ino: 2})
	// A safetensors whose framing failed: NULL weights hash IS a defect.
	seed(t, s, run, "st2", "safetensors", 300, "", placement{path: "/models/c", device: 1, ino: 3})

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	byFormat := map[string]FormatRow{}
	for _, f := range rep.Formats {
		byFormat[f.Format] = f
	}
	if got := byFormat["ckpt"].NoWeightsHash; got != 1 {
		t.Errorf("ckpt NoWeightsHash = %d, want 1", got)
	}
	if got := byFormat["safetensors"].NoWeightsHash; got != 1 {
		t.Errorf("safetensors NoWeightsHash = %d, want 1", got)
	}
	// FramingFailures counts only formats that should have parsed, so the pickle
	// file must not appear in it.
	if rep.Health.FramingFailures != 1 {
		t.Errorf("FramingFailures = %d, want 1 (the pickle file is not a failure)",
			rep.Health.FramingFailures)
	}
}

func TestSizeDistributionBuckets(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")

	seed(t, s, run, "tiny", "safetensors", 1<<20, "w1", placement{path: "/models/t", device: 1, ino: 1})
	seed(t, s, run, "lora", "safetensors", 200<<20, "w2", placement{path: "/models/l", device: 1, ino: 2})
	seed(t, s, run, "ckpt", "safetensors", 6<<30, "w3", placement{path: "/models/c", device: 1, ino: 3})

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	total := int64(0)
	for _, b := range rep.Sizes {
		total += b.DistinctModels
	}
	if total != 3 {
		t.Fatalf("buckets hold %d models, want 3 -- the ranges must cover every size", total)
	}

	want := map[string]int64{"< 16 MiB": 1, "128 – 512 MiB": 1, "2 – 8 GiB": 1}
	for _, b := range rep.Sizes {
		if w, ok := want[b.Label]; ok && b.DistinctModels != w {
			t.Errorf("bucket %q holds %d, want %d", b.Label, b.DistinctModels, w)
		}
	}
}

func TestProvisionalPathsSurfaceInHealth(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")
	seed(t, s, run, "aaa", "safetensors", 100, "wa",
		placement{path: "/models/confirmed", device: 1, ino: 1},
		placement{path: "/models/guessed", device: 1, ino: 2, provisional: true})

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if rep.Health.ProvisionalPaths != 1 {
		t.Fatalf("ProvisionalPaths = %d, want 1", rep.Health.ProvisionalPaths)
	}

	var buf bytes.Buffer
	if err := rep.Text(&buf); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !strings.Contains(buf.String(), "mm verify --provisional") {
		t.Error("the report does not tell the reader how to resolve provisional paths")
	}
}

// The reflink caveat is load-bearing: without it the duplication figure reads as
// reclaimable space, and on a btrfs array full of intentional reflinked views it
// is not (spec §9.4).
func TestTextIncludesReflinkCaveatWhenDuplicationExists(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")
	seed(t, s, run, "aaa", "safetensors", 1000, "wa",
		placement{path: "/models/a1", device: 1, ino: 1},
		placement{path: "/models/a2", device: 1, ino: 2})

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var buf bytes.Buffer
	if err := rep.Text(&buf); err != nil {
		t.Fatalf("Text: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "upper bound") || !strings.Contains(out, "share extents") {
		t.Error("duplication was reported without the shared-extent caveat")
	}
}

func TestTextOnEmptyDatabaseGivesTheNextStep(t *testing.T) {
	s := newStore(t)
	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var buf bytes.Buffer
	if err := rep.Text(&buf); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !strings.Contains(buf.String(), "mm scan") {
		t.Error("an empty report does not tell the reader what to run")
	}
}

// An interrupted run's numbers are partial, and a reader who is not told that
// will read them as the library's true state.
func TestTextFlagsIncompleteRuns(t *testing.T) {
	s := newStore(t)
	run, _ := s.BeginScanRun("/models")
	seed(t, s, run, "aaa", "safetensors", 100, "wa", placement{path: "/models/a", device: 1, ino: 1})
	if err := s.FinishScanRun(run, store.StatusInterrupted, store.ScanCounters{FilesSeen: 1}); err != nil {
		t.Fatal(err)
	}

	rep, err := Generate(s.DB(), "test.db", 10)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	var buf bytes.Buffer
	if err := rep.Text(&buf); err != nil {
		t.Fatalf("Text: %v", err)
	}
	if !strings.Contains(buf.String(), "did not complete") {
		t.Error("an interrupted run was reported without a warning")
	}
}

func TestHumanBytesAndComma(t *testing.T) {
	byteCases := map[int64]string{
		0: "0 B", 512: "512 B", 1024: "1.00 KiB",
		1536: "1.50 KiB", 1 << 30: "1.00 GiB", 7 * 1 << 40: "7.00 TiB",
	}
	for in, want := range byteCases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
	commaCases := map[int64]string{
		0: "0", 999: "999", 1000: "1,000", 19000: "19,000",
		1234567: "1,234,567", -4567: "-4,567",
	}
	for in, want := range commaCases {
		if got := comma(in); got != want {
			t.Errorf("comma(%d) = %q, want %q", in, got, want)
		}
	}
}
