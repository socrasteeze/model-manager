package store

import (
	"testing"
	"time"
)

// seedUpstream records what a provider last said about a remote model.
func seedUpstream(t *testing.T, s *Store, modelID, latestVersionID, latestFileSHA string) {
	t.Helper()
	if err := s.PutOriginModelStatus(OriginModelStatus{
		Provider: "civitai", ModelID: modelID,
		LatestVersionID: latestVersionID, LatestVersionName: "v" + latestVersionID,
		LatestFileSHA256: latestFileSHA, HTTPStatus: 200,
	}); err != nil {
		t.Fatal(err)
	}
}

// seedOwned records that a local file was published as one version of a model.
func seedOwned(t *testing.T, s *Store, sha, modelID, versionID string) {
	t.Helper()
	if err := s.PutModelOrigin(ModelOrigin{
		SHA256: sha, Provider: "civitai",
		ModelID: modelID, VersionID: versionID, VersionName: "v" + versionID,
	}); err != nil {
		t.Fatal(err)
	}
}

func updateSHAs(t *testing.T, s *Store) []string {
	t.Helper()
	ups, err := s.PendingUpdates(0)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(ups))
	for _, u := range ups {
		out = append(out, u.SHA256)
	}
	return out
}

func TestPendingUpdateAppearsForAnOlderOwnedVersion(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedOwned(t, s, "aaa", "42", "100")
	seedUpstream(t, s, "42", "200", "")

	ups, err := s.PendingUpdates(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 {
		t.Fatalf("got %d pending updates, want 1", len(ups))
	}
	if ups[0].HaveVersionID != "100" || ups[0].LatestVersionID != "200" {
		t.Errorf("have/latest = %s/%s, want 100/200", ups[0].HaveVersionID, ups[0].LatestVersionID)
	}
	// The list has to be able to name which of your files this is about.
	if ups[0].Name != "A" || ups[0].LocalPath == "" {
		t.Errorf("update did not carry the local model's name/path: %+v", ups[0])
	}
}

// The staleness test. Downloading the update makes the badge wrong, and
// nothing invalidates a flag -- because there is no flag. Indexing the new
// file's bytes is enough on its own.
func TestUpdateClearsWhenTheNewFileIsIndexedByHash(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedOwned(t, s, "aaa", "42", "100")
	seedUpstream(t, s, "42", "200", "bbb")

	if got := updateSHAs(t, s); len(got) != 1 {
		t.Fatalf("expected one pending update before the download, got %v", got)
	}

	// The user downloads it; a scan indexes the new file. Nothing else runs --
	// no enrichment, no invalidation hook, no second sweep.
	seedSearch(t, s, seedSpec{sha: "bbb", path: "/models/b.safetensors", name: "B"})

	if got := updateSHAs(t, s); len(got) != 0 {
		t.Errorf("update survived the new file being indexed: %v", got)
	}
}

// The same clearing, via the other route: the new file gets enriched and
// records the version id, without its hash ever having been known upstream.
func TestUpdateClearsWhenTheNewVersionIsOwnedByVersionID(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedOwned(t, s, "aaa", "42", "100")
	seedUpstream(t, s, "42", "200", "")

	if got := updateSHAs(t, s); len(got) != 1 {
		t.Fatalf("expected one pending update, got %v", got)
	}

	seedSearch(t, s, seedSpec{sha: "bbb", path: "/models/b.safetensors", name: "B"})
	seedOwned(t, s, "bbb", "42", "200")

	if got := updateSHAs(t, s); len(got) != 0 {
		t.Errorf("update survived owning the latest version: %v", got)
	}
}

// Keeping an old version beside a current one must not badge the old one.
// Same rule annotate.go's match() applies when it compares only the newest
// owned version.
func TestOlderKeptVersionIsNotBadged(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedSearch(t, s, seedSpec{sha: "bbb", path: "/models/b.safetensors", name: "B"})
	seedOwned(t, s, "aaa", "42", "100") // the old copy, deliberately kept
	seedOwned(t, s, "bbb", "42", "200") // the current copy
	seedUpstream(t, s, "42", "300", "")

	got := updateSHAs(t, s)
	if len(got) != 1 || got[0] != "bbb" {
		t.Errorf("badged %v, want only the newest owned copy (bbb)", got)
	}
}

func TestModelNeverCheckedIsNotBadged(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedOwned(t, s, "aaa", "42", "100")
	// No status row at all.

	if got := updateSHAs(t, s); len(got) != 0 {
		t.Errorf("badged a model that was never checked: %v", got)
	}
}

// Providers report uppercase hex; model_file.sha256 is lower. If the write
// path did not normalise, exclusion #2 would never fire and the badge would
// never clear after a download -- silently, and permanently.
func TestUpstreamHashIsStoredLowercase(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedOwned(t, s, "aaa", "42", "100")
	seedUpstream(t, s, "42", "200", "BBB") // as a provider reports it

	if got := updateSHAs(t, s); len(got) != 1 {
		t.Fatalf("expected one pending update, got %v", got)
	}
	seedSearch(t, s, seedSpec{sha: "bbb", path: "/models/b.safetensors", name: "B"})
	if got := updateSHAs(t, s); len(got) != 0 {
		t.Error("an uppercase upstream hash never matched the indexed lowercase one")
	}
}

// A check that failed tells you nothing new about the newest version, so it
// must not retract a known answer. The same rule origin_cache applies when a
// later 404 must not erase an archived body.
func TestFailedCheckDoesNotClearAKnownLatest(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedOwned(t, s, "aaa", "42", "100")
	seedUpstream(t, s, "42", "200", "")

	if err := s.MarkOriginModelChecked("civitai", "42", 500, "boom"); err != nil {
		t.Fatal(err)
	}

	ups, err := s.PendingUpdates(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].LatestVersionID != "200" {
		t.Fatalf("a failed check retracted the known latest: %+v", ups)
	}
	// ...but it did record that we asked, so the sweep can move on.
	owned, err := s.OwnedOriginModels("civitai", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 1 || owned[0].CheckedAt == "" {
		t.Errorf("the failed check did not advance checked_at: %+v", owned)
	}
}

// Two providers naming one file must not produce two rows: Search LEFT JOINs
// this view, and a duplicate would multiply its result rows and break LIMIT.
func TestOneRowPerFileWithTwoOriginProviders(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})
	seedOwned(t, s, "aaa", "42", "100")
	if err := s.PutModelOrigin(ModelOrigin{
		SHA256: "aaa", Provider: "civarchive", ModelID: "42", VersionID: "100",
	}); err != nil {
		t.Fatal(err)
	}
	seedUpstream(t, s, "42", "200", "")
	if err := s.PutOriginModelStatus(OriginModelStatus{
		Provider: "civarchive", ModelID: "42", LatestVersionID: "200", HTTPStatus: 200,
	}); err != nil {
		t.Fatal(err)
	}

	if got := updateSHAs(t, s); len(got) != 1 {
		t.Errorf("got %d rows for one file known to two providers, want 1: %v", len(got), got)
	}
}

