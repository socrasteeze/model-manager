package store

import "testing"

// The load-bearing filter. A 404 only advances checked_at, and the default max
// age is zero so checked_at is not consulted either -- so without the gone stamp
// a taken-down model returns to the sweep queue on every run, forever. On a
// library that has outlived a few takedowns that becomes most of every sweep.
func TestGoneModelsLeaveTheSweepQueue(t *testing.T) {
	s := openTemp(t)
	run, _ := s.BeginScanRun("/models")
	for _, sha := range []string{"aa", "bb"} {
		if err := s.UpsertFileAndPath(
			ModelFile{SHA256: sha, ProbeSHA256: "p" + sha, Size: 1, Format: "safetensors"},
			FilePath{SHA256: sha, Path: "/models/" + sha, Root: "/models",
				Device: 1, Inode: 1, Size: 1, MtimeNs: 1, ScanRunID: run},
		); err != nil {
			t.Fatal(err)
		}
	}
	for i, sha := range []string{"aa", "bb"} {
		if err := s.PutModelOrigin(ModelOrigin{
			SHA256: sha, Provider: "civitai",
			ModelID: []string{"100", "200"}[i], VersionID: "1", Source: OriginSourceArchive,
		}); err != nil {
			t.Fatal(err)
		}
	}

	owned, err := s.OwnedOriginModels("civitai", 0, 0, false)
	if err != nil || len(owned) != 2 {
		t.Fatalf("owned = %d (%v), want 2", len(owned), err)
	}

	if err := s.MarkOriginModelGone("civitai", "100"); err != nil {
		t.Fatal(err)
	}
	owned, _ = s.OwnedOriginModels("civitai", 0, 0, false)
	if len(owned) != 1 || owned[0].ModelID != "200" {
		t.Fatalf("owned after a takedown = %+v, want only model 200", owned)
	}

	// And the operator can still ask whether any came back.
	owned, _ = s.OwnedOriginModels("civitai", 0, 0, true)
	if len(owned) != 2 {
		t.Errorf("recheckGone = %d models, want both", len(owned))
	}

	if gone, err := s.OriginModelGone("civitai", "100"); err != nil || !gone {
		t.Errorf("OriginModelGone = %v, %v", gone, err)
	}
	if gone, _ := s.OriginModelGone("civitai", "200"); gone {
		t.Error("a live model reported gone")
	}

	// Idempotent: re-confirming keeps the first date, which is the one worth
	// having.
	before, _ := s.OwnedOriginModels("civitai", 0, 0, true)
	_ = before
	var first string
	s.DB().QueryRow(`SELECT upstream_gone_at FROM origin_model_status
                      WHERE provider='civitai' AND origin_model_id='100'`).Scan(&first)
	if err := s.MarkOriginModelGone("civitai", "100"); err != nil {
		t.Fatal(err)
	}
	var second string
	s.DB().QueryRow(`SELECT upstream_gone_at FROM origin_model_status
                      WHERE provider='civitai' AND origin_model_id='100'`).Scan(&second)
	if first != second {
		t.Errorf("the takedown stamp moved from %q to %q", first, second)
	}
}

// A model 404'd on its very first check has no status row yet, so the stamp has
// to be able to create one rather than updating nothing.
func TestMarkOriginModelGoneCreatesTheStatusRow(t *testing.T) {
	s := openTemp(t)
	if err := s.MarkOriginModelGone("civitai", "555"); err != nil {
		t.Fatal(err)
	}
	if gone, err := s.OriginModelGone("civitai", "555"); err != nil || !gone {
		t.Errorf("OriginModelGone = %v, %v; the first 404 recorded nothing", gone, err)
	}
}

func item(provider, model, version string) ArchiveItem {
	return ArchiveItem{Provider: provider, ModelID: model, VersionID: version}
}

