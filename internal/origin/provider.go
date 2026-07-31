package origin

// Remote browsing.
//
// Phase 2 could only ask "what is this file I already have?" -- an exact lookup
// keyed by a hash we computed locally. Browsing is the other direction: asking a
// remote index what exists, before any of it is on disk.
//
// The two directions have very different trust properties, and the type system
// keeps them apart. A hash lookup binds a record to content and cannot bind the
// wrong one (§2). A search result is a *claim* about a file that does not exist
// here yet, so nothing it says is recorded against a local model until the bytes
// have been downloaded and hashed. A Listing is therefore never a source of
// field observations; it is only ever a thing you might choose to fetch.

import (
	"context"
	"sort"
	"strings"
)

// Provider IDs. These are stable strings: they are persisted in the origin
// cache and appear in the API, so renaming one is a migration.
const (
	ProviderCivitaiID     = "civitai"
	ProviderCivArchiveID  = "civarchive"
	ProviderHuggingFaceID = "huggingface"
)

// Provider is a remote index that can be searched and downloaded from.
//
// Deliberately small. Every provider here is a different shape underneath --
// Civitai has model/version/file nesting, HuggingFace has repos with flat file
// lists and no hash index, CivArchive is a mirror of dead Civitai records -- and
// the only thing the rest of the app needs from any of them is "show me
// candidates" and "tell me how to fetch one".
type Provider interface {
	// ID returns the stable provider identifier.
	ID() string

	// DisplayName is what a human sees in a UI tab.
	DisplayName() string

	// Search returns one page of results.
	Search(ctx context.Context, q Query) (*Page, error)

	// Files resolves a listing to its downloadable files. Providers that
	// already populate Listing.Files during Search may return them directly;
	// the separate call exists because Civitai's search response omits file
	// hashes on some sort orders and a second fetch is the only way to get them.
	Files(ctx context.Context, l Listing) ([]RemoteFile, error)
}

// Query is a provider-independent search request.
//
// Fields a given provider cannot express are ignored rather than approximated.
// Silently widening a filter is worse than not applying it: the caller would
// believe a constraint held when it did not.
type Query struct {
	// Text is the free-text search. Empty means "browse everything", which is
	// how the default listing (most downloaded, newest) is requested.
	Text string

	// Types filters by model type using this project's vocabulary
	// (lora, checkpoint, embedding, ...). Providers map it to their own.
	Types []string

	// BaseModels filters by base model using normalized names ("SDXL", "Pony",
	// "Illustrious", "Flux.1 D", ...), matching what normalizeCivitaiBase emits
	// so that a filter chosen from local facets means the same thing remotely.
	BaseModels []string

	// NSFW includes adult content. Off by default.
	//
	// Note that on Civitai this interacts with the host: civitai.com and
	// civitai.red are the same API over different catalogues, .red being the
	// adult split. Asking for NSFW content from a host that does not serve it
	// returns nothing rather than erroring, so the host is a setting
	// (MM_CIVITAI_API) rather than a constant.
	NSFW bool

	// Sort is a provider-independent ordering. See the Sort* constants.
	Sort string

	// Page is 1-based. Cursor-based providers translate it internally.
	Page int

	// Cursor continues from a previous Page.NextCursor. Preferred over Page
	// where a provider supports it: deep paging by offset drifts as the index
	// changes underneath you.
	Cursor string

	// Limit is the page size. Zero means the provider default.
	Limit int
}

// Sort orders. Not every provider supports every one; each maps to its closest
// native equivalent and falls back to relevance.
const (
	SortRelevance = "relevance"
	SortDownloads = "downloads"
	SortNewest    = "newest"
	SortUpdated   = "updated"
	SortRating    = "rating"
)

// Page is one page of search results.
type Page struct {
	Items []Listing `json:"items"`

	// NextCursor is empty when there is no further page.
	NextCursor string `json:"next_cursor,omitempty"`

	// NextPage is 0 when there is no further page.
	NextPage int `json:"next_page,omitempty"`

	// Total is the provider's reported match count, or 0 if it does not report
	// one. Several of these indexes report an estimate, so it is display-only.
	Total int `json:"total,omitempty"`
}

