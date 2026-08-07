package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/download"
	"github.com/socrasteeze/model-manager/internal/evict"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/testutil"
)

func testTempRoot(t *testing.T) string { t.Helper(); return testutil.TempDir(t) }

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

func repeat64(c byte) string { return strings.Repeat(string(c), 64) }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// evictRig is a client that has already pulled a model, which is the only state
// from which eviction is offered at all.
type evictRig struct {
	*pullRig
	job download.Job
}

func newEvictRig(t *testing.T, mutate func(*Config)) *evictRig {
	t.Helper()
	r := newPullRig(t)
	// AllowEvict is set after the pull because the flag has nothing to do with
	// fetching; flipping it here keeps the rig honest about which half each test
	// is exercising.
	r.client.cfg.AllowEvict = true
	if mutate != nil {
		mutate(&r.client.cfg)
	}
	job := r.pull(t)
	if job.State != download.StateComplete {
		t.Fatalf("setup pull failed: %s %s", job.State, job.Error)
	}
	return &evictRig{pullRig: r, job: job}
}

func (r *evictRig) evict(t *testing.T, body string) (int, string) {
	t.Helper()
	w := do(r.client, "POST", "http://localhost/api/models/"+r.nas.sha+"/evict", body, nil)
	return w.Code, w.Body.String()
}

// The happy path, and the shape of what survives. Everything the library knows
// about the model must still be there; only the claim that the bytes are on this
// disk goes away.
func TestEvictRemovesFileAndKeepsEverythingElse(t *testing.T) {
	r := newEvictRig(t, nil)

	before := r.detail(t)
	if before.Record == nil || before.Record.Name == "" || len(before.Previews) == 0 {
		t.Fatal("setup did not produce a model worth checking survives")
	}

	code, body := r.evict(t, "")
	if code != http.StatusOK {
		t.Fatalf("evict returned %d: %s", code, body)
	}
	var resp evictResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "evicted" {
		t.Errorf("status = %q", resp.Status)
	}
	if resp.FreedBytes != int64(len(r.nas.body)) {
		t.Errorf("freed = %d, want %d", resp.FreedBytes, len(r.nas.body))
	}

	if _, err := os.Stat(r.job.FinalPath); !os.IsNotExist(err) {
		t.Errorf("the file is still on disk: %v", err)
	}

	after := r.detail(t)
	// The path row is kept and marked absent, exactly as RemoveRoot does -- not
	// deleted, because the model is still known, just not resident.
	if len(after.Paths) != len(before.Paths) {
		t.Errorf("path rows = %d, want %d kept", len(after.Paths), len(before.Paths))
	}
	for _, p := range after.Paths {
		if p.Present {
			t.Errorf("path %s still marked present", p.Path)
		}
	}
	// And every hash-keyed table is untouched. Deleting the model_file row would
	// ON DELETE CASCADE all of this away, which is why eviction never reaches
	// for it.
	if after.Record == nil || after.Record.Name != before.Record.Name {
		t.Error("the resolved record did not survive eviction")
	}
	if len(after.Previews) != len(before.Previews) {
		t.Error("previews did not survive eviction")
	}
	if len(after.Tags) != len(before.Tags) {
		t.Error("tags did not survive eviction")
	}
	if len(after.Origins) != len(before.Origins) {
		t.Error("origin identity did not survive eviction")
	}
	if len(r.candidates(t)) == 0 {
		t.Error("provenance did not survive eviction")
	}

	// Still listed as available from the upstream, with the eviction recorded.
	if len(after.Pulled) != 1 || after.Pulled[0].Resident() {
		t.Errorf("pulled = %+v, want one non-resident record", after.Pulled)
	}
}

// The load-bearing guard. A file nothing recorded fetching cannot be proven
// re-fetchable, so it is not a cache entry and will not be deleted.
func TestEvictRefusesAFileThatWasNotPulled(t *testing.T) {
	root := testTempRoot(t)
	f := serveFilesServer(t, func(c *Config) { c.AllowEvict = true })
	_ = root

	code := do(f.srv, "POST", "http://localhost/api/models/"+f.sha+"/evict", "", nil).Code
	if code != http.StatusConflict {
		t.Fatalf("evict returned %d, want 409 for a file with no recorded upstream", code)
	}
	if _, err := os.Stat(f.path); err != nil {
		t.Fatalf("the file was removed anyway: %v", err)
	}
}

