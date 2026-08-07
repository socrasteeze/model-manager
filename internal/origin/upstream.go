package origin

// Browsing another model-manager.
//
// The three providers beside this one are public indexes of models that mostly
// are not yours. This one is a second instance of this program -- typically the
// machine that holds the master library -- and it is a provider for the same
// reason they are: the question a browser asks it ("what exists that I could
// fetch?") is the same question, and the answer lands in the same Listing.
//
// That framing buys the interesting property for free. Annotate compares a
// listing to the local library by content hash, so a model on the NAS shows up
// as "have" or "new" against this machine's disk with no code that knows the
// upstream is special. And because the upstream's primary key for a model *is*
// its SHA256, the download URL and the hash the transfer verifies against are
// the same string -- there is no filename to mismatch and no metadata to
// disagree about.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// UpstreamProvider searches another model-manager daemon.
type UpstreamProvider struct{ Client *Client }

func (p *UpstreamProvider) ID() string { return ProviderUpstreamID }

func (p *UpstreamProvider) DisplayName() string { return p.client().UpstreamLabel() }

func (p *UpstreamProvider) client() *Client {
	if p.Client == nil {
		return NewClient()
	}
	return p.Client
}

// upstreamHit is the subset of the upstream's SearchHit this adapter reads.
//
// Narrower than store.SearchHit on purpose, and the narrowing is the point:
// `path` is the upstream's own absolute filesystem path, which is meaningless
// on this machine and is nobody else's business. Not decoding it means it never
// enters this process, rather than merely never being displayed.
//
// Everything here is optional on the wire. encoding/json zero-fills what an
// older upstream omits, which is what lets a new client browse an old daemon.
type upstreamHit struct {
	SHA256       string   `json:"sha256"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	BaseModel    string   `json:"base_model"`
	Version      string   `json:"version"`
	Format       string   `json:"format"`
	Size         int64    `json:"size"`
	TriggerWords []string `json:"trigger_words"`
	Tags         []string `json:"tags"`
	PreviewImage string   `json:"preview_image"`
	Filename     string   `json:"filename"`
	NSFW         *bool    `json:"nsfw"`
}

type upstreamResults struct {
	Hits   []upstreamHit `json:"hits"`
	Total  int           `json:"total"`
	Limit  int           `json:"limit"`
	Offset int           `json:"offset"`
}

// Search queries the upstream's own library endpoint.
//
// Deliberately GET /api/models -- the endpoint the upstream already serves to
// its own UI -- rather than something new built for this. Three reasons, and the
// first is the one that matters: it is what lets a client newer than the NAS
// still browse it, so upgrading the laptop does not require upgrading the array.
// The second is that the library's own query already expresses everything a
// Query can say, including a real total. The third is that a second search path
// is a second thing that can drift from the first.
func (p *UpstreamProvider) Search(ctx context.Context, q Query) (*Page, error) {
	c := p.client()
	base := c.upstreamBase()
	if base == "" {
		return nil, fmt.Errorf("origin: no upstream configured (set MM_UPSTREAM_URL)")
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	page := q.Page
	if page < 1 {
		page = 1
	}
	offset := (page - 1) * limit

	v := url.Values{}
	if strings.TrimSpace(q.Text) != "" {
		v.Set("q", q.Text)
	}
	for _, t := range q.Types {
		v.Add("type", t)
	}
	for _, b := range q.BaseModels {
		v.Add("base_model", b)
	}
	// A private library has no download count and no rating, so those two orders
	// have no honest analogue here and fall back to name rather than to an
	// arbitrary column that would look sorted without being.
	switch q.Sort {
	case SortNewest:
		v.Set("sort", "added")
		v.Set("order", "desc")
	case SortUpdated:
		v.Set("sort", "recent")
		v.Set("order", "desc")
	default:
		v.Set("sort", "name")
	}
	// q.NSFW is deliberately not forwarded. The upstream treats nsfw as a
	// tri-state filter, so sending false would drop every model whose flag was
	// never set -- most of a real library. The flag exists to keep a public
	// index's adult content out of a search, which is not a reason to hide the
	// user's own files from the user.
	v.Set("limit", strconv.Itoa(limit))
	v.Set("offset", strconv.Itoa(offset))

	body, status, err := c.getUpstreamJSON(ctx, base+"/api/models?"+v.Encode())
	if err != nil {
		return nil, err
	}
	if status == 404 || len(body) == 0 {
		// A 404 from getUpstreamJSON is a definite answer with a nil body. From
		// this endpoint it means the URL is not a model-manager at all, which is
		// worth saying plainly rather than reporting as an empty library.
		return nil, fmt.Errorf("origin: %s does not look like a model-manager API", base)
	}

	var results upstreamResults
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("origin: decoding upstream search: %w", err)
	}

	out := &Page{Items: make([]Listing, 0, len(results.Hits)), Total: results.Total}
	for _, hit := range results.Hits {
		if hit.SHA256 == "" {
			continue
		}
		out.Items = append(out.Items, p.listing(base, hit))
	}
	if offset+len(results.Hits) < results.Total {
		out.NextPage = page + 1
	}
	return out, nil
}

func (p *UpstreamProvider) listing(base string, hit upstreamHit) Listing {
	sha := strings.ToLower(hit.SHA256)

	name := strings.TrimSpace(hit.Name)
	if name == "" {
		name = hit.Filename
	}

	// ID is the content hash rather than any remote model id the upstream might
	// know about.
	//
	// The doc on Listing.ID asks for something "stable enough to re-fetch with",
	// and for this provider the hash is literally that: it is the path parameter
	// of the download URL below. Threading the upstream's Civitai model id
	// through here instead would light up match()'s outdated branch, which is
	// tempting -- but it would put a Civitai id into the upstream/ namespace of
	// the ownership index, where two providers' id spaces then overlap, and it
	// would require fields an older upstream does not send. Exact-hash matching
	// still gives have-vs-new correctly, which is the answer that decides
	// whether to pull.
	l := Listing{
		Provider:     ProviderUpstreamID,
		ID:           sha,
		Name:         name,
		Type:         hit.Type,
		BaseModel:    hit.BaseModel,
		VersionName:  hit.Version,
		Tags:         hit.Tags,
		TriggerWords: hit.TriggerWords,
		NSFW:         hit.NSFW != nil && *hit.NSFW,
		Files: []RemoteFile{{
			Name:      hit.Filename,
			SizeBytes: hit.Size,
			// Upper-cased to match what the other providers report, since
			// Annotate upper-cases both sides before comparing.
			SHA256: strings.ToUpper(sha),
			// The store's own vocabulary ("safetensors", "gguf", "ckpt", "pt")
			// passes through unchanged: isSafeFormat already reads it, so a
			// pickle from the upstream is flagged as one exactly as a pickle
			// from Civitai would be.
			Format:      hit.Format,
			Primary:     true,
			DownloadURL: base + "/api/models/" + sha + "/file",
			// The upstream requires a bearer token whenever it is bound off
			// loopback, and a client with no token configured should be told
			// that before it starts a transfer rather than after a 401.
			RequiresAuth: p.client().UpstreamToken != "",
		}},
	}
	if hit.PreviewImage != "" {
		l.ThumbnailURL = base + "/api/previews/" + hit.PreviewImage
	}
	return l
}

// Files returns what Search already resolved, without a request.
//
// Every upstream listing carries a hash, so ResolveFiles skips this provider
// entirely and this is dead in practice. It is written this way rather than
// left to fetch because if it ever were called, spending a request would
// consume a slot of the resolve budget -- and that budget exists for
// HuggingFace, whose listings have no hashes and would report as "new" without
// it. A provider that needs nothing must not be able to starve the one that
// does.
func (p *UpstreamProvider) Files(_ context.Context, l Listing) ([]RemoteFile, error) {
	return l.Files, nil
}