// A partial archive is the normal case, and which part is missing is what
// decides between retrying, waiting, and accepting it. One status column could
// not carry that.
func TestArchiveCompletenessIsPerStep(t *testing.T) {
	s := openTemp(t)
	a := item("civitai", "999", "4567")
	if err := s.PutArchiveItem(a); err != nil {
		t.Fatal(err)
	}

	got, err := s.ArchiveItemFor("civitai", "999", "4567")
	if err != nil || got == nil {
		t.Fatalf("ArchiveItemFor = %v, %v", got, err)
	}
	if got.Complete() {
		t.Error("a fresh item reported complete")
	}

	for _, step := range []string{"meta", "origin_cache"} {
		if err := s.MarkArchiveStep("civitai", "999", "4567", step); err != nil {
			t.Fatal(err)
		}
	}
	got, _ = s.ArchiveItemFor("civitai", "999", "4567")
	if !got.MetaOK || !got.OriginCacheOK {
		t.Errorf("marked steps did not stick: %+v", got)
	}
	if got.FileOK || got.PreviewsOK || got.Complete() {
		t.Errorf("unmarked steps reported done: %+v", got)
	}

	// An unknown step is a caller bug and must not silently succeed.
	if err := s.MarkArchiveStep("civitai", "999", "4567", "nonsense"); err == nil {
		t.Error("an unknown step was accepted")
	}
}