func TestOwnedOriginModelsOrdersLeastRecentlyCheckedFirst(t *testing.T) {
	s := searchStore(t)
	for _, sha := range []string{"aaa", "bbb", "ccc"} {
		seedSearch(t, s, seedSpec{sha: sha, path: "/models/" + sha + ".safetensors", name: sha})
	}
	seedOwned(t, s, "aaa", "1", "10")
	seedOwned(t, s, "bbb", "2", "20")
	seedOwned(t, s, "ccc", "3", "30")

	// Model 2 was checked; 1 and 3 never were.
	seedUpstream(t, s, "2", "21", "")

	owned, err := s.OwnedOriginModels("civitai", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 3 {
		t.Fatalf("got %d owned models, want 3", len(owned))
	}
	// Never-checked sorts before checked: it is the most stale a thing can be.
	if owned[len(owned)-1].ModelID != "2" {
		t.Errorf("order = %v, want the checked model last",
			[]string{owned[0].ModelID, owned[1].ModelID, owned[2].ModelID})
	}

	// maxAge skips the one just checked.
	fresh, err := s.OwnedOriginModels("civitai", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 2 {
		t.Errorf("maxAge returned %d models, want the 2 never checked", len(fresh))
	}
}

func TestModelOriginUpsertKeepsDirectEvidenceAndVersionNames(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A"})

	if err := s.PutModelOrigin(ModelOrigin{
		SHA256: "aaa", Provider: "civitai", ModelID: "42",
		VersionID: "100", VersionName: "v1.0", Source: OriginSourceDownload,
	}); err != nil {
		t.Fatal(err)
	}
	// A later archive pass that could not read a version name must not erase
	// the one we have, nor downgrade direct evidence to an inference.
	if err := s.PutModelOrigin(ModelOrigin{
		SHA256: "aaa", Provider: "civitai", ModelID: "42",
		VersionID: "100", VersionName: "", Source: OriginSourceArchive,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ModelOrigins("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d origin rows, want 1", len(got))
	}
	if got[0].VersionName != "v1.0" {
		t.Errorf("version name = %q, want it kept", got[0].VersionName)
	}
	if got[0].Source != OriginSourceDownload {
		t.Errorf("source = %q, want direct evidence kept", got[0].Source)
	}
}

func TestBaseModelChangeIsFlagged(t *testing.T) {
	s := searchStore(t)
	seedSearch(t, s, seedSpec{sha: "aaa", path: "/models/a.safetensors", name: "A", baseModel: "SD 1.5"})
	seedOwned(t, s, "aaa", "42", "100")
	if err := s.PutOriginModelStatus(OriginModelStatus{
		Provider: "civitai", ModelID: "42",
		LatestVersionID: "200", LatestBaseModel: "SDXL 1.0", HTTPStatus: 200,
	}); err != nil {
		t.Fatal(err)
	}

	ups, err := s.PendingUpdates(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || !ups[0].BaseModelChanged {
		t.Errorf("a base-model change was not flagged: %+v", ups)
	}
}