// Listing is one searchable item: a Civitai model version, a HuggingFace repo,
// or an archived CivArchive record.
//
// This is a remote claim, not a local record. It is never written to
// field_value. Metadata only becomes an observation after the file is fetched
// and hashed, at which point the ordinary Civitai/HuggingFace extraction path
// runs against a real local sha256.
type Listing struct {
	Provider string `json:"provider"`

	// ID is the provider-native identifier, stable enough to re-fetch with.
	ID string `json:"id"`

	// VersionID distinguishes releases of the same model where the provider has
	// that concept. It is what makes "a newer version exists" answerable.
	VersionID   string `json:"version_id,omitempty"`
	VersionName string `json:"version_name,omitempty"`

	Name        string   `json:"name"`
	Author      string   `json:"author,omitempty"`
	Type        string   `json:"type,omitempty"`
	BaseModel   string   `json:"base_model,omitempty"`
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	NSFW        bool     `json:"nsfw,omitempty"`

	Downloads int64 `json:"downloads,omitempty"`
	Likes     int64 `json:"likes,omitempty"`

	// PublishedAt/UpdatedAt are RFC3339 as the provider gave them. Not parsed
	// into time.Time because several providers omit or malform them and a zero
	// time is indistinguishable from "not stated".
	PublishedAt string `json:"published_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`

	// PageURL is the human-facing page, so a UI can always offer "open on the
	// site" even when this app cannot render everything the site knows.
	PageURL      string `json:"page_url,omitempty"`
	ThumbnailURL string `json:"thumbnail_url,omitempty"`

	// TriggerWords are carried through because they are the single most useful
	// thing to see before deciding whether to download a LoRA.
	TriggerWords []string `json:"trigger_words,omitempty"`

	Files []RemoteFile `json:"files,omitempty"`

	// Local is filled in by the annotator, not by the provider. It answers "do
	// I already have this?" by hash rather than by name (§2), which is the
	// thing every other model browser gets wrong.
	Local *LocalMatch `json:"local,omitempty"`
}

// RemoteFile is a downloadable file within a Listing.
type RemoteFile struct {
	Name string `json:"name"`

	// SizeBytes is 0 when the provider does not say. Civitai reports sizeKB as
	// a float, which is converted here so callers never deal in kilobytes.
	SizeBytes int64 `json:"size_bytes,omitempty"`

	// SHA256 is uppercase hex when known, empty otherwise. This is the field
	// that makes already-have detection exact; without it a match is a guess.
	SHA256 string `json:"sha256,omitempty"`

	// Format is the container ("SafeTensor", "PickleTensor", "GGUF", ...) as
	// reported. Worth surfacing: a pickle is a code-execution risk and the UI
	// should be able to say so before anyone clicks download.
	Format string `json:"format,omitempty"`

	// Primary marks the file a provider considers the main artifact, as opposed
	// to a config, VAE or training log shipped alongside it.
	Primary bool `json:"primary,omitempty"`

	DownloadURL string `json:"download_url,omitempty"`

	// RequiresAuth means the provider will refuse this download without an API
	// key. Knowing in advance turns a confusing 401 into an explanation.
	RequiresAuth bool `json:"requires_auth,omitempty"`
}

// LocalMatch records how a search result relates to what is already on disk.
type LocalMatch struct {
	// Status is one of the Match* constants.
	Status string `json:"status"`

	// SHA256 is the local file this matched, when there is one.
	SHA256 string `json:"sha256,omitempty"`

	// Path is one local path for that file, for display.
	Path string `json:"path,omitempty"`

	// HaveVersionID is the version of this model already held, set when Status
	// is MatchOutdated.
	HaveVersionID string `json:"have_version_id,omitempty"`
	HaveVersionName string `json:"have_version_name,omitempty"`
}

// Local match statuses.
const (
	// MatchHave means a local file has the same content hash. Exact.
	MatchHave = "have"

	// MatchOutdated means a different version of the same remote model is held
	// locally, and this listing is newer. This is the "can be updated" signal.
	MatchOutdated = "outdated"

	// MatchNew means nothing local corresponds to this listing.
	MatchNew = "new"
)

// PrimaryFile returns the file a download should default to.
//
// Preference order: the provider's own primary flag, then a safetensors file,
// then the largest. The safetensors preference is deliberate and outranks size:
// where a repo ships both, the pickle is the legacy copy and fetching it would
// be choosing the format that can execute code on load.
func (l Listing) PrimaryFile() *RemoteFile {
	if len(l.Files) == 0 {
		return nil
	}
	best := -1
	score := func(f RemoteFile) int {
		s := 0
		if f.Primary {
			s += 4
		}
		if isSafeFormat(f) {
			s += 2
		}
		return s
	}
	for i, f := range l.Files {
		if best < 0 {
			best = i
			continue
		}
		si, sb := score(f), score(l.Files[best])
		if si > sb || (si == sb && f.SizeBytes > l.Files[best].SizeBytes) {
			best = i
		}
	}
	return &l.Files[best]
}

func isSafeFormat(f RemoteFile) bool {
	if strings.EqualFold(f.Format, "SafeTensor") || strings.EqualFold(f.Format, "safetensors") {
		return true
	}
	name := strings.ToLower(f.Name)
	return strings.HasSuffix(name, ".safetensors") || strings.HasSuffix(name, ".gguf")
}

