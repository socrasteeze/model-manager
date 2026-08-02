package origin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testClient returns a client with throttling and backoff removed.
func testClient() *Client {
	c := NewClient()
	c.MinInterval = 0
	c.MaxRetries = 0
	// NewClient reads the ambient environment; a developer with CIVITAI_API_KEY
	// set must not get different test results from one without it.
	c.APIKey = ""
	c.HFToken = ""
	c.CivitaiBase = ""
	c.HuggingFaceBase = ""
	c.CivArchiveBase = ""
	return c
}

func TestCivitaiSearchFlattensVersions(t *testing.T) {
	body := `{
      "items": [{
        "id": 42, "name": "Test LoRA", "type": "LORA", "nsfw": false,
        "tags": ["style", "anime"],
        "creator": {"username": "someone"},
        "stats": {"downloadCount": 1234, "thumbsUpCount": 7},
        "modelVersions": [
          {"id": 200, "name": "v2", "baseModel": "Pony",
           "trainedWords": ["trigger"], "publishedAt": "2024-05-01T00:00:00Z",
           "images": [{"url": "https://img/1.jpg"}],
           "files": [{"name": "v2.safetensors", "sizeKB": 2048, "primary": true,
                      "metadata": {"format": "SafeTensor"},
                      "hashes": {"SHA256": "aabb"},
                      "downloadUrl": "https://dl/2"}]},
          {"id": 100, "name": "v1", "baseModel": "Pony",
           "publishedAt": "2024-01-01T00:00:00Z", "files": []}
        ]
      }],
      "metadata": {"totalItems": 1, "currentPage": 1, "totalPages": 3}
    }`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("types"); got != "LORA" {
			t.Errorf("types = %q, want LORA", got)
		}
		if got := r.URL.Query().Get("nsfw"); got != "false" {
			t.Errorf("nsfw = %q, want false", got)
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := testClient()
	c.CivitaiBase = srv.URL
	p := &CivitaiProvider{Client: c}

	page, err := p.Search(context.Background(), Query{Types: []string{"lora"}})
	if err != nil {
		t.Fatal(err)
	}

	// Every version becomes its own listing: an older version is frequently the
	// one wanted, and it is what makes an owned-older-version detectable.
	if len(page.Items) != 2 {
		t.Fatalf("got %d listings, want 2", len(page.Items))
	}
	first := page.Items[0]
	if first.VersionID != "200" || first.ID != "42" {
		t.Errorf("ids = %s/%s, want 42/200", first.ID, first.VersionID)
	}
	if first.Type != "lora" {
		t.Errorf("type = %q, want lora", first.Type)
	}
	if first.BaseModel != "Pony" {
		t.Errorf("base = %q, want Pony", first.BaseModel)
	}
	if len(first.Files) != 1 || first.Files[0].SHA256 != "AABB" {
		t.Errorf("file hash not upper-cased: %+v", first.Files)
	}
	// sizeKB is kilobytes; callers must never see that unit.
	if first.Files[0].SizeBytes != 2048*1024 {
		t.Errorf("size = %d, want %d", first.Files[0].SizeBytes, 2048*1024)
	}
	if page.NextPage != 2 {
		t.Errorf("next page = %d, want 2", page.NextPage)
	}
}

// TestCredentialsAreHostScoped is the important one: a single Client now talks
// to three third parties, and a globally applied Authorization header would
// disclose the Civitai key to HuggingFace.
func TestCredentialsAreHostScoped(t *testing.T) {
	var civitaiAuth, hfAuth string

	civitai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		civitaiAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"items":[],"metadata":{}}`))
	}))
	defer civitai.Close()

	hf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hfAuth = r.Header.Get("Authorization")
		w.Write([]byte(`[]`))
	}))
	defer hf.Close()

	c := testClient()
	c.CivitaiBase = civitai.URL
	c.HuggingFaceBase = hf.URL
	c.APIKey = "civitai-secret"
	c.HFToken = "hf-secret"

	if _, err := (&CivitaiProvider{Client: c}).Search(context.Background(), Query{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&HuggingFaceProvider{Client: c}).Search(context.Background(), Query{}); err != nil {
		t.Fatal(err)
	}

	if civitaiAuth != "Bearer civitai-secret" {
		t.Errorf("civitai auth = %q, want the civitai key", civitaiAuth)
	}
	if hfAuth != "Bearer hf-secret" {
		t.Errorf("hf auth = %q, want the hf token", hfAuth)
	}
	if hfAuth == "Bearer civitai-secret" {
		t.Fatal("civitai key leaked to huggingface")
	}
}

func TestHostMatchIsExact(t *testing.T) {
	// A suffix match would treat a lookalike domain as HuggingFace and hand it
	// the token.
	if isHuggingFaceHost("evil-huggingface.co") {
		t.Error("lookalike domain accepted as huggingface")
	}
	if !isHuggingFaceHost("huggingface.co") {
		t.Error("real host rejected")
	}
	if hostMatches("attacker.test", "https://civitai.com/api/v1") {
		t.Error("unrelated host matched the civitai base")
	}
}

func TestHuggingFaceFilesUseLFSHashes(t *testing.T) {
	tree := `[
      {"type":"file","path":"config.json","size":100,"oid":"1111"},
      {"type":"file","path":"sub/model.safetensors","size":135,
       "oid":"deadbeef",
       "lfs":{"oid":"` + repeatHex64('a') + `","size":123456789}},
      {"type":"file","path":"legacy.ckpt","size":50,"oid":"2222"}
    ]`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("recursive") != "true" {
			t.Errorf("tree listing was not recursive: %s", r.URL)
		}
		w.Write([]byte(tree))
	}))
	defer srv.Close()

	c := testClient()
	c.HuggingFaceBase = srv.URL
	p := &HuggingFaceProvider{Client: c}

	files, err := p.Files(context.Background(), Listing{ID: "owner/repo"})
	if err != nil {
		t.Fatal(err)
	}

	// config.json is not a weight file and must not be offered as a download.
	if len(files) != 2 {
		t.Fatalf("got %d files, want 2: %+v", len(files), files)
	}

	var safet, ckpt *RemoteFile
	for i := range files {
		switch files[i].Name {
		case "sub/model.safetensors":
			safet = &files[i]
		case "legacy.ckpt":
			ckpt = &files[i]
		}
	}
	if safet == nil || ckpt == nil {
		t.Fatalf("expected files missing: %+v", files)
	}

	// The LFS oid is the content SHA256, which is what makes already-have
	// detection exact on HuggingFace despite there being no by-hash endpoint.
	if safet.SHA256 != repeatHex64('A') {
		t.Errorf("sha256 = %q, want the LFS oid upper-cased", safet.SHA256)
	}
	if safet.SizeBytes != 123456789 {
		t.Errorf("size = %d, want the LFS size not the pointer size", safet.SizeBytes)
	}
	// A plain git oid is a SHA1 over a blob header. Recording it as a SHA256
	// would produce confident, wrong match results.
	if ckpt.SHA256 != "" {
		t.Errorf("non-LFS file got a hash: %q", ckpt.SHA256)
	}
}

func TestPrimaryFilePrefersSafetensors(t *testing.T) {
	l := Listing{Files: []RemoteFile{
		{Name: "big.ckpt", SizeBytes: 900, Format: "PickleTensor"},
		{Name: "small.safetensors", SizeBytes: 100, Format: "SafeTensor"},
	}}
	got := l.PrimaryFile()
	if got == nil || got.Name != "small.safetensors" {
		t.Fatalf("picked %+v, want the safetensors despite being smaller", got)
	}
}

func TestCivArchiveAcceptsSeveralEnvelopes(t *testing.T) {
	record := `{"modelId": 7, "modelName": "Gone", "type": "LORA",
                "baseModel": "SDXL 1.0", "deletedAt": "2024-03-04T00:00:00Z",
                "files": [{"name":"a.safetensors","sizeKB":1024,
                           "hashes":{"SHA256":"ffee"}}]}`

	for _, envelope := range []string{
		"[" + record + "]",
		`{"items":[` + record + `]}`,
		`{"results":[` + record + `]}`,
		`{"data":[` + record + `]}`,
	} {
		records, _, err := decodeCivArchive(json.RawMessage(envelope))
		if err != nil {
			t.Fatalf("envelope %s: %v", envelope[:12], err)
		}
		if len(records) != 1 {
			t.Fatalf("envelope %s: got %d records", envelope[:12], len(records))
		}
		l := caListing(records[0], "https://civarchive.com")
		if l.ID != "7" || l.Name != "Gone" {
			t.Errorf("bad mapping: %+v", l)
		}
		if l.Files[0].SHA256 != "FFEE" {
			t.Errorf("hash = %q", l.Files[0].SHA256)
		}
		// The removal date is the reason this provider exists; surface it.
		if !strings.Contains(l.Description, "Removed from Civitai") {
			t.Errorf("deletion not surfaced: %q", l.Description)
		}
	}
}

func TestAnnotateHaveOutdatedNew(t *testing.T) {
	st := testStore(t)

	// A local file, enriched from Civitai model 42 version 100.
	const localSHA = "1111111111111111111111111111111111111111111111111111111111111111"
	seed(t, st, localSHA, false)
	cache := NewCache(st)
	if err := cache.PutFound(ProviderCivitaiID, localSHA,
		json.RawMessage(`{"id":100,"modelId":42,"name":"v1"}`), 200); err != nil {
		t.Fatal(err)
	}

	idx, err := BuildLocalIndex(st)
	if err != nil {
		t.Fatal(err)
	}

	items := []Listing{
		// Exact content match.
		{Provider: ProviderCivitaiID, ID: "42", VersionID: "100",
			Files: []RemoteFile{{SHA256: localSHA}}},
		// Same model, different version, hash not held: an update.
		{Provider: ProviderCivitaiID, ID: "42", VersionID: "300",
			Files: []RemoteFile{{SHA256: "2222222222222222222222222222222222222222222222222222222222222222"}}},
		// Unrelated model.
		{Provider: ProviderCivitaiID, ID: "99", VersionID: "1"},
	}
	idx.Annotate(items)

	if got := items[0].Local.Status; got != MatchHave {
		t.Errorf("exact hash match = %q, want have", got)
	}
	if items[0].Local.SHA256 != strings.ToUpper(localSHA) {
		t.Errorf("match sha = %q", items[0].Local.SHA256)
	}
	if got := items[1].Local.Status; got != MatchOutdated {
		t.Errorf("newer version = %q, want outdated", got)
	}
	if items[1].Local.HaveVersionID != "100" {
		t.Errorf("held version = %q, want 100", items[1].Local.HaveVersionID)
	}
	if got := items[2].Local.Status; got != MatchNew {
		t.Errorf("unrelated model = %q, want new", got)
	}
}

func TestSiteBaseStripsAPISuffix(t *testing.T) {
	if got := siteBase("https://huggingface.co/api"); got != "https://huggingface.co" {
		t.Errorf("got %q", got)
	}
	if got := siteBase("http://127.0.0.1:8080"); got != "http://127.0.0.1:8080" {
		t.Errorf("got %q", got)
	}
}

func repeatHex64(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	return string(b)
}


// An owned model's OLDER versions must not badge as updates: Civitai returns
// every version, and calling v1 an "update" to an owned v5 invites a downgrade.
func TestOutdatedOnlyForNewerVersions(t *testing.T) {
	st := testStore(t)
	const localSHA = "3333333333333333333333333333333333333333333333333333333333333333"
	seed(t, st, localSHA, false)
	cache := NewCache(st)
	// The library holds version 500 of model 77.
	if err := cache.PutFound(ProviderCivitaiID, localSHA,
		json.RawMessage(`{"id":500,"modelId":77,"name":"v5"}`), 200); err != nil {
		t.Fatal(err)
	}

	idx, err := BuildLocalIndex(st)
	if err != nil {
		t.Fatal(err)
	}

	items := []Listing{
		{Provider: ProviderCivitaiID, ID: "77", VersionID: "100", VersionName: "v1"}, // older
		{Provider: ProviderCivitaiID, ID: "77", VersionID: "900", VersionName: "v9"}, // newer
	}
	idx.Annotate(items)

	if got := items[0].Local.Status; got != MatchNew {
		t.Errorf("older version = %q, want new (not an update)", got)
	}
	// The held version still rides along so a UI can say "you have v5".
	if items[0].Local.HaveVersionID != "500" {
		t.Errorf("older version lost the held-version context: %+v", items[0].Local)
	}
	if got := items[1].Local.Status; got != MatchOutdated {
		t.Errorf("newer version = %q, want outdated", got)
	}
}

// The ResolveFiles budget counts network spend. A provider that answers from
// the listing's own files must not consume slots, or hashless-but-file-bearing
// providers starve the one provider (HuggingFace) the second fetch exists for.
func TestResolveFilesBudgetCountsNetworkSpend(t *testing.T) {
	tree := `[{"type":"file","path":"m.safetensors","size":9,"oid":"x",
              "lfs":{"oid":"` + repeatHex64('c') + `","size":9}}]`
	var treeCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		treeCalls++
		w.Write([]byte(tree))
	}))
	defer srv.Close()

	c := testClient()
	c.HuggingFaceBase = srv.URL
	reg := NewRegistry(c)

	// Ten CivArchive listings whose files exist but carry no hash (their
	// Files() is a no-op), then one HuggingFace listing needing a real fetch.
	var items []Listing
	for i := 0; i < 10; i++ {
		items = append(items, Listing{
			Provider: ProviderCivArchiveID,
			ID:       fmt.Sprintf("%d", i+1),
			Files:    []RemoteFile{{Name: "gone.safetensors", DownloadURL: "https://x/dl"}},
		})
	}
	items = append(items, Listing{Provider: ProviderHuggingFaceID, ID: "owner/repo"})

	// Budget of 5 would previously be eaten by the first five no-ops.
	reg.ResolveFiles(context.Background(), items, 5)

	if treeCalls != 1 {
		t.Fatalf("tree endpoint called %d times, want exactly 1", treeCalls)
	}
	hf := items[len(items)-1]
	if len(hf.Files) == 0 || hf.Files[0].SHA256 != repeatHex64('C') {
		t.Fatalf("huggingface listing never got its hash: %+v", hf.Files)
	}
}

// TestCivArchiveAcceptsStringIDs is the fix for a live failure: a real run
// against civarchive.com returned `"v9208"` for an id field. json.Number
// rejects any non-numeric token outright ("invalid number literal"), which
// took the entire page down with it -- this is the exact reproduction.
func TestCivArchiveAcceptsStringIDs(t *testing.T) {
	body := `{"items":[
      {"id": "v9208", "modelId": 42, "modelName": "String Version", "type": "LORA"},
      {"id": 100, "modelId": 43, "modelName": "Numeric Id", "type": "LORA"}
    ]}`
	records, _, err := decodeCivArchive(json.RawMessage(body))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].ID.String() != "v9208" {
		t.Errorf("string id = %q, want v9208", records[0].ID.String())
	}
	if records[1].ID.String() != "100" {
		t.Errorf("numeric id = %q, want 100", records[1].ID.String())
	}
}

// A single record with a field shape not yet learned must cost only that
// record, not the whole page -- the "defensive decoding" the package claims.
func TestCivArchiveOneBadRecordDoesNotBlankThePage(t *testing.T) {
	body := `{"items":[
      {"id": "ok-1", "modelId": 1, "modelName": "Fine"},
      {"id": 2, "modelId": {"nested":"not a valid id shape"}, "modelName": "Broken"},
      {"id": "ok-3", "modelId": 3, "modelName": "Also Fine"}
    ]}`
	records, _, err := decodeCivArchive(json.RawMessage(body))
	if err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	// The malformed middle record is dropped; the two good ones survive.
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2 (one dropped, two kept): %+v", len(records), records)
	}
	names := []string{records[0].ModelName, records[1].ModelName}
	if names[0] != "Fine" || names[1] != "Also Fine" {
		t.Errorf("wrong records survived: %v", names)
	}
}

// SearchAll must return a non-nil, empty slice when every provider errors, so
// mm browse --json and the HTTP /api/browse response serialize as [] rather
// than null.
func TestSearchAllReturnsEmptySliceNotNil(t *testing.T) {
	c := testClient()
	c.CivitaiBase = "http://127.0.0.1:1" // nothing listens here
	c.HuggingFaceBase = "http://127.0.0.1:1"
	c.CivArchiveBase = "http://127.0.0.1:1"
	reg := NewRegistry(c)

	items, errs := reg.SearchAll(context.Background(), nil, Query{})
	if items == nil {
		t.Fatal("SearchAll returned a nil slice; JSON callers would see null instead of []")
	}
	if len(items) != 0 {
		t.Fatalf("got %d items from unreachable providers", len(items))
	}
	if len(errs) == 0 {
		t.Fatal("expected every provider to report an error")
	}
}
