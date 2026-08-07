package origin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstreamServer stands in for a NAS running model-manager, answering
// /api/models the way the real handler does.
func upstreamServer(t *testing.T, hits string, seen *http.Request) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = *r
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(hits))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const oneHit = `{"hits":[{
    "sha256":"AB12CD34",
    "name":"Watercolor",
    "type":"lora",
    "base_model":"SDXL",
    "version":"v3",
    "format":"safetensors",
    "size":151234567,
    "trigger_words":["watercolor style"],
    "tags":["style"],
    "preview_image":"deadbeef",
    "filename":"watercolor_v3.safetensors",
    "path":"/mnt/tank/models/loras/watercolor_v3.safetensors",
    "present":true,
    "path_count":1
}],"total":1,"limit":50,"offset":0}`

// TestUpstreamListingDropsUpstreamPaths is the privacy invariant for the whole
// feature: the upstream's own filesystem layout is meaningless on this machine
// and is nobody else's business. The adapter never decodes `path`, so this
// pins the absence rather than merely the non-display.
func TestUpstreamListingDropsUpstreamPaths(t *testing.T) {
	srv := upstreamServer(t, oneHit, nil)
	c := testClient()
	c.UpstreamBase = srv.URL

	page, err := (&UpstreamProvider{Client: c}).Search(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("got %d listings, want 1", len(page.Items))
	}

	encoded, err := json.Marshal(page.Items[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"/mnt/tank", "loras/watercolor_v3.safetensors"} {
		if strings.Contains(string(encoded), leak) {
			t.Errorf("upstream path leaked into the listing: %s", encoded)
		}
	}
	// The bare filename is kept on purpose -- a pull has to name the file it
	// writes -- and is not a leak, since it carries no directory.
	if page.Items[0].Files[0].Name != "watercolor_v3.safetensors" {
		t.Errorf("filename dropped: %+v", page.Items[0].Files[0])
	}
}

// The hash is the whole contract: it is the listing id, the upper-cased value
// Annotate compares against local content, and the path parameter of the
// download URL the transfer will verify against. If those three ever disagree,
// a pull verifies one file against another file's hash.
func TestUpstreamListingCarriesItsOwnHash(t *testing.T) {
	srv := upstreamServer(t, oneHit, nil)
	c := testClient()
	c.UpstreamBase = srv.URL
	c.UpstreamToken = "secret"

	page, err := (&UpstreamProvider{Client: c}).Search(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	l := page.Items[0]

	if l.Provider != ProviderUpstreamID {
		t.Errorf("provider = %q, want %q", l.Provider, ProviderUpstreamID)
	}
	if l.ID != "ab12cd34" {
		t.Errorf("id = %q, want the lower-cased hash", l.ID)
	}
	if len(l.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(l.Files))
	}
	f := l.Files[0]
	if f.SHA256 != "AB12CD34" {
		t.Errorf("file hash = %q, want it upper-cased for Annotate", f.SHA256)
	}
	if want := srv.URL + "/api/models/ab12cd34/file"; f.DownloadURL != want {
		t.Errorf("download url = %q, want %q", f.DownloadURL, want)
	}
	if !strings.HasSuffix(f.DownloadURL, "/"+strings.ToLower(f.SHA256)+"/file") {
		t.Error("the download URL and the verified hash must be the same value")
	}
	if !f.Primary {
		t.Error("the only file must be the primary one, or PrimaryFile has nothing to pick")
	}
	if !f.RequiresAuth {
		t.Error("a token is configured, so the UI should warn before a 401 rather than after")
	}
	if l.ThumbnailURL != srv.URL+"/api/previews/deadbeef" {
		t.Errorf("thumbnail url = %q", l.ThumbnailURL)
	}
	// safetensors must survive as a safe format, so PrimaryFile's preference and
	// the UI's pickle warning both keep working against an upstream.
	if !isSafeFormat(f) {
		t.Errorf("format %q not recognised as safe", f.Format)
	}
}

// Annotate already matches by content hash, so a model held locally must report
// as owned with no upstream-specific code anywhere in the annotator.
func TestUpstreamListingAnnotatesAgainstLocalHashes(t *testing.T) {
	srv := upstreamServer(t, oneHit, nil)
	c := testClient()
	c.UpstreamBase = srv.URL

	page, err := (&UpstreamProvider{Client: c}).Search(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}

	idx := &LocalIndex{
		bySHA:         map[string]string{"AB12CD34": "D:\\models\\watercolor.safetensors"},
		resident:      map[string]bool{"AB12CD34": true},
		ownedVersions: map[string][]ownedVersion{},
	}
	idx.Annotate(page.Items)
	if got := page.Items[0].Local; got == nil || got.Status != MatchHave || !got.Resident {
		t.Fatalf("local match = %+v, want have and resident", got)
	}

	// Owned but not on disk: still "have", because it is still the user's model
	// and calling it new would prompt a pointless re-download -- but not
	// resident, so a card can offer to fetch it back. The display path survives,
	// which is why residency cannot be inferred from the path being empty.
	evicted := &LocalIndex{
		bySHA:         map[string]string{"AB12CD34": "D:\\models\\watercolor.safetensors"},
		resident:      map[string]bool{"AB12CD34": false},
		ownedVersions: map[string][]ownedVersion{},
	}
	evicted.Annotate(page.Items)
	got := page.Items[0].Local
	if got == nil || got.Status != MatchHave {
		t.Fatalf("local match = %+v, want have", got)
	}
	if got.Resident {
		t.Error("an evicted model reported as resident")
	}
	if got.Path == "" {
		t.Error("the display path was dropped for an evicted model")
	}

	// And a library that does not hold it reports new rather than matching some
	// 64-hex string against a Civitai model id.
	empty := &LocalIndex{
		bySHA: map[string]string{}, resident: map[string]bool{},
		ownedVersions: map[string][]ownedVersion{},
	}
	empty.Annotate(page.Items)
	if got := page.Items[0].Local; got == nil || got.Status != MatchNew {
		t.Fatalf("local match = %+v, want new", got)
	}
}

// Sorts a private library cannot express must fall back rather than being sent
// through as something the upstream would silently reinterpret.
func TestUpstreamSearchMapsSortsAndOmitsNSFW(t *testing.T) {
	cases := []struct {
		in        string
		wantSort  string
		wantOrder string
	}{
		{SortNewest, "added", "desc"},
		{SortUpdated, "recent", "desc"},
		{SortDownloads, "name", ""},
		{SortRating, "name", ""},
		{"", "name", ""},
	}
	for _, tc := range cases {
		var got http.Request
		srv := upstreamServer(t, `{"hits":[],"total":0}`, &got)
		c := testClient()
		c.UpstreamBase = srv.URL

		if _, err := (&UpstreamProvider{Client: c}).Search(context.Background(),
			Query{Sort: tc.in, NSFW: false, Types: []string{"lora"}, Text: "cat"}); err != nil {
			t.Fatal(err)
		}
		q := got.URL.Query()
		if q.Get("sort") != tc.wantSort || q.Get("order") != tc.wantOrder {
			t.Errorf("sort %q -> sort=%q order=%q, want sort=%q order=%q",
				tc.in, q.Get("sort"), q.Get("order"), tc.wantSort, tc.wantOrder)
		}
		if q.Get("q") != "cat" || q.Get("type") != "lora" {
			t.Errorf("filters lost: %v", q)
		}
		// nsfw is a tri-state upstream, so sending false would hide every model
		// whose flag was never set -- most of a real library.
		if q.Has("nsfw") {
			t.Errorf("nsfw forwarded to the upstream: %v", q)
		}
	}
}

// Files() must not spend a request. ResolveFiles' budget exists for HuggingFace,
// whose listings carry no hashes; a provider that needs nothing must not be able
// to starve the one that does.
func TestUpstreamFilesIssuesNoRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("Files() made a request to %s", r.URL)
	}))
	defer srv.Close()

	c := testClient()
	c.UpstreamBase = srv.URL
	l := Listing{Provider: ProviderUpstreamID, Files: []RemoteFile{{Name: "a", SHA256: "AA"}}}

	files, err := (&UpstreamProvider{Client: c}).Files(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].SHA256 != "AA" {
		t.Errorf("Files() = %+v, want the listing's own files", files)
	}
}

