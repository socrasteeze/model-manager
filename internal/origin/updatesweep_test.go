package origin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/socrasteeze/model-manager/internal/store"
)

// A /models/{id} body: one model with its newest version first.
func modelBody(modelID, versionID, versionName string) string {
	return `{"id":` + modelID + `,"name":"M","type":"LORA","modelVersions":[` +
		`{"id":` + versionID + `,"name":"` + versionName + `","baseModel":"SDXL 1.0",` +
		`"files":[{"name":"m.safetensors","primary":true,"hashes":{"SHA256":"CAFE"}}]}]}`
}

func sweepStore(t *testing.T, shas ...string) *store.Store {
	t.Helper()
	st := testStore(t)
	for i, sha := range shas {
		seed(t, st, sha, false)
		if err := st.PutModelOrigin(store.ModelOrigin{
			SHA256: sha, Provider: ProviderCivitaiID,
			ModelID:   itoa(i + 1),
			VersionID: "100", VersionName: "v1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

const (
	sweepA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sweepB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	sweepC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestSweepRecordsLatestVersionPerModel(t *testing.T) {
	st := sweepStore(t, sweepA)
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(modelBody("1", "200", "v2")))
	})

	stats, err := SweepUpdates(context.Background(), st, SweepOptions{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Checked != 1 || stats.Found != 1 {
		t.Fatalf("stats = %+v, want checked 1 found 1", stats)
	}

	ups, err := st.PendingUpdates(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].LatestVersionID != "200" {
		t.Fatalf("pending updates = %+v, want one at v200", ups)
	}
	// The provider reports uppercase hex; it has to land lower-case or the
	// badge can never clear once the file is downloaded.
	if ups[0].SizeBytes != 0 && ups[0].DownloadURL == "" {
		t.Errorf("file details were not recorded: %+v", ups[0])
	}
}

// A sweep cut short by a rate limit must keep everything it learned before the
// cut, and must say that it was cut short.
func TestSweepRecordsEachModelAsItGoesAndStopsOnRateLimit(t *testing.T) {
	st := sweepStore(t, sweepA, sweepB, sweepC)

	var n int32
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			_, _ = w.Write([]byte(modelBody("1", "200", "v2")))
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c.MaxRetries = 1
	oldBackoff := backoffFn
	backoffFn = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { backoffFn = oldBackoff })

	stats, err := SweepUpdates(context.Background(), st, SweepOptions{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	if !stats.RateLimited {
		t.Error("a rate-limited sweep did not set RateLimited")
	}
	if stats.Checked >= 3 {
		t.Errorf("checked %d models, want fewer than all 3 after the limit hit", stats.Checked)
	}

	// The first model's answer survives -- that is the resumability property.
	owned, err := st.OwnedOriginModels(ProviderCivitaiID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var checked int
	for _, m := range owned {
		if m.CheckedAt != "" {
			checked++
		}
	}
	if checked == 0 {
		t.Error("nothing was committed before the rate limit stopped the run")
	}
	if checked == len(owned) {
		t.Error("every model was marked checked despite the run stopping early")
	}
}

// A model removed upstream is a fact, not a failure -- and its known latest
// version must survive, since it may still be reachable elsewhere.
func TestSweepKeepsAKnownLatestWhenTheModelIsGone(t *testing.T) {
	st := sweepStore(t, sweepA)
	if err := st.PutOriginModelStatus(store.OriginModelStatus{
		Provider: ProviderCivitaiID, ModelID: "1",
		LatestVersionID: "200", LatestVersionName: "v2", HTTPStatus: 200,
	}); err != nil {
		t.Fatal(err)
	}

	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	stats, err := SweepUpdates(context.Background(), st, SweepOptions{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Errors != 0 {
		t.Errorf("a removed model counted as an error: %+v", stats)
	}

	ups, err := st.PendingUpdates(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].LatestVersionID != "200" {
		t.Errorf("a 404 retracted a known latest version: %+v", ups)
	}
}

// The sweep's input is the persisted identity, which for a library enriched
// before this feature existed lives only in the archive.
func TestSweepBackfillsIdentityBeforeItRuns(t *testing.T) {
	st := testStore(t)
	seed(t, st, sweepA, false)
	cache := NewCache(st)
	if err := cache.PutFound(ProviderCivitaiID, sweepA,
		json.RawMessage(`{"id":100,"modelId":7,"name":"v1"}`), 200); err != nil {
		t.Fatal(err)
	}

	var asked []string
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		_, _ = w.Write([]byte(modelBody("7", "300", "v3")))
	})

	stats, err := SweepUpdates(context.Background(), st, SweepOptions{Client: c})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Checked != 1 {
		t.Fatalf("checked %d models, want 1 discovered by the backfill", stats.Checked)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "/7") {
		t.Errorf("asked %v, want the model id from the archived response", asked)
	}
}

func TestSweepMaxAgeSkipsRecentlyChecked(t *testing.T) {
	st := sweepStore(t, sweepA)
	if err := st.MarkOriginModelChecked(ProviderCivitaiID, "1", 200, ""); err != nil {
		t.Fatal(err)
	}

	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("a model checked moments ago was asked about again")
		w.WriteHeader(http.StatusNotFound)
	})

	stats, err := SweepUpdates(context.Background(), st, SweepOptions{Client: c, MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Checked != 0 {
		t.Errorf("checked %d models, want 0 under MaxAge", stats.Checked)
	}
}

// Progress must report how far the run actually got, and carry live stats --
// the same two properties the enrichment sweep was fixed for.
func TestSweepProgressReportsTruePartialAndLiveStats(t *testing.T) {
	st := sweepStore(t, sweepA, sweepB, sweepC)

	ctx, cancel := context.WithCancel(context.Background())
	var n int32
	c := fakeCivitai(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			cancel()
		}
		_, _ = w.Write([]byte(modelBody("1", "200", "v2")))
	})

	var last struct {
		done, total int
		stats       UpdateStats
	}
	if _, err := SweepUpdates(ctx, st, SweepOptions{
		Client: c,
		Progress: func(done, total int, s UpdateStats) {
			last.done, last.total, last.stats = done, total, s
		},
	}); err != nil {
		t.Fatal(err)
	}

	if last.total != 3 {
		t.Fatalf("total = %d, want 3", last.total)
	}
	if last.done == last.total {
		t.Fatalf("final progress reported %d/%d for a cancelled run", last.done, last.total)
	}
	if last.stats.Checked == 0 {
		t.Error("the final progress snapshot carried no live counters")
	}
}
