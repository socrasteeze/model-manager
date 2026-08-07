package origin

// Civitai search.
//
// The by-hash lookup in civitai.go answers "what is this file?". This answers
// "what exists?", which is the model-browser half of the feature and a wholly
// different endpoint shape: /models returns models, each carrying nested
// versions, each carrying files.
//
// The nesting is why a Listing is flattened to the *version* rather than the
// model. A model is an abstract thing with no hash and nothing to download; a
// version is the unit that has files, a base model, trigger words and an
// identity you can compare against what is already on disk.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/socrasteeze/model-manager/internal/modeltype"
)

// CivitaiProvider searches Civitai.
type CivitaiProvider struct{ Client *Client }

func (p *CivitaiProvider) ID() string          { return ProviderCivitaiID }
func (p *CivitaiProvider) DisplayName() string { return "Civitai" }

// civitaiSearchResponse is the /models envelope.
type civitaiSearchResponse struct {
	Items    []civitaiSearchModel `json:"items"`
	Metadata struct {
		TotalItems  int    `json:"totalItems"`
		CurrentPage int    `json:"currentPage"`
		PageSize    int    `json:"pageSize"`
		TotalPages  int    `json:"totalPages"`
		NextCursor  any    `json:"nextCursor"`
		NextPage    string `json:"nextPage"`
	} `json:"metadata"`
}

type civitaiSearchModel struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	NSFW        bool     `json:"nsfw"`
	Tags        []string `json:"tags"`
	Description string   `json:"description"`
	Creator     struct {
		Username string `json:"username"`
	} `json:"creator"`
	Stats struct {
		DownloadCount int64 `json:"downloadCount"`
		ThumbsUpCount int64 `json:"thumbsUpCount"`
		FavoriteCount int64 `json:"favoriteCount"`
	} `json:"stats"`
	ModelVersions []civitaiSearchVersion `json:"modelVersions"`
}

type civitaiSearchVersion struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	BaseModel    string   `json:"baseModel"`
	Description  string   `json:"description"`
	TrainedWords []string `json:"trainedWords"`
	CreatedAt    string   `json:"createdAt"`
	PublishedAt  string   `json:"publishedAt"`
	DownloadURL  string   `json:"downloadUrl"`

	Files []struct {
		Name     string            `json:"name"`
		SizeKB   float64           `json:"sizeKB"`
		Type     string            `json:"type"`
		Primary  bool              `json:"primary"`
		Hashes   map[string]string `json:"hashes"`
		Metadata struct {
			Format string `json:"format"`
		} `json:"metadata"`
		DownloadURL string `json:"downloadUrl"`
	} `json:"files"`

	Images []struct {
		URL  string `json:"url"`
		NSFW any    `json:"nsfw"`
	} `json:"images"`
}

// Search queries /models.
func (p *CivitaiProvider) Search(ctx context.Context, q Query) (*Page, error) {
	c := p.client()
	endpoint := c.civitaiBase() + "/models?" + civitaiSearchParams(q).Encode()

	raw, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("civitai: search: %w", err)
	}
	if raw == nil {
		return &Page{}, nil
	}

	var resp civitaiSearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("civitai: decoding search response: %w", err)
	}

	page := &Page{Total: resp.Metadata.TotalItems}
	for _, m := range resp.Items {
		page.Items = append(page.Items, civitaiListings(m)...)
	}
	// nextCursor is typed loosely upstream: a string for some sort orders, a
	// number for others, and absent at the end of the result set.
	page.NextCursor = looseString(resp.Metadata.NextCursor)
	if resp.Metadata.CurrentPage > 0 && resp.Metadata.CurrentPage < resp.Metadata.TotalPages {
		page.NextPage = resp.Metadata.CurrentPage + 1
	}
	return page, nil
}