// Re-running an intake must not erase what an earlier run achieved -- that is
// the whole point of recording each step as it finishes.
func TestPutArchiveItemDoesNotResetProgress(t *testing.T) {
	s := openTemp(t)
	a := item("civitai", "999", "4567")
	if err := s.PutArchiveItem(a); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveStep("civitai", "999", "4567", "meta"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutArchiveItem(a); err != nil {
		t.Fatal(err)
	}

	got, _ := s.ArchiveItemFor("civitai", "999", "4567")
	if !got.MetaOK {
		t.Error("a re-run cleared a completed step; the run would redo work it had already done")
	}
}

// "3 of 12" and "0 of 0" are different situations that previews_ok alone cannot
// distinguish, and only the first is worth retrying.
func TestPreviewCountsDecideCompleteness(t *testing.T) {
	s := openTemp(t)
	if err := s.PutArchiveItem(item("civitai", "1", "1")); err != nil {
		t.Fatal(err)
	}

	if err := s.SetArchivePreviewCounts("civitai", "1", "1", 12, 3); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ArchiveItemFor("civitai", "1", "1")
	if got.PreviewsOK {
		t.Error("3 of 12 reported complete")
	}
	if got.PreviewsTotal != 12 || got.PreviewsGot != 3 {
		t.Errorf("counts = %d/%d", got.PreviewsGot, got.PreviewsTotal)
	}

	// A version with no images is complete, not pending forever.
	if err := s.SetArchivePreviewCounts("civitai", "1", "1", 0, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ArchiveItemFor("civitai", "1", "1"); !got.PreviewsOK {
		t.Error("a version with no previews was left incomplete")
	}

	if err := s.SetArchivePreviewCounts("civitai", "1", "1", 12, 12); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.ArchiveItemFor("civitai", "1", "1"); !got.PreviewsOK {
		t.Error("12 of 12 reported incomplete")
	}
}

// The takedown stamp records when it was FIRST seen gone. Re-confirming must not
// move it, or the one date worth keeping is overwritten by the most recent
// pointless re-check.
func TestVersionGoneIsRecordedOnce(t *testing.T) {
	s := openTemp(t)
	if err := s.PutArchiveItem(item("civitai", "999", "4567")); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkArchiveVersionGone("civitai", "999", "4567"); err != nil {
		t.Fatal(err)
	}
	first, _ := s.ArchiveItemFor("civitai", "999", "4567")
	if !first.Gone() {
		t.Fatal("the takedown was not recorded")
	}

	if err := s.MarkArchiveVersionGone("civitai", "999", "4567"); err != nil {
		t.Fatal(err)
	}
	again, _ := s.ArchiveItemFor("civitai", "999", "4567")
	if again.UpstreamGoneAt != first.UpstreamGoneAt {
		t.Errorf("the stamp moved from %q to %q on re-confirmation",
			first.UpstreamGoneAt, again.UpstreamGoneAt)
	}
}

// The "what did I save that no longer exists" view is the archive's reason to
// exist, so it is a filter rather than a query the user has to know how to write.
func TestArchiveItemsFilters(t *testing.T) {
	s := openTemp(t)
	for _, a := range []ArchiveItem{
		item("civitai", "1", "1"), item("civitai", "2", "2"), item("civitai", "3", "3"),
	} {
		if err := s.PutArchiveItem(a); err != nil {
			t.Fatal(err)
		}
	}
	// One complete, one gone, one plain incomplete.
	for _, step := range []string{"file", "meta", "origin_cache", "previews"} {
		if err := s.MarkArchiveStep("civitai", "1", "1", step); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkArchiveVersionGone("civitai", "2", "2"); err != nil {
		t.Fatal(err)
	}

	all, err := s.ArchiveItems(ArchiveItemsQuery{})
	if err != nil || len(all) != 3 {
		t.Fatalf("all = %d items (%v)", len(all), err)
	}
	incomplete, _ := s.ArchiveItems(ArchiveItemsQuery{Incomplete: true})
	if len(incomplete) != 2 {
		t.Errorf("incomplete = %d, want 2", len(incomplete))
	}
	gone, _ := s.ArchiveItems(ArchiveItemsQuery{Gone: true})
	if len(gone) != 1 || gone[0].ModelID != "2" {
		t.Errorf("gone = %+v, want only model 2", gone)
	}

	counts, err := s.ArchiveSummary()
	if err != nil {
		t.Fatal(err)
	}
	if counts.Items != 3 || counts.Complete != 1 || counts.Incomplete != 2 || counts.Gone != 1 {
		t.Errorf("summary = %+v", counts)
	}
}

// An empty archive must summarize as zeros rather than erroring: SQLite returns
// NULL for a SUM over no rows, which is the shape that would otherwise fail on
// a fresh install.
func TestArchiveSummaryOnAnEmptyArchive(t *testing.T) {
	s := openTemp(t)
	counts, err := s.ArchiveSummary()
	if err != nil {
		t.Fatalf("summarizing an empty archive failed: %v", err)
	}
	if counts.Items != 0 || counts.Complete != 0 || counts.Gone != 0 || counts.Watched != 0 {
		t.Errorf("summary = %+v, want zeros", counts)
	}
}

// Staged previews exist so a model taken down before it could be fetched still
// has its images -- they cannot wait for a model_file row that may never arrive.
func TestArchivePreviewsStageWithoutAModelFile(t *testing.T) {
	s := openTemp(t)
	p := ArchivePreview{
		Provider: "civitai", ModelID: "1", VersionID: "1",
		ImageSHA256: "beef", SourceURL: "https://example.test/a.png", Position: 0,
	}
	if err := s.PutArchivePreview(p); err != nil {
		t.Fatalf("staging a preview with no model_file row failed: %v", err)
	}

	// Re-fetching the same URL updates rather than duplicating.
	p.Position = 3
	if err := s.PutArchivePreview(p); err != nil {
		t.Fatal(err)
	}
	got, err := s.ArchivePreviews("civitai", "1", "1")
	if err != nil || len(got) != 1 {
		t.Fatalf("previews = %+v (%v)", got, err)
	}
	if got[0].Position != 3 {
		t.Errorf("position = %d, want the updated value", got[0].Position)
	}

	urls, err := s.ArchivedPreviewURLs("civitai", "1", "1")
	if err != nil {
		t.Fatal(err)
	}
	if urls["https://example.test/a.png"] != "beef" {
		t.Errorf("url index = %v; a re-run could not skip what it already has", urls)
	}
}

// Least-recently-checked first is what makes a sweep resumable across ticks.
func TestArchiveWatchesAreOrderedForResumability(t *testing.T) {
	s := openTemp(t)
	for _, id := range []string{"a", "b", "c"} {
		if err := s.PutArchiveWatch(ArchiveWatch{Provider: "civitai", ModelID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkArchiveWatchChecked("civitai", "a"); err != nil {
		t.Fatal(err)
	}

	got, err := s.ArchiveWatches(0)
	if err != nil || len(got) != 3 {
		t.Fatalf("watches = %+v (%v)", got, err)
	}
	if got[len(got)-1].ModelID != "a" {
		t.Errorf("the checked model is not last: %+v", got)
	}

	// auto_pull is off unless asked for: a watch subscribes to information, not
	// to unattended multi-gigabyte downloads.
	if got[0].AutoPull {
		t.Error("auto_pull defaulted on")
	}
	if err := s.PutArchiveWatch(ArchiveWatch{Provider: "civitai", ModelID: "b", AutoPull: true}); err != nil {
		t.Fatal(err)
	}
	watches, _ := s.ArchiveWatches(0)
	for _, w := range watches {
		if w.ModelID == "b" && !w.AutoPull {
			t.Error("auto_pull did not stick")
		}
	}

	if err := s.RemoveArchiveWatch("civitai", "a"); err != nil {
		t.Fatal(err)
	}
	if watches, _ = s.ArchiveWatches(0); len(watches) != 2 {
		t.Errorf("watches after removal = %d, want 2", len(watches))
	}
}