// A provider that cannot be reached during file resolution must say so.
//
// Swallowing the error turned an outage into a wrong answer stated confidently:
// without hashes, an owned HuggingFace model renders as "new", which invites the
// duplicate download the whole project exists to prevent.
func TestResolveFilesReportsProviderFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream is down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := testClient()
	c.HuggingFaceBase = srv.URL
	c.MaxRetries = 0
	c.MinInterval = 0

	// Two hashless listings from the same provider, so the dedup can be checked
	// as well as the reporting.
	items := []Listing{
		{Provider: ProviderHuggingFaceID, ID: "a/one", Files: []RemoteFile{{Name: "m.safetensors"}}},
		{Provider: ProviderHuggingFaceID, ID: "a/two", Files: []RemoteFile{{Name: "m.safetensors"}}},
	}
	errs := NewRegistry(c).ResolveFiles(context.Background(), items, 25)

	if len(errs) != 1 {
		t.Fatalf("errors = %v, want exactly one entry per failing provider", errs)
	}
	if errs[ProviderHuggingFaceID] == nil {
		t.Errorf("the failing provider is not named: %v", errs)
	}

	// And a healthy run reports nothing, so an empty map means "all fine"
	// rather than "not checked".
	quiet := testClient()
	if errs := NewRegistry(quiet).ResolveFiles(context.Background(),
		[]Listing{{Provider: ProviderUpstreamID, Files: []RemoteFile{{SHA256: "AA"}}}}, 25); len(errs) != 0 {
		t.Errorf("a listing that already had a hash produced errors: %v", errs)
	}
}