// civitaiSearchParams maps a provider-independent Query onto Civitai's filters.
func civitaiSearchParams(q Query) url.Values {
	v := url.Values{}
	if q.Text != "" {
		v.Set("query", q.Text)
	}
	for _, t := range q.Types {
		if native := civitaiType(t); native != "" {
			v.Add("types", native)
		}
	}
	for _, b := range q.BaseModels {
		// The query carries normalized names ("SDXL", "Flux") because that is
		// what local facets show, but Civitai's filter wants its own labels
		// ("SDXL 1.0", "Flux.1 D"). Expand each normalized name to the native
		// spellings it covers; an unrecognized value passes through verbatim so
		// a user typing Civitai's exact label still works.
		for _, native := range civitaiBaseModels(b) {
			v.Add("baseModels", native)
		}
	}
	if sort := civitaiSort(q.Sort); sort != "" {
		v.Set("sort", sort)
	}

	// The nsfw parameter is a ceiling, not a request: false excludes adult
	// content, true permits it. Omitting it entirely lets the host's own
	// default apply, which differs between civitai.com and civitai.red.
	if !q.NSFW {
		v.Set("nsfw", "false")
	}

	limit := q.Limit
	if limit <= 0 {
		limit = 24
	}
	if limit > 100 {
		limit = 100
	}
	v.Set("limit", strconv.Itoa(limit))

	// Cursor paging is preferred where available: /models by offset drifts as
	// the index shifts, silently repeating and skipping items across pages.
	switch {
	case q.Cursor != "":
		v.Set("cursor", q.Cursor)
	case q.Page > 1:
		v.Set("page", strconv.Itoa(q.Page))
	}
	return v
}

// civitaiType maps this project's type vocabulary to Civitai's.
func civitaiType(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "lora":
		return "LORA"
	case "lycoris":
		return "LoCon"
	case "checkpoint":
		return "Checkpoint"
	case "embedding", "textual inversion":
		return "TextualInversion"
	case "controlnet":
		return "Controlnet"
	case "vae":
		return "VAE"
	case "upscaler":
		return "Upscaler"
	case "hypernetwork":
		return "Hypernetwork"
	case "motion", "motionmodule":
		return "MotionModule"
	case "wildcards":
		return "Wildcards"
	case "poses":
		return "Poses"
	default:
		return ""
	}
}

// civitaiTypeToLocal is the inverse, for reading results back.
//
// An unrecognised Civitai type becomes "", not itself. That used to be
// `strings.ToLower(t)`, which looked harmless while a type was only displayed
// — but the type reaches the download path and decides a directory name, so
// passing an unknown provider string through meant this app could be talked
// into creating a folder named whatever Civitai invented next.
func civitaiTypeToLocal(t string) string {
	return modeltype.Normalize(t)
}

// civitaiBaseModels expands a normalized base-model name to the native labels
// Civitai's filter accepts. The inverse of normalizeCivitaiBase, and lossy the
// same way: one normalized name covers several native ones, so the expansion
// is a union. Approximate by design -- documented in docs/phase6.md.
func civitaiBaseModels(normalized string) []string {
	switch strings.ToLower(strings.TrimSpace(normalized)) {
	case "sdxl":
		return []string{"SDXL 1.0", "SDXL 0.9", "SDXL Turbo", "SDXL Lightning"}
	case "pony":
		return []string{"Pony"}
	case "illustrious":
		return []string{"Illustrious"}
	case "noobai":
		return []string{"NoobAI"}
	case "flux", "flux.1", "flux1":
		return []string{"Flux.1 D", "Flux.1 S"}
	case "flux.2", "flux2":
		return []string{"Flux.2"}
	case "krea 2", "krea2", "krea":
		return []string{"Flux.1 Krea"}
	case "sd 1.5", "sd1.5":
		return []string{"SD 1.5"}
	case "sd 2.x", "sd2":
		return []string{"SD 2.0", "SD 2.1"}
	case "sd 3", "sd3":
		return []string{"SD 3", "SD 3.5", "SD 3.5 Medium", "SD 3.5 Large"}
	default:
		return []string{normalized}
	}
}

func civitaiSort(s string) string {
	switch s {
	case SortDownloads:
		return "Most Downloaded"
	case SortNewest:
		return "Newest"
	case SortRating:
		return "Highest Rated"
	default:
		return ""
	}
}

