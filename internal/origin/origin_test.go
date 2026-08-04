package origin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/blobstore"
	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "master.db"), store.Options{AllowNetworkPath: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seed(t *testing.T, s *store.Store, sha string, provisional bool) {
	t.Helper()
	run, _ := s.BeginScanRun("/models")
	if err := s.UpsertFileAndPath(
		store.ModelFile{SHA256: sha, ProbeSHA256: "p" + sha, Size: 1000, Format: "safetensors"},
		store.FilePath{SHA256: sha, Path: "/models/" + sha + ".safetensors", Root: "/models",
			Device: 1, Inode: uint64(len(sha)) + uint64(sha[0]), Size: 1000, MtimeNs: 1,
			Provisional: provisional, ScanRunID: run},
	); err != nil {
		t.Fatal(err)
	}
}

const civitaiBody = `{
  "id": 12345, "modelId": 999, "name": "v2.0", "baseModel": "Pony Diffusion V6 XL",
  "description": "<p>A <b>styled</b> LoRA</p>",
  "trainedWords": ["ponystyle", "ponystyle"],
  "model": {"name": "Pony Style", "type": "LORA", "nsfw": false, "tags": ["style", "anime"]},
  "files": [{"name": "m.safetensors", "primary": true,
             "hashes": {"SHA256": "ABCDEF", "AutoV2": "1234567890"}}],
  "images": [{"url": "IMAGE_URL", "width": 512, "height": 768}]
}`

// --- extraction ----------------------------------------------------------------

func TestObservationsFromCivitai(t *testing.T) {
	obs, tags, hashes, images := ObservationsFromCivitai(json.RawMessage(civitaiBody), "")

	byField := map[string]any{}
	for _, o := range obs {
		byField[o.Field] = o.Value
	}

	if byField[provenance.FieldName] != "Pony Style" {
		t.Errorf("name = %v", byField[provenance.FieldName])
	}
	if byField[provenance.FieldVersion] != "v2.0" {
		t.Errorf("version = %v", byField[provenance.FieldVersion])
	}
	// Pony's official name contains "XL"; classifying it as SDXL would merge
	// three buckets that behave nothing alike.
	if byField[provenance.FieldBaseModel] != "Pony" {
		t.Errorf("base_model = %v, want Pony", byField[provenance.FieldBaseModel])
	}
	if byField[provenance.FieldType] != "lora" {
		t.Errorf("type = %v", byField[provenance.FieldType])
	}
	if byField[provenance.FieldDescription] != "A styled LoRA" {
		t.Errorf("description = %v, want the markup stripped", byField[provenance.FieldDescription])
	}
	if byField[provenance.FieldNSFW] != false {
		t.Errorf("nsfw = %v", byField[provenance.FieldNSFW])
	}

	triggers, _ := byField[provenance.FieldTriggerWords].([]string)
	if len(triggers) != 1 {
		t.Errorf("triggers = %v, want duplicates removed", triggers)
	}
	if len(tags) != 2 {
		t.Errorf("tags = %v", tags)
	}
	// Every hash type, not just SHA256: AutoV2 is how other tools refer to the
	// same file.
	if hashes["SHA256"] != "ABCDEF" || hashes["AutoV2"] != "1234567890" {
		t.Errorf("hashes = %v", hashes)
	}
	if len(images) != 1 {
		t.Errorf("images = %v", images)
	}
}

func TestObservationsFromHuggingFace(t *testing.T) {
	body := `{
      "id": "someone/My-Cool-LoRA",
      "pipeline_tag": "text-to-image",
      "tags": ["lora", "diffusers", "region:us", "license:apache-2.0", "base_model:stabilityai/sdxl-base"],
      "cardData": {"license": "apache-2.0", "instance_prompt": "mytrigger"}
    }`
	obs, tags := ObservationsFromHuggingFace(json.RawMessage(body))

	byField := map[string]any{}
	for _, o := range obs {
		byField[o.Field] = o.Value
	}
	if byField[provenance.FieldName] != "My-Cool-LoRA" {
		t.Errorf("name = %v", byField[provenance.FieldName])
	}
	if byField[provenance.FieldType] != "lora" {
		t.Errorf("type = %v", byField[provenance.FieldType])
	}
	if byField[provenance.FieldBaseModel] != "SDXL" {
		t.Errorf("base_model = %v", byField[provenance.FieldBaseModel])
	}

	// Machine labels would swamp a tag facet meant for human organization.
	for _, tag := range tags {
		if strings.Contains(tag, ":") {
			t.Errorf("machine tag %q survived filtering: %v", tag, tags)
		}
	}
}

func TestHFBaseModelAcceptsStringOrList(t *testing.T) {
	forms := []string{
		`{"id":"a/b","cardData":{"base_model":"stabilityai/sdxl-turbo"}}`,
		`{"id":"a/b","cardData":{"base_model":["stabilityai/sdxl-turbo"]}}`,
	}
	for _, body := range forms {
		obs, _ := ObservationsFromHuggingFace(json.RawMessage(body))
		found := false
		for _, o := range obs {
			if o.Field == provenance.FieldBaseModel && o.Value == "SDXL" {
				found = true
			}
		}
		if !found {
			t.Errorf("base_model not extracted from %s", body)
		}
	}
}

// --- cache ---------------------------------------------------------------------

// The archive property: once a model is gone from Civitai, this copy is the only
// one left, so a later 404 must never overwrite it.
func TestPositiveArchiveSurvivesALaterMiss(t *testing.T) {
	st := testStore(t)
	cache := NewCache(st)

	if err := cache.PutFound(ProviderCivitai, "aaa", json.RawMessage(civitaiBody), 200); err != nil {
		t.Fatal(err)
	}
	// The model is taken down; the next run gets a 404.
	if err := cache.PutMissing(ProviderCivitai, "aaa", 404); err != nil {
		t.Fatal(err)
	}

	entry, ok, err := cache.Get(ProviderCivitai, "aaa")
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if !entry.Found {
		t.Fatal("a 404 erased the archived response; that copy may be the only one left")
	}
	if !strings.Contains(string(entry.Raw), "Pony Style") {
		t.Fatal("the archived body was replaced")
	}
}

// Without negative caching every run re-queries thousands of known misses, which
// is most of a self-trained library.
func TestNegativeCacheIsHonoredThenExpires(t *testing.T) {
	st := testStore(t)
	cache := NewCache(st)

	if err := cache.PutMissing(ProviderCivitai, "bbb", 404); err != nil {
		t.Fatal(err)
	}
	entry, ok, err := cache.Get(ProviderCivitai, "bbb")
	if err != nil || !ok || entry.Found {
		t.Fatalf("fresh negative not returned: %v %v", ok, err)
	}

	// Force expiry.
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(
		`UPDATE origin_cache SET expires_at = ? WHERE lookup_key = 'bbb'`, past); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := cache.Get(ProviderCivitai, "bbb"); ok {
		t.Fatal("an expired negative was still returned; a model uploaded later would never be found")
	}
}

func TestCacheStats(t *testing.T) {
	st := testStore(t)
	cache := NewCache(st)
	_ = cache.PutFound(ProviderCivitai, "a", json.RawMessage(`{"x":1}`), 200)
	_ = cache.PutMissing(ProviderCivitai, "b", 404)

	stats, err := cache.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.Positive != 1 || stats.Negative != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Bytes == 0 {
		t.Error("archive byte count is zero despite a stored body")
	}
}

// --- client --------------------------------------------------------------------

func fakeCivitai(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	old := CivitaiBaseURL
	CivitaiBaseURL = srv.URL
	t.Cleanup(func() { CivitaiBaseURL = old })

	c := NewClient()
	c.MinInterval = 0 // no throttling in tests
	return c
}

func TestLookupByHashFound(t *testing.T) {
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		// The hash that identifies the file locally is the same key Civitai
		// indexes by, so the URL should carry it directly.
		if !strings.Contains(r.URL.Path, "/model-versions/by-hash/") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(civitaiBody))
	})

	raw, status, err := c.LookupCivitaiByHash(context.Background(), "abc123")
	if err != nil || status != 200 || raw == nil {
		t.Fatalf("lookup = %v %d %v", raw != nil, status, err)
	}
}

