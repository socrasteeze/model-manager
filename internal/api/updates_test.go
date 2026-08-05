package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/enrichjob"
	"github.com/socrasteeze/model-manager/internal/origin"
	"github.com/socrasteeze/model-manager/internal/store"
	"github.com/socrasteeze/model-manager/internal/updatejob"
)

// updateServer builds a writable server whose origin client points at a stub,
// so nothing here reaches the real Civitai.
func updateServer(t *testing.T, handler http.HandlerFunc) (*Server, *store.Store) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := origin.NewClient()
	client.MinInterval = 0
	client.CivitaiBase = srv.URL
	client.APIKey, client.HFToken = "", ""
	client.MaxRetries = 1

	st := testStore(t)
	s := New(Config{
		Store:    st,
		Version:  "test",
		Security: Security{},
		Origin:   client,
		Updates:  updatejob.New(st, func() *origin.Client { return client }),
	})
	return s, st
}

// seedPendingUpdate makes the seeded model "aaa" look one version behind.
func seedPendingUpdate(t *testing.T, st *store.Store) {
	t.Helper()
	if err := st.PutModelOrigin(store.ModelOrigin{
		SHA256: "aaa", Provider: "civitai", ModelID: "42",
		VersionID: "100", VersionName: "v1.0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutOriginModelStatus(store.OriginModelStatus{
		Provider: "civitai", ModelID: "42",
		LatestVersionID: "200", LatestVersionName: "v2.0", HTTPStatus: 200,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUpdatesListReadsStoredData(t *testing.T) {
	s, st := updateServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("GET /api/updates contacted the provider; it must read stored data")
	})
	seedPendingUpdate(t, st)

	w := do(s, "GET", "http://127.0.0.1/api/updates", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Updates   []store.PendingUpdate `json:"updates"`
		Available bool                  `json:"available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Updates) != 1 || body.Updates[0].LatestVersionName != "v2.0" {
		t.Fatalf("updates = %+v, want one at v2.0", body.Updates)
	}
	if !body.Available {
		t.Error("available = false on a writable daemon with an origin client")
	}
}

// The headline claim of persisting this: the answer is still there after a
// restart, with no second sweep.
func TestUpdatesSurviveARestart(t *testing.T) {
	s, st := updateServer(t, func(w http.ResponseWriter, r *http.Request) {})
	seedPendingUpdate(t, st)

	// A brand new Server over the same store, as a restart would produce.
	fresh := New(Config{Store: st, Version: "test", Security: Security{}})

	w := do(fresh, "GET", "http://127.0.0.1/api/updates", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Updates []store.PendingUpdate `json:"updates"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Updates) != 1 {
		t.Errorf("got %d updates after a restart, want 1", len(body.Updates))
	}
	_ = s
}

// A --no-remote daemon contacts nobody, but it can still show what a previous
// sweep recorded -- that is the entire point of persisting it. Deliberately
// asymmetric with enrichPrereq, which gates its GET too.
func TestUpdatesListReadableWithoutAnOriginClient(t *testing.T) {
	st := testStore(t)
	seedPendingUpdate(t, st)
	s := New(Config{Store: st, Version: "test", Security: Security{}})

	w := do(s, "GET", "http://127.0.0.1/api/updates", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var body struct {
		Updates   []store.PendingUpdate `json:"updates"`
		Available bool                  `json:"available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Updates) != 1 {
		t.Errorf("stored updates were hidden on a --no-remote daemon")
	}
	if body.Available {
		t.Error("available = true with no origin client; the button should be hidden")
	}
}

func TestStartUpdateSweepUnavailableWithoutAnOriginClient(t *testing.T) {
	st := testStore(t)
	s := New(Config{Store: st, Version: "test", Security: Security{}})

	if w := do(s, "POST", "http://127.0.0.1/api/updates", "", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", w.Code)
	}
}

func TestUpdateSweepRefusedWhenReadOnly(t *testing.T) {
	s, _ := updateServer(t, func(w http.ResponseWriter, r *http.Request) {})
	s.cfg.ReadOnly = true

	if w := do(s, "POST", "http://127.0.0.1/api/updates", "", nil); w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
}

// Both sweeps spend one shared throttle, and jobrun only enforces at-most-one
// within a Runner -- so the API layer has to keep them from overlapping.
func TestUpdateSweepRefusedWhileEnrichmentIsRunning(t *testing.T) {
	st := testStore(t)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(slow.Close)

	client := origin.NewClient()
	client.MinInterval = 0
	client.CivitaiBase = slow.URL
	client.APIKey, client.HFToken = "", ""
	client.MaxRetries = 1

	enrich := enrichjob.New(st, nil, func() *origin.Client { return client })
	s := New(Config{
		Store: st, Version: "test", Security: Security{},
		Origin: client, Enrich: enrich,
		Updates: updatejob.New(st, func() *origin.Client { return client }),
	})

	if _, err := enrich.Start("all", enrichjob.Options{SkipImages: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { enrich.Cancel("") })

	w := do(s, "POST", "http://127.0.0.1/api/updates", "", nil)
	if w.Code != http.StatusConflict {
		t.Errorf("status %d while an enrichment sweep is running, want 409: %s", w.Code, w.Body.String())
	}
}

func TestUpdateSweepRefusesASecondConcurrentRun(t *testing.T) {
	s, st := updateServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusNotFound)
	})
	seedPendingUpdate(t, st)

	if w := do(s, "POST", "http://127.0.0.1/api/updates", "", nil); w.Code != http.StatusAccepted {
		t.Fatalf("first sweep returned %d, want 202: %s", w.Code, w.Body.String())
	}
	w := do(s, "POST", "http://127.0.0.1/api/updates", "", nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("second concurrent sweep returned %d, want 409", w.Code)
	}
	// The refusal carries the running job, so a client that lost track of it
	// has something to poll rather than just being told no.
	var body struct {
		Job *updatejob.Job `json:"job"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Job == nil || body.Job.ID == "" {
		t.Error("the 409 did not carry the running job")
	}
	s.cfg.Updates.Cancel("")
}

func TestNeedsUpdateFilterExposedOnSearch(t *testing.T) {
	s, st := updateServer(t, func(w http.ResponseWriter, r *http.Request) {})
	seedPendingUpdate(t, st)

	w := do(s, "GET", "http://127.0.0.1/api/models?needs_update=true", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var res store.SearchResults
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Total != 1 || len(res.Hits) != 1 {
		t.Fatalf("needs_update=true matched %d, want 1", res.Total)
	}
	if !res.Hits[0].UpdateAvailable || res.Hits[0].LatestVersionName != "v2.0" {
		t.Errorf("the hit did not carry the badge fields: %+v", res.Hits[0])
	}

	// And the facet count agrees, with its own filter lifted.
	fw := do(s, "GET", "http://127.0.0.1/api/facets?needs_update=true", "", nil)
	var facets store.Facets
	if err := json.Unmarshal(fw.Body.Bytes(), &facets); err != nil {
		t.Fatal(err)
	}
	if facets.NeedsUpdate != 1 {
		t.Errorf("facet count = %d, want 1", facets.NeedsUpdate)
	}
}