// A new permission, spelled as one. Without --allow-evict a writable daemon
// still refuses, so the flag cannot be acquired by accident.
func TestEvictRefusedWithoutTheFlag(t *testing.T) {
	r := newEvictRig(t, func(c *Config) { c.AllowEvict = false })

	code, body := r.evict(t, "")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("evict returned %d, want 503", code)
	}
	if !contains(body, "--allow-evict") {
		t.Errorf("the refusal should name the flag: %s", body)
	}
	if _, err := os.Stat(r.job.FinalPath); err != nil {
		t.Errorf("the file was removed despite the refusal: %v", err)
	}
}

func TestEvictRefusedInReadOnlyMode(t *testing.T) {
	r := newEvictRig(t, nil)
	r.client.cfg.ReadOnly = true

	code, _ := r.evict(t, "")
	if code != http.StatusForbidden {
		t.Fatalf("evict returned %d, want 403", code)
	}
	if _, err := os.Stat(r.job.FinalPath); err != nil {
		t.Errorf("the file was removed in read-only mode: %v", err)
	}
}

// The bytes on disk must still be the bytes the index describes, checked with
// the same four-tuple the incremental scan is keyed on.
func TestEvictRefusesWhenTheFileChangedSinceIndexing(t *testing.T) {
	r := newEvictRig(t, nil)

	if err := os.WriteFile(r.job.FinalPath, []byte("something else entirely"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := r.evict(t, "")
	if code != http.StatusConflict {
		t.Fatalf("evict returned %d, want 409", code)
	}
	if !contains(body, "changed") {
		t.Errorf("the refusal should say why: %s", body)
	}
	if _, err := os.Stat(r.job.FinalPath); err != nil {
		t.Errorf("a changed file was deleted anyway: %v", err)
	}
}

// The quiet one. A hardlinked view holds the inode, so removing the model frees
// nothing while the response claims the whole file back.
func TestEvictRefusesWhenAViewLinksToTheModel(t *testing.T) {
	r := newEvictRig(t, nil)

	if _, err := r.clientS.DB().Exec(
		`INSERT INTO view (name, root, strategy, created_at)
         VALUES ('by-type', ?, 'hardlink', '2026-01-01T00:00:00Z')`,
		filepath.Join(r.root, "views")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.clientS.DB().Exec(
		`INSERT INTO view_entry (view_id, sha256, path, source_path, strategy, bytes, created_at)
         VALUES ((SELECT id FROM view WHERE name = 'by-type'), ?, ?, ?, 'hardlink', 1, '2026-01-01T00:00:00Z')`,
		r.nas.sha, filepath.Join(r.root, "views", "lora", "x.safetensors"), r.job.FinalPath); err != nil {
		t.Fatal(err)
	}

	code, body := r.evict(t, "")
	if code != http.StatusConflict {
		t.Fatalf("evict returned %d, want 409", code)
	}
	if !contains(body, "by-type") {
		t.Errorf("the refusal should name the view blocking it: %s", body)
	}
	if _, err := os.Stat(r.job.FinalPath); err != nil {
		t.Errorf("the file was removed while a hardlinked view held it: %v", err)
	}
}

// A tier-staged copy is a second present path on the same hash, so "which file"
// is a real question and the daemon must not answer it by guessing.
func TestEvictRefusesToGuessBetweenTwoCopies(t *testing.T) {
	r := newEvictRig(t, nil)

	second := filepath.Join(testTempRoot(t), "staged.safetensors")
	if err := os.WriteFile(second, r.nas.body, 0o644); err != nil {
		t.Fatal(err)
	}
	run, err := r.clientS.BeginScanRun(r.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.clientS.UpsertFileAndPath(
		store.ModelFile{SHA256: r.nas.sha, ProbeSHA256: "p", Size: int64(len(r.nas.body)), Format: "safetensors"},
		store.FilePath{SHA256: r.nas.sha, Path: second, Root: r.root, Device: 9, Inode: 9,
			Size: int64(len(r.nas.body)), MtimeNs: 1, Present: true, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}

	code, body := r.evict(t, "")
	if code != http.StatusConflict {
		t.Fatalf("evict returned %d, want 409 when two copies are present", code)
	}
	if !contains(body, "name the one") {
		t.Errorf("the refusal should ask which: %s", body)
	}

	// Naming the pulled one works; naming the other is refused, because it is
	// not the copy this daemon fetched and can fetch back.
	code, body = r.evict(t, `{"path":`+mustJSON(t, second)+`}`)
	if code != http.StatusConflict {
		t.Fatalf("evicting the un-pulled copy returned %d, want 409: %s", code, body)
	}
	if _, err := os.Stat(second); err != nil {
		t.Errorf("the un-pulled copy was deleted: %v", err)
	}

	code, body = r.evict(t, `{"path":`+mustJSON(t, r.job.FinalPath)+`}`)
	if code != http.StatusOK {
		t.Fatalf("evicting the pulled copy returned %d: %s", code, body)
	}
	if _, err := os.Stat(r.job.FinalPath); !os.IsNotExist(err) {
		t.Error("the named pulled copy was not removed")
	}
}

// The round trip that makes eviction safe to offer: what was freed can be got
// back, and the record flips resident again.
func TestRePullAfterEvictRestoresTheCopy(t *testing.T) {
	r := newEvictRig(t, nil)

	if code, body := r.evict(t, ""); code != http.StatusOK {
		t.Fatalf("evict returned %d: %s", code, body)
	}
	if copies, _ := r.clientS.PulledCopies(r.nas.sha); len(copies) != 1 || copies[0].Resident() {
		t.Fatalf("after eviction: %+v", copies)
	}

	job := r.pull(t)
	if job.State != download.StateComplete {
		t.Fatalf("re-pull failed: %s %s", job.State, job.Error)
	}
	if _, err := os.Stat(job.FinalPath); err != nil {
		t.Errorf("the re-pulled file is not on disk: %v", err)
	}

	copies, err := r.clientS.PulledCopies(r.nas.sha)
	if err != nil || len(copies) != 1 {
		t.Fatalf("pulled copies = %+v (%v)", copies, err)
	}
	if !copies[0].Resident() {
		t.Error("the re-pulled copy is still marked evicted")
	}
	// UpsertFileAndPath's ON CONFLICT must have flipped the path row back to
	// present, or the model would stay invisible in the library.
	d := r.detail(t)
	var present bool
	for _, p := range d.Paths {
		if p.Present {
			present = true
		}
	}
	if !present {
		t.Error("no path row is present after the re-pull")
	}
}

func TestPullsEndpointReportsReclaimableSpace(t *testing.T) {
	r := newEvictRig(t, nil)

	w := do(r.client, "GET", "http://localhost/api/pulls", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("pulls returned %d", w.Code)
	}
	var got struct {
		Pulls            []store.PulledCopy `json:"pulls"`
		ReclaimableBytes int64              `json:"reclaimable_bytes"`
		EvictAvailable   bool               `json:"evict_available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Pulls) != 1 {
		t.Fatalf("pulls = %+v", got.Pulls)
	}
	if got.ReclaimableBytes != int64(len(r.nas.body)) {
		t.Errorf("reclaimable = %d, want %d", got.ReclaimableBytes, len(r.nas.body))
	}
	if !got.EvictAvailable {
		t.Error("evict_available false on a daemon that can evict")
	}

	// Once evicted it stops counting toward reclaimable space, or the number
	// would promise room that is already free.
	if code, body := r.evict(t, ""); code != http.StatusOK {
		t.Fatalf("evict returned %d: %s", code, body)
	}
	w = do(r.client, "GET", "http://localhost/api/pulls", "", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Pulls) != 0 || got.ReclaimableBytes != 0 {
		t.Errorf("after eviction: pulls = %+v, reclaimable = %d", got.Pulls, got.ReclaimableBytes)
	}
}

// TestTwoRootsEvictIndependently is the bug the (sha256, upstream, path) key
// fixes, and it is the reason the key changed.
//
// Keyed on (sha256, upstream) alone, the second pull overwrote the first's row.
// The first file was then unreachable through eviction forever: its path no
// longer matched any recorded copy, so the guard refused it with "that path is
// not the copy this daemon pulled" -- about a file this daemon had, in fact,
// pulled. Undeletable, and the refusal was a lie.
func TestTwoRootsEvictIndependently(t *testing.T) {
	r := newEvictRig(t, nil)
	first := r.job.FinalPath

	// A second managed root, and the same model pulled into it.
	second := testTempRoot(t)
	if _, err := r.clientS.AddRoot(second, "", ""); err != nil {
		t.Fatal(err)
	}
	r.root = second
	secondJob := r.pull(t)
	if secondJob.State != download.StateComplete {
		t.Fatalf("second pull failed: %s %s", secondJob.State, secondJob.Error)
	}
	if pathsSame(secondJob.FinalPath, first) {
		t.Fatalf("both pulls landed at %s; the test proves nothing", first)
	}

	copies, err := r.clientS.PulledCopies(r.nas.sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 2 {
		t.Fatalf("pulled copies = %d, want one row per copy: %+v", len(copies), copies)
	}

	// Evict the first by name. The second must be untouched in every respect:
	// on disk, resident in the record, and still evictable itself.
	code, body := r.evict(t, `{"path":`+mustJSON(t, first)+`}`)
	if code != http.StatusOK {
		t.Fatalf("evicting the first copy returned %d: %s", code, body)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Error("the first copy is still on disk")
	}
	if _, err := os.Stat(secondJob.FinalPath); err != nil {
		t.Fatalf("the second copy was removed too: %v", err)
	}

	copies, _ = r.clientS.PulledCopies(r.nas.sha)
	var resident, evicted int
	for _, c := range copies {
		if c.Resident() {
			resident++
		} else {
			evicted++
		}
	}
	if resident != 1 || evicted != 1 {
		t.Fatalf("after one eviction: %d resident, %d evicted; want 1 and 1: %+v",
			resident, evicted, copies)
	}

	// The model is still present, because one copy remains -- so Browse must
	// still call it resident.
	d := r.detail(t)
	var present int
	for _, p := range d.Paths {
		if p.Present {
			present++
		}
	}
	if present != 1 {
		t.Errorf("present path rows = %d, want 1", present)
	}

	// And the survivor evicts on its own, which is the half that used to be
	// impossible.
	code, body = r.evict(t, `{"path":`+mustJSON(t, secondJob.FinalPath)+`}`)
	if code != http.StatusOK {
		t.Fatalf("evicting the second copy returned %d: %s", code, body)
	}
	if _, err := os.Stat(secondJob.FinalPath); !os.IsNotExist(err) {
		t.Error("the second copy is still on disk")
	}
	if d := r.detail(t); len(d.Pulled) != 2 {
		t.Errorf("pulled records = %d, want both kept", len(d.Pulled))
	}
}

// A re-pull to the SAME path is the same copy and must refresh it rather than
// growing a second row -- the distinction the key rests on.
func TestRePullToTheSamePathStaysOneRow(t *testing.T) {
	r := newEvictRig(t, nil)

	if code, body := r.evict(t, ""); code != http.StatusOK {
		t.Fatalf("evict returned %d: %s", code, body)
	}
	if job := r.pull(t); job.State != download.StateComplete {
		t.Fatalf("re-pull failed: %s", job.State)
	}

	copies, err := r.clientS.PulledCopies(r.nas.sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 {
		t.Fatalf("pulled copies = %d, want 1: %+v", len(copies), copies)
	}
	if !copies[0].Resident() {
		t.Error("the re-pulled copy is still marked evicted")
	}
}

func pathsSame(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// A live transfer for these bytes is a writer the stale-bytes and view guards
// do not cover. Deleting underneath it is the daemon fighting itself.
func TestEvictRefusesWhileADownloadHoldsTheHash(t *testing.T) {
	r := newEvictRig(t, nil)

	// An upstream that serves everything normally except /file, which it holds
	// open -- a transfer that has started and will not finish on its own, which
	// is what "in flight" means for a multi-gigabyte model.
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })

	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/file") {
			<-release
			return
		}
		r.nas.srv.ServeHTTP(w, req)
	}))
	defer gated.Close()
	r.client.cfg.Origin.UpstreamBase = gated.URL

	// A second root, so the in-flight pull is a legitimate request rather than a
	// duplicate of one that already landed.
	second := testTempRoot(t)
	if _, err := r.clientS.AddRoot(second, "", ""); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"url":       gated.URL + "/api/models/" + r.nas.sha + "/file",
		"dest_root": second,
		"filename":  "watercolour.safetensors",
		"sha256":    r.nas.sha,
		"type":      "lora",
	})
	w := do(r.client, "POST", "http://localhost/api/downloads", string(body), nil)
	if w.Code != http.StatusAccepted && w.Code != http.StatusConflict {
		t.Fatalf("starting the blocking pull returned %d: %s", w.Code, w.Body.String())
	}

	// Wait for the transfer to actually be live, rather than assuming it.
	deadline := time.Now().Add(5 * time.Second)
	for r.client.downloadHolding(r.nas.sha) == "" {
		if time.Now().After(deadline) {
			t.Fatal("the download never registered as in flight")
		}
		time.Sleep(10 * time.Millisecond)
	}

	code, msg := r.evict(t, "")
	if code != http.StatusConflict {
		t.Fatalf("evict returned %d, want 409 while a download holds the hash", code)
	}
	if !contains(msg, "download") {
		t.Errorf("the refusal should say what is holding it: %s", msg)
	}
	if _, err := os.Stat(r.job.FinalPath); err != nil {
		t.Errorf("the file was removed while a transfer held the hash: %v", err)
	}

	// And once nothing is transferring, the same request succeeds -- the guard
	// is a "not now", not a "never".
	once.Do(func() { close(release) })
	for r.client.downloadHolding(r.nas.sha) != "" {
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatal("the download never left flight")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, msg := r.evict(t, ""); code != http.StatusOK {
		t.Fatalf("evict returned %d once the transfer finished: %s", code, msg)
	}
}

// The guard sits after identity on purpose. A transfer in flight for a hash
// normally means that hash is NOT in model_file yet -- that is the ordinary
// state of a download, not an exception -- so asking first would answer "a
// download holds this" for a model that does not exist, where 404 is correct.
func TestInFlightGuardDoesNotPreemptTheUnknownModelAnswer(t *testing.T) {
	r := newEvictRig(t, nil)
	unknown := repeat64('e')

	_, err := evict.Do(r.clientS, evict.Request{
		SHA256: unknown,
		HeldBy: func(string) string { return "job-1" },
	})
	if !errors.Is(err, evict.ErrUnknownModel) {
		t.Fatalf("err = %v, want ErrUnknownModel; the in-flight guard must not run first", err)
	}

	// And with no hook at all -- the CLI's shape -- nothing changes.
	if _, err := evict.Do(r.clientS, evict.Request{SHA256: unknown}); !errors.Is(err, evict.ErrUnknownModel) {
		t.Fatalf("err = %v, want ErrUnknownModel with a nil hook", err)
	}
}

// An evicted model is still owned, so Browse keeps calling it "have" -- that is
// the pre-existing rule and it is right, since an unplugged drive should not
// prompt a re-download of the library. But it must stop calling it resident, or
// the card offers no way to get back a file the user deliberately removed.
//
// Built from a real store rather than a hand-made index, because the bug this
// pins was in the query: the display path is populated even for an absent row,
// so "has a path" is not the same question as "is it here", and an index
// assembled by hand in a unit test agreed with whichever answer it was given.
func TestBrowseReportsAnEvictedModelAsNotResident(t *testing.T) {
	r := newEvictRig(t, nil)
	sha := strings.ToUpper(r.nas.sha)
	listing := func() origin.Listing {
		return origin.Listing{
			Provider: origin.ProviderUpstreamID,
			ID:       r.nas.sha,
			Files:    []origin.RemoteFile{{SHA256: sha}},
		}
	}

	idx, err := origin.BuildLocalIndex(r.clientS)
	if err != nil {
		t.Fatal(err)
	}
	items := []origin.Listing{listing()}
	idx.Annotate(items)
	if got := items[0].Local; got == nil || got.Status != origin.MatchHave || !got.Resident {
		t.Fatalf("before eviction: %+v, want have and resident", got)
	}

	if code, body := r.evict(t, ""); code != http.StatusOK {
		t.Fatalf("evict returned %d: %s", code, body)
	}

	idx, err = origin.BuildLocalIndex(r.clientS)
	if err != nil {
		t.Fatal(err)
	}
	items = []origin.Listing{listing()}
	idx.Annotate(items)
	got := items[0].Local
	if got == nil || got.Status != origin.MatchHave {
		t.Fatalf("after eviction: %+v, want it still owned", got)
	}
	if got.Resident {
		t.Error("an evicted model still reports as resident; the card would offer no way to get it back")
	}
	if got.Path == "" {
		t.Error("the display path was dropped; the card can no longer say where it used to be")
	}
}

func TestEvictUnknownHash(t *testing.T) {
	r := newEvictRig(t, nil)
	w := do(r.client, "POST",
		"http://localhost/api/models/"+repeat64('f')+"/evict", "", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("evict returned %d, want 404", w.Code)
	}
}