// A 404 is a definite answer, not a failure.
func TestLookupByHashNotFound(t *testing.T) {
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	raw, status, err := c.LookupCivitaiByHash(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("a 404 was reported as an error: %v", err)
	}
	if raw != nil || status != 404 {
		t.Fatalf("raw = %v status = %d", raw, status)
	}
}

func TestRetriesOnServerErrorThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(civitaiBody))
	})
	c.MaxRetries = 4
	// Keep the test fast; the backoff curve itself is unit-tested separately.
	oldBackoff := backoffFn
	backoffFn = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoffFn = oldBackoff })

	raw, _, err := c.LookupCivitaiByHash(context.Background(), "abc")
	if err != nil {
		t.Fatalf("gave up despite a retryable error: %v", err)
	}
	if raw == nil {
		t.Fatal("no body after a successful retry")
	}
	if calls.Load() != 3 {
		t.Fatalf("made %d calls, want 3", calls.Load())
	}
}

// The server knows better than any backoff curve we would invent.
func TestRetryAfterIsHonored(t *testing.T) {
	cases := map[string]time.Duration{
		"5":            5 * time.Second,
		"0":            0,
		"":             0,
		"600":          300 * time.Second, // clamped
		"not-a-number": 0,
	}
	for header, want := range cases {
		if got := retryAfter(header); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", header, got, want)
		}
	}
}