// Registration is configuration, not reachability. An upstream nobody named has
// no URL to fail against, so registering it would put an error on every search
// that says only "you have not opted in".
func TestRegistryIncludesUpstreamOnlyWhenConfigured(t *testing.T) {
	plain := NewRegistry(testClient())
	if _, ok := plain.Get(ProviderUpstreamID); ok {
		t.Error("upstream registered with no MM_UPSTREAM_URL set")
	}
	if len(plain.IDs()) != 3 {
		t.Errorf("providers = %v, want the three public ones", plain.IDs())
	}

	c := testClient()
	c.UpstreamBase = "http://nas.example:8737"
	withUpstream := NewRegistry(c)
	if _, ok := withUpstream.Get(ProviderUpstreamID); !ok {
		t.Error("configured upstream not registered")
	}

	// --no-remote silences the third parties without silencing a machine the
	// operator configured themselves.
	quiet := testClient()
	quiet.UpstreamBase = "http://nas.example:8737"
	quiet.NoThirdParty = true
	ids := NewRegistry(quiet).IDs()
	if len(ids) != 1 || ids[0] != ProviderUpstreamID {
		t.Errorf("providers under --no-remote = %v, want only the upstream", ids)
	}

	// And with neither, there is nothing to search at all rather than three
	// providers that will each fail.
	silent := testClient()
	silent.NoThirdParty = true
	if ids := NewRegistry(silent).IDs(); len(ids) != 0 {
		t.Errorf("providers = %v, want none", ids)
	}
}

// A URL that answers but is not a model-manager must say so, rather than
// rendering as a library that happens to be empty.
func TestUpstreamSearchRejectsNonModelManager(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := testClient()
	c.UpstreamBase = srv.URL
	_, err := (&UpstreamProvider{Client: c}).Search(context.Background(), Query{})
	if err == nil {
		t.Fatal("a 404 from /api/models reported as an empty library")
	}
	if !strings.Contains(err.Error(), "model-manager") {
		t.Errorf("error should name the problem: %v", err)
	}
}

func TestUpstreamSearchPaging(t *testing.T) {
	var got http.Request
	srv := upstreamServer(t, `{"hits":[{"sha256":"aa"}],"total":10,"limit":1,"offset":0}`, &got)
	c := testClient()
	c.UpstreamBase = srv.URL

	page, err := (&UpstreamProvider{Client: c}).Search(context.Background(), Query{Page: 3, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Query().Get("offset") != "2" {
		t.Errorf("offset = %q, want 2 for page 3 at limit 1", got.URL.Query().Get("offset"))
	}
	if page.Total != 10 {
		t.Errorf("total = %d, want 10", page.Total)
	}
	if page.NextPage != 4 {
		t.Errorf("next page = %d, want 4", page.NextPage)
	}
}
