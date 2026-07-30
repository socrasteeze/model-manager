package origin

// CivArchive.
//
// CivArchive mirrors Civitai records, including ones that have been removed
// upstream. That makes it the provider that matters most for this project's
// stated purpose: a model taken down from Civitai has metadata that exists
// nowhere else, and §12.1 already treats the local archive as possibly the last
// surviving copy. CivArchive is the one place a *missing* record can still be
// recovered from.
//
// UNVERIFIED ENDPOINTS
//
// The exact request and response shapes below could not be confirmed against
// the live service: the environment this was written in blocks outbound
// connections to civarchive.com, so nothing here has been exercised against a
// real response. The mapping is therefore written defensively --
//
//   - every endpoint path is a template in one place (civArchivePaths), so
//     correcting one is a single-line change rather than a rewrite;
//   - the decoder accepts several plausible envelopes and field spellings
//     rather than binding to one guess;
//   - a shape that does not match yields zero results and a clear error rather
//     than a panic or silently empty output.
//
// Run `mm browse --provider civarchive --json <query>` against the real service
// to confirm, and correct civArchivePaths if the paths differ.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// CivArchiveBaseURL is the default API root, overridable via MM_CIVARCHIVE_API.
var CivArchiveBaseURL = "https://civarchive.com/api"

// civArchivePaths holds every endpoint template. See the UNVERIFIED note above.
var civArchivePaths = struct {
	// Search takes the query string; %s is the already-encoded parameters.
	Search string
	// Model takes a Civitai model id.
	Model string
	// ByHash takes an uppercase SHA256.
	ByHash string
}{
	Search: "/search?%s",
	Model:  "/models/%s",
	ByHash: "/models/hash/%s",
}

// CivArchiveProvider searches CivArchive.
type CivArchiveProvider struct{ Client *Client }

func (p *CivArchiveProvider) ID() string          { return ProviderCivArchiveID }
func (p *CivArchiveProvider) DisplayName() string { return "CivArchive" }

// caRecord is a deliberately loose view of an archived record.
//
// Field aliases are listed because the mirror may keep Civitai's own spelling
// (modelId, baseModel) or a normalized one. Decoding into a tolerant struct
// costs nothing and avoids an all-or-nothing bet on which it is.
type caRecord struct {
	ID        json.Number `json:"id"`
	ModelID   json.Number `json:"modelId"`
	ModelIDAlt json.Number `json:"model_id"`
	VersionID json.Number `json:"versionId"`
	VersionIDAlt json.Number `json:"modelVersionId"`

	Name        string `json:"name"`
	ModelName   string `json:"modelName"`
	VersionName string `json:"versionName"`

	Type      string `json:"type"`
	BaseModel string `json:"baseModel"`
	BaseModelAlt string `json:"base_model"`

	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	NSFW        bool     `json:"nsfw"`

	Creator  string `json:"creator"`
	Username string `json:"username"`

	TrainedWords []string `json:"trainedWords"`

	PublishedAt string `json:"publishedAt"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`

	DeletedAt string `json:"deletedAt"`

	Images []struct {
		URL string `json:"url"`
	} `json:"images"`

	Files []caFile `json:"files"`
}

type caFile struct {
	Name        string  `json:"name"`
	SizeKB      float64 `json:"sizeKB"`
	SizeBytes   int64   `json:"sizeBytes"`
	Format      string  `json:"format"`
	Type        string  `json:"type"`
	Primary     bool    `json:"primary"`
	SHA256      string  `json:"sha256"`
	DownloadURL string  `json:"downloadUrl"`
	URL         string  `json:"url"`

	Hashes map[string]string `json:"hashes"`
}

// Search queries the archive.
func (p *CivArchiveProvider) Search(ctx context.Context, q Query) (*Page, error) {
	c := p.client()

	v := url.Values{}
	if q.Text != "" {
		v.Set("query", q.Text)
	}
	if len(q.Types) > 0 {
		if native := civitaiType(q.Types[0]); native != "" {
			v.Set("type", native)
		}
	}
	if len(q.BaseModels) > 0 {
		v.Set("baseModel", q.BaseModels[0])
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 24
	}
	v.Set("limit", strconv.Itoa(limit))
	if q.Page > 1 {
		v.Set("page", strconv.Itoa(q.Page))
	}

	endpoint := c.civArchiveBase() + fmt.Sprintf(civArchivePaths.Search, v.Encode())
	raw, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("civarchive: search: %w", err)
	}
	if raw == nil {
		return &Page{}, nil
	}

	records, total, err := decodeCivArchive(raw)
	if err != nil {
		return nil, err
	}

	page := &Page{Total: total}
	site := siteBase(c.civArchiveBase())
	for _, r := range records {
		page.Items = append(page.Items, caListing(r, site))
	}
	if len(page.Items) == limit {
		page.NextPage = maxInt(q.Page, 1) + 1
	}
	return page, nil
}