func TestRateLimitGivesUpWithATypedError(t *testing.T) {
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.MaxRetries = 1
	oldBackoff := backoffFn
	backoffFn = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoffFn = oldBackoff })

	_, _, err := c.LookupCivitaiByHash(context.Background(), "abc")
	if !isRateLimit(err) {
		t.Fatalf("err = %v, want a rate-limit error the runner can act on", err)
	}
}

func TestContextCancellationStopsTheClient(t *testing.T) {
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(civitaiBody))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := c.LookupCivitaiByHash(ctx, "abc"); err == nil {
		t.Fatal("a cancelled context still performed the request")
	}
}

// --- enrichment ----------------------------------------------------------------

func TestEnrichMergesAndArchives(t *testing.T) {
	st := testStore(t)
	seed(t, st, "aaa", false)

	imageBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a,
		0, 0, 0, 0x0d, 'I', 'H', 'D', 'R', 0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0}

	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/image.png") {
			_, _ = w.Write(imageBytes)
			return
		}
		_, _ = w.Write([]byte(strings.ReplaceAll(civitaiBody, "IMAGE_URL", srv.URL+"/image.png")))
	}))
	t.Cleanup(srv.Close)

	old := CivitaiBaseURL
	CivitaiBaseURL = srv.URL
	t.Cleanup(func() { CivitaiBaseURL = old })

	blobs, err := blobstore.New(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient()
	client.MinInterval = 0

	stats, err := Enrich(context.Background(), st, EnrichOptions{Client: client, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Found != 1 || stats.Fetched != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if stats.Images != 1 {
		t.Errorf("images = %d, want 1", stats.Images)
	}

	rec, err := st.GetModelRecord("aaa")
	if err != nil || rec == nil {
		t.Fatalf("record = %v %v", rec, err)
	}
	if rec.Name != "Pony Style" || rec.BaseModel != "Pony" {
		t.Fatalf("record = %+v", rec)
	}

	// A second run must not re-query: the archive is the point.
	stats2, err := Enrich(context.Background(), st, EnrichOptions{Client: client, Blobs: blobs})
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Fetched != 0 || stats2.CacheHits != 1 {
		t.Fatalf("second run re-fetched: %+v", stats2)
	}
}

// A provisional path was bound by sampled probe rather than a full read.
// Querying an origin with a hash we are not sure of would archive someone else's
// metadata under this file.
func TestProvisionalModelsAreNotEnriched(t *testing.T) {
	st := testStore(t)
	seed(t, st, "prov", true)

	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a provisional model was looked up")
		w.WriteHeader(http.StatusNotFound)
	})

	stats, err := Enrich(context.Background(), st, EnrichOptions{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Considered != 0 {
		t.Fatalf("considered %d provisional models, want 0", stats.Considered)
	}
}

func TestEnrichRecordsMissesWithoutFailing(t *testing.T) {
	st := testStore(t)
	seed(t, st, "ccc", false)

	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	stats, err := Enrich(context.Background(), st, EnrichOptions{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Missing != 1 || stats.Errors != 0 {
		t.Fatalf("stats = %+v; a model not on Civitai is a fact, not a failure", stats)
	}

	// The miss is cached so the next run does not ask again.
	stats2, _ := Enrich(context.Background(), st, EnrichOptions{Client: c})
	if stats2.Fetched != 0 {
		t.Fatalf("re-queried a known miss: %+v", stats2)
	}
}

// Origin outranks tool scrapes but must still lose to a manual value.
func TestEnrichmentDoesNotOverrideManual(t *testing.T) {
	st := testStore(t)
	seed(t, st, "aaa", false)

	if err := st.RecordObservations("aaa", provenance.SourceManual,
		[]store.FieldObservation{{Field: provenance.FieldName, Value: "My Name"}}); err != nil {
		t.Fatal(err)
	}

	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(civitaiBody))
	})
	if _, err := Enrich(context.Background(), st, EnrichOptions{Client: c}); err != nil {
		t.Fatal(err)
	}

	rec, _ := st.GetModelRecord("aaa")
	if rec.Name != "My Name" {
		t.Fatalf("name = %q; enrichment overwrote a manual value", rec.Name)
	}
	// And the disagreement is surfaced rather than lost.
	pending, _ := st.PendingSuggestions("aaa", 0)
	if len(pending) == 0 {
		t.Fatal("no suggestion raised for the origin/manual disagreement")
	}
}

func TestEnrichLimitBoundsARun(t *testing.T) {
	st := testStore(t)
	for _, sha := range []string{"a1", "b2", "c3"} {
		seed(t, st, sha, false)
	}
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	stats, err := Enrich(context.Background(), st, EnrichOptions{Client: c, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Considered != 2 {
		t.Fatalf("considered %d, want the limit of 2", stats.Considered)
	}
}

// A poller reading progress mid-run (or after a stop) has to be told the truth
// about how far the run actually got, not a number that always equals the
// full eligible set regardless of when the run stopped.
func TestEnrichReportsTruePartialProgressOnCancellation(t *testing.T) {
	st := testStore(t)
	for _, sha := range []string{"a1", "b2", "c3"} {
		seed(t, st, sha, false)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var requests int32
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			// Cancel once the first model's lookup has completed, so the next
			// iteration's ctx.Err() check breaks the loop before it starts a
			// second one.
			cancel()
		}
		w.WriteHeader(http.StatusNotFound)
	})

	var calls []struct{ done, total int }
	stats, err := Enrich(ctx, st, EnrichOptions{
		Client: c,
		Progress: func(done, total int, _ EnrichStats) {
			calls = append(calls, struct{ done, total int }{done, total})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Considered != 3 {
		t.Fatalf("considered = %d, want 3 -- cancellation must not change what was eligible", stats.Considered)
	}
	if requests != 1 {
		t.Fatalf("the provider was asked about %d models, want exactly 1 before cancellation stopped the loop", requests)
	}
	if len(calls) == 0 {
		t.Fatal("Progress was never called")
	}

	last := calls[len(calls)-1]
	if last.done == last.total {
		t.Fatalf("final progress reported %d/%d (100%%) for a run that only reached 1 of 3 models", last.done, last.total)
	}
	if last.done != 1 || last.total != 3 {
		t.Fatalf("final progress = %d/%d, want 1/3", last.done, last.total)
	}
}

// A rate-limited stop is not a failure and not a cancellation, so it returns a
// nil error with a normal-looking context -- the only place the truth survives
// is EnrichStats.RateLimited, and it has to reach a live poller through the
// Progress snapshot, not only the value Enrich eventually returns.
func TestEnrichSetsRateLimitedAndReportsWhatWasAttempted(t *testing.T) {
	st := testStore(t)
	for _, sha := range []string{"a1", "b2"} {
		seed(t, st, sha, false)
	}
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.MaxRetries = 1
	oldBackoff := backoffFn
	backoffFn = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoffFn = oldBackoff })

	var last struct {
		done, total int
		stats       EnrichStats
	}
	stats, err := Enrich(context.Background(), st, EnrichOptions{
		Client: c,
		Progress: func(done, total int, s EnrichStats) {
			last.done, last.total, last.stats = done, total, s
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.RateLimited {
		t.Fatal("stats.RateLimited is false after the provider rate-limited the run")
	}
	if stats.Errors != 1 {
		t.Fatalf("errors = %d, want exactly 1 (the model that hit the rate limit)", stats.Errors)
	}
	if stats.Considered != 2 {
		t.Fatalf("considered = %d, want 2 -- Considered is eligibility, not attempts, and must not shrink", stats.Considered)
	}
	if last.done != 1 || last.total != 2 {
		t.Fatalf("final progress = %d/%d, want 1/2 -- the second model was never reached", last.done, last.total)
	}
	if !last.stats.RateLimited {
		t.Error("the final Progress call's stats snapshot does not carry RateLimited -- a live poller would never see it")
	}
}

// The single-model refresh path (Targets with one hash, Limit unset) must not
// pay for a full-library scan to answer a question about one row.
func TestEnrichTargetsByHashPathMatchesTheFullScan(t *testing.T) {
	st := testStore(t)
	seed(t, st, "aaa", false)
	seed(t, st, "bbb", false)
	seed(t, st, "prov", true) // provisional: never eligible either way

	// Below maxTargetParams: takes the IN(...) fast path.
	out, err := enrichTargets(st, 0, []string{"aaa"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0] != "aaa" {
		t.Fatalf("enrichTargets(want=[aaa]) = %v, want [aaa]", out)
	}

	// A provisional hash must be excluded even when explicitly named -- the
	// eligibility rule is not something a caller-supplied Targets list can
	// bypass.
	out, err = enrichTargets(st, 0, []string{"prov"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("enrichTargets(want=[prov]) = %v, want none: a provisional hash must stay excluded", out)
	}

	// Naming two hashes returns both, still ordered by size descending, and a
	// hash not in the library is silently absent rather than an error.
	out, err = enrichTargets(st, 0, []string{"aaa", "bbb", "does-not-exist"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("enrichTargets(want=[aaa,bbb,x]) = %v, want 2 matches", out)
	}

	// The untargeted full-scan path (want empty) still sees everything eligible.
	out, err = enrichTargets(st, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("enrichTargets(want=nil) = %v, want both eligible models", out)
	}
}