// Registry holds the enabled providers in a stable order.
type Registry struct {
	providers []Provider
}

// NewRegistry builds a registry for the given client.
//
// All three are always constructed. A provider that cannot be reached fails at
// search time with an error naming it, which is a better failure than silently
// omitting a source the user asked to search.
func NewRegistry(c *Client) *Registry {
	if c == nil {
		c = NewClient()
	}
	return &Registry{providers: []Provider{
		&CivitaiProvider{Client: c},
		&CivArchiveProvider{Client: c},
		&HuggingFaceProvider{Client: c},
	}}
}

// All returns every registered provider.
func (r *Registry) All() []Provider { return r.providers }

// Get returns a provider by ID.
func (r *Registry) Get(id string) (Provider, bool) {
	for _, p := range r.providers {
		if strings.EqualFold(p.ID(), id) {
			return p, true
		}
	}
	return nil, false
}

// IDs lists the registered provider IDs.
func (r *Registry) IDs() []string {
	out := make([]string, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, p.ID())
	}
	return out
}

// SearchAll queries several providers and merges the results.
//
// A failing provider does not fail the search. These are three independent
// third-party services; if CivArchive is down, results from Civitai are still
// worth showing, and the error is returned alongside them so the UI can say
// which source is missing rather than quietly under-reporting.
func (r *Registry) SearchAll(ctx context.Context, ids []string, q Query) ([]Listing, map[string]error) {
	if len(ids) == 0 {
		ids = r.IDs()
	}
	var (
		out  []Listing
		errs = map[string]error{}
	)
	for _, id := range ids {
		p, ok := r.Get(id)
		if !ok {
			continue
		}
		page, err := p.Search(ctx, q)
		if err != nil {
			errs[id] = err
			continue
		}
		if page != nil {
			out = append(out, page.Items...)
		}
	}
	sortListings(out, q.Sort)
	return out, errs
}

// ResolveFiles fills in file details for listings whose search response carried
// no content hashes.
//
// Needed for correctness, not polish. HuggingFace's search response lists
// filenames but no hashes, so without this every HuggingFace result would be
// reported as "new" even when the exact file is already on disk -- prompting
// precisely the duplicate download this project exists to prevent.
//
// Costs one request per listing, so it is capped and skipped for providers that
// already supplied hashes. Listings are resolved in order, and a failure leaves
// that listing's files as they were rather than aborting the batch.
func (r *Registry) ResolveFiles(ctx context.Context, items []Listing, max int) {
	if max <= 0 {
		max = 25
	}
	// The budget counts NETWORK SPEND, not loop iterations. A provider whose
	// Files() returns the listing's own files unchanged has fetched nothing
	// and must not consume a slot -- otherwise a page of hashless CivArchive
	// listings exhausts the budget before the HuggingFace listings (the ones
	// the second fetch exists for) are ever reached, and every one of them
	// reports as "new" even when owned. Errors DO count: an erroring provider
	// spent a request per try, and exempting it would turn the cap into
	// unbounded retries.
	spent := 0
	for i := range items {
		if spent >= max || ctx.Err() != nil {
			return
		}
		if hasAnyHash(items[i]) {
			continue
		}
		p, ok := r.Get(items[i].Provider)
		if !ok {
			continue
		}
		before := len(items[i].Files)
		files, err := p.Files(ctx, items[i])
		if err != nil {
			spent++
			continue
		}
		if len(files) == 0 {
			continue
		}
		changed := len(files) != before || hasHashIn(files)
		items[i].Files = files
		if changed {
			spent++
		}
	}
}

func hasHashIn(files []RemoteFile) bool {
	for _, f := range files {
		if f.SHA256 != "" {
			return true
		}
	}
	return false
}

func hasAnyHash(l Listing) bool {
	for _, f := range l.Files {
		if f.SHA256 != "" {
			return true
		}
	}
	return false
}

// sortListings gives a merged multi-provider result set one consistent order.
//
// Only applied across providers; within a provider the server's own ordering is
// already correct and better informed than anything reconstructible here.
func sortListings(items []Listing, order string) {
	switch order {
	case SortDownloads:
		sort.SliceStable(items, func(i, j int) bool { return items[i].Downloads > items[j].Downloads })
	case SortNewest:
		sort.SliceStable(items, func(i, j int) bool { return items[i].PublishedAt > items[j].PublishedAt })
	case SortUpdated:
		sort.SliceStable(items, func(i, j int) bool { return items[i].UpdatedAt > items[j].UpdatedAt })
	}
}

// dedupeStrings removes blanks and duplicates while preserving order.
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[strings.ToLower(s)] {
			continue
		}
		seen[strings.ToLower(s)] = true
		out = append(out, s)
	}
	return out
}