// decodeCivArchive unwraps whichever envelope the service uses.
//
// Three shapes are accepted: a bare array, {items:[...]} as Civitai uses, and
// {results:[...]} / {data:[...]} as search services commonly use. Trying each
// is cheap; guessing wrong and reporting "no results" for a working service is
// the failure worth avoiding.
func decodeCivArchive(raw json.RawMessage) ([]caRecord, int, error) {
	var asArray []caRecord
	if err := json.Unmarshal(raw, &asArray); err == nil && asArray != nil {
		return asArray, 0, nil
	}

	var env struct {
		Items    []caRecord `json:"items"`
		Results  []caRecord `json:"results"`
		Data     []caRecord `json:"data"`
		Models   []caRecord `json:"models"`
		Total    int        `json:"total"`
		Metadata struct {
			TotalItems int `json:"totalItems"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0, fmt.Errorf("civarchive: unrecognized response shape: %w", err)
	}

	total := env.Total
	if total == 0 {
		total = env.Metadata.TotalItems
	}
	for _, list := range [][]caRecord{env.Items, env.Results, env.Data, env.Models} {
		if len(list) > 0 {
			return list, total, nil
		}
	}
	return nil, total, nil
}

// caListing converts an archived record to a Listing.
func caListing(r caRecord, site string) Listing {
	modelID := firstNonEmptyNum(r.ModelID, r.ModelIDAlt, r.ID)
	versionID := firstNonEmptyNum(r.VersionIDAlt, r.VersionID)

	l := Listing{
		Provider:     ProviderCivArchiveID,
		ID:           modelID,
		VersionID:    versionID,
		VersionName:  r.VersionName,
		Name:         firstNonEmpty(r.ModelName, r.Name),
		Author:       firstNonEmpty(r.Creator, r.Username),
		Type:         civitaiTypeToLocal(r.Type),
		BaseModel:    normalizeCivitaiBase(firstNonEmpty(r.BaseModel, r.BaseModelAlt)),
		Description:  stripHTML(r.Description),
		Tags:         dedupeStrings(r.Tags),
		NSFW:         r.NSFW,
		PublishedAt:  firstNonEmpty(r.PublishedAt, r.CreatedAt),
		UpdatedAt:    firstNonEmpty(r.UpdatedAt, r.PublishedAt, r.CreatedAt),
		TriggerWords: dedupeStrings(r.TrainedWords),
	}
	if len(r.Images) > 0 {
		l.ThumbnailURL = r.Images[0].URL
	}
	if modelID != "" {
		l.PageURL = site + "/models/" + modelID
	}

	// A record with a deletion date is the whole point of this provider: it is
	// metadata for something no longer obtainable from Civitai at all.
	if r.DeletedAt != "" {
		l.Description = strings.TrimSpace("Removed from Civitai on " +
			r.DeletedAt[:minInt(10, len(r.DeletedAt))] + ". " + l.Description)
	}

	for _, f := range r.Files {
		rf := RemoteFile{
			Name:        f.Name,
			SizeBytes:   f.SizeBytes,
			Format:      firstNonEmpty(f.Format, f.Type),
			Primary:     f.Primary,
			DownloadURL: firstNonEmpty(f.DownloadURL, f.URL),
			SHA256:      strings.ToUpper(f.SHA256),
		}
		if rf.SizeBytes == 0 && f.SizeKB > 0 {
			rf.SizeBytes = int64(f.SizeKB * 1024)
		}
		if rf.SHA256 == "" {
			for k, hv := range f.Hashes {
				if strings.EqualFold(k, "SHA256") {
					rf.SHA256 = strings.ToUpper(hv)
					break
				}
			}
		}
		l.Files = append(l.Files, rf)
	}
	return l
}

// Files returns the archived record's files.
func (p *CivArchiveProvider) Files(ctx context.Context, l Listing) ([]RemoteFile, error) {
	if len(l.Files) > 0 {
		return l.Files, nil
	}
	if l.ID == "" {
		return nil, fmt.Errorf("civarchive: listing has no model id")
	}
	c := p.client()
	endpoint := c.civArchiveBase() + fmt.Sprintf(civArchivePaths.Model, url.PathEscape(l.ID))
	raw, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("civarchive: fetching %s: %w", l.ID, err)
	}
	if raw == nil {
		return nil, nil
	}
	records, _, err := decodeCivArchive(raw)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		// A single record rather than a collection.
		var one caRecord
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, fmt.Errorf("civarchive: decoding %s: %w", l.ID, err)
		}
		records = []caRecord{one}
	}
	return caListing(records[0], siteBase(c.civArchiveBase())).Files, nil
}

// LookupCivArchiveByHash asks the archive to identify a hash.
//
// This is the recovery path for a local file that Civitai no longer knows
// about: the by-hash lookup there returns 404 forever, while the archive may
// still hold the record. Callers cache the result under the civarchive provider
// so a found record is preserved with the same never-overwritten-by-a-404
// guarantee as everything else in the archive (§12.1).
func (c *Client) LookupCivArchiveByHash(ctx context.Context, sha256 string) (json.RawMessage, int, error) {
	endpoint := c.civArchiveBase() +
		fmt.Sprintf(civArchivePaths.ByHash, strings.ToUpper(sha256))
	return c.getJSON(ctx, endpoint)
}

// ObservationsFromCivArchive extracts typed fields from an archived record.
//
// Recorded at the origin tier like Civitai, because it is the same data from a
// mirror. It ranks below Civitai in the same-tier trust order: where both
// answer, the live service is the more current of the two.
func ObservationsFromCivArchive(raw json.RawMessage) ([]FieldObservationSet, error) {
	records, _, err := decodeCivArchive(raw)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		var one caRecord
		if err := json.Unmarshal(raw, &one); err != nil {
			return nil, err
		}
		records = []caRecord{one}
	}
	var out []FieldObservationSet
	for _, r := range records {
		l := caListing(r, siteBase(CivArchiveBaseURL))
		out = append(out, FieldObservationSet{Listing: l})
	}
	return out, nil
}

// FieldObservationSet pairs a decoded listing with the fields it implies.
type FieldObservationSet struct {
	Listing Listing
}

func (p *CivArchiveProvider) client() *Client {
	if p.Client != nil {
		return p.Client
	}
	return NewClient()
}

func firstNonEmptyNum(vals ...json.Number) string {
	for _, v := range vals {
		s := v.String()
		if s != "" && s != "0" {
			return s
		}
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