// civitaiListings flattens a model into one Listing per version.
//
// Every version is emitted, not just the newest. Older versions are frequently
// the ones people actually want -- a v2 retrained on a different base is not an
// upgrade -- and emitting them all is also what lets the annotator recognise an
// owned older version and mark the newer one as an available update.
func civitaiListings(m civitaiSearchModel) []Listing {
	localType := civitaiTypeToLocal(m.Type)
	out := make([]Listing, 0, len(m.ModelVersions))

	for _, v := range m.ModelVersions {
		l := Listing{
			Provider:    ProviderCivitaiID,
			ID:          strconv.FormatInt(m.ID, 10),
			VersionID:   strconv.FormatInt(v.ID, 10),
			VersionName: v.Name,
			Name:        m.Name,
			Author:      m.Creator.Username,
			Type:        localType,
			BaseModel:   normalizeCivitaiBase(v.BaseModel),
			Description: stripHTML(firstNonEmpty(v.Description, m.Description)),
			Tags:        dedupeStrings(m.Tags),
			NSFW:        m.NSFW,
			Downloads:   m.Stats.DownloadCount,
			Likes:       m.Stats.ThumbsUpCount + m.Stats.FavoriteCount,
			PublishedAt: firstNonEmpty(v.PublishedAt, v.CreatedAt),
			UpdatedAt:   firstNonEmpty(v.PublishedAt, v.CreatedAt),
			PageURL: fmt.Sprintf("https://civitai.com/models/%d?modelVersionId=%d",
				m.ID, v.ID),
			TriggerWords: dedupeStrings(v.TrainedWords),
		}
		if len(v.Images) > 0 {
			l.ThumbnailURL = v.Images[0].URL
		}
		for _, f := range v.Files {
			rf := RemoteFile{
				Name:      f.Name,
				SizeBytes: int64(f.SizeKB * 1024),
				Format:    firstNonEmpty(f.Metadata.Format, f.Type),
				Primary:   f.Primary,
				// The version-level downloadUrl resolves to the PRIMARY file,
				// so it is only a valid fallback for the primary. Inheriting
				// it on a secondary file (a VAE, a config) would pair that
				// file's hash with the primary's bytes -- a multi-GB download
				// that can only fail verification, or worse, publish the
				// wrong file under the right name.
				DownloadURL: f.DownloadURL,
			}
			if rf.DownloadURL == "" && f.Primary {
				rf.DownloadURL = v.DownloadURL
			}
			// Hash keys are upper-case in the API but not contractually so.
			for k, hv := range f.Hashes {
				if strings.EqualFold(k, "SHA256") {
					rf.SHA256 = strings.ToUpper(hv)
					break
				}
			}
			l.Files = append(l.Files, rf)
		}
		out = append(out, l)
	}
	return out
}

// Files returns the listing's files, fetching the version when Search did not
// include them -- or included them without hashes, which some sort orders do.
// Skipping the fetch in that case would make the "second fetch is the only way
// to get them" purpose of this method unreachable exactly when it applies.
func (p *CivitaiProvider) Files(ctx context.Context, l Listing) ([]RemoteFile, error) {
	if len(l.Files) > 0 && hasHashIn(l.Files) {
		return l.Files, nil
	}
	if l.VersionID == "" {
		return nil, fmt.Errorf("civitai: listing has no version id")
	}
	c := p.client()
	raw, _, err := c.getJSON(ctx, c.civitaiBase()+"/model-versions/"+url.PathEscape(l.VersionID))
	if err != nil {
		return nil, fmt.Errorf("civitai: fetching version %s: %w", l.VersionID, err)
	}
	if raw == nil {
		return nil, nil
	}

	var v CivitaiVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("civitai: decoding version %s: %w", l.VersionID, err)
	}
	var out []RemoteFile
	for _, f := range v.Files {
		rf := RemoteFile{
			Name:      f.Name,
			SizeBytes: int64(f.SizeKB * 1024),
			Format:    firstNonEmpty(f.Metadata.Format, f.Type),
			Primary:   f.Primary,
			// Version-level downloadUrl is the primary file; see civitaiListings.
			DownloadURL: f.DownloadURL,
		}
		if rf.DownloadURL == "" && f.Primary {
			rf.DownloadURL = v.DownloadURL
		}
		for k, hv := range f.Hashes {
			if strings.EqualFold(k, "SHA256") {
				rf.SHA256 = strings.ToUpper(hv)
				break
			}
		}
		out = append(out, rf)
	}
	return out, nil
}

// LatestVersion returns the newest version of a Civitai model.
//
// Used by update checking: given a model id recorded when a file was enriched,
// this is what the local copy is compared against.
// Two lines over FetchCivitaiModel rather than a second copy of the same
// request and decode: archive intake needs the raw body this one discards, and
// leaving both to fetch and parse /models/{id} independently is how they drift.
func (c *Client) LatestVersion(ctx context.Context, modelID string) (*Listing, error) {
	_, listings, _, err := c.FetchCivitaiModel(ctx, modelID)
	if err != nil || len(listings) == 0 {
		return nil, err
	}
	// The API returns versions newest-first; do not re-sort by date, because
	// publishedAt is null for drafts and would sort them to the wrong end.
	return &listings[0], nil
}

func (p *CivitaiProvider) client() *Client {
	if p.Client != nil {
		return p.Client
	}
	return NewClient()
}

// looseString renders a JSON value that may be a string or a number.
func looseString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}
