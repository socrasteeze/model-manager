package origin

// HuggingFace search.
//
// A note on hashes, because it changes what is possible here.
//
// huggingface.go says HuggingFace has no hash index, and for *reverse* lookup
// that is true: there is no by-hash endpoint, so a local file cannot be
// identified by asking HF what it is. But the tree endpoint exposes each file's
// LFS object id, and for LFS-backed files that oid *is* the SHA256 of the
// content. So the forward direction works: the hash is knowable before a single
// byte is downloaded.
//
// Two things fall out of that. Already-have detection is exact on HuggingFace
// too, not a filename guess. And a download can be checksum-verified against a
// hash obtained independently of the bytes, which is the property that makes
// verification meaningful rather than circular.
//
// Small non-LFS files (configs, tokenizers) carry a git blob SHA1 instead and
// are left without a hash rather than having a SHA1 recorded in a SHA256 field.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// CivArchiveBaseURL is declared here alongside the other roots; see
// civarchive.go for why it is treated as unverified.

// HuggingFaceProvider searches HuggingFace.
type HuggingFaceProvider struct{ Client *Client }

func (p *HuggingFaceProvider) ID() string          { return ProviderHuggingFaceID }
func (p *HuggingFaceProvider) DisplayName() string { return "HuggingFace" }

// hfSearchModel is one entry of the /api/models array.
type hfSearchModel struct {
	ID           string   `json:"id"`
	Author       string   `json:"author"`
	PipelineTag  string   `json:"pipeline_tag"`
	Tags         []string `json:"tags"`
	Downloads    int64    `json:"downloads"`
	Likes        int64    `json:"likes"`
	LibraryName  string   `json:"library_name"`
	CreatedAt    string   `json:"createdAt"`
	LastModified string   `json:"lastModified"`
	Private      bool     `json:"private"`
	Gated        any      `json:"gated"`
	CardData     struct {
		License        string   `json:"license"`
		BaseModel      any      `json:"base_model"`
		Tags           []string `json:"tags"`
		InstancePrompt string   `json:"instance_prompt"`
	} `json:"cardData"`
	Siblings []struct {
		Filename string `json:"rfilename"`
	} `json:"siblings"`
}

// hfTreeEntry is one entry of /api/models/{repo}/tree/{rev}.
type hfTreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	OID  string `json:"oid"`
	LFS  *struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	} `json:"lfs"`
}

// Search queries /api/models.
func (p *HuggingFaceProvider) Search(ctx context.Context, q Query) (*Page, error) {
	c := p.client()
	endpoint := c.huggingFaceBase() + "/models?" + hfSearchParams(q).Encode()

	raw, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("huggingface: search: %w", err)
	}
	if raw == nil {
		return &Page{}, nil
	}

	var models []hfSearchModel
	if err := json.Unmarshal(raw, &models); err != nil {
		return nil, fmt.Errorf("huggingface: decoding search response: %w", err)
	}

	page := &Page{}
	site := siteBase(c.huggingFaceBase())
	for _, m := range models {
		page.Items = append(page.Items, hfListing(m, site))
	}
	// HF pages by offset and does not report a total, so there is no way to
	// know this is the last page other than a short read.
	limit := hfLimit(q)
	if len(models) == limit {
		page.NextPage = maxInt(q.Page, 1) + 1
	}
	return page, nil
}

func hfSearchParams(q Query) url.Values {
	v := url.Values{}
	if q.Text != "" {
		v.Set("search", q.Text)
	}

	// HF filters are free-form repo tags rather than a controlled vocabulary,
	// so a type maps to the tag the community actually uses. A type with no
	// meaningful tag is left unfiltered rather than guessed at.
	for _, t := range q.Types {
		if tag := hfTypeFilter(t); tag != "" {
			v.Add("filter", tag)
		}
	}
	for _, b := range q.BaseModels {
		v.Add("filter", "base_model:"+b)
	}

	switch q.Sort {
	case SortDownloads:
		v.Set("sort", "downloads")
		v.Set("direction", "-1")
	case SortNewest:
		v.Set("sort", "createdAt")
		v.Set("direction", "-1")
	case SortUpdated:
		v.Set("sort", "lastModified")
		v.Set("direction", "-1")
	case SortRating:
		v.Set("sort", "likes")
		v.Set("direction", "-1")
	}

	limit := hfLimit(q)
	v.Set("limit", strconv.Itoa(limit))
	if q.Page > 1 {
		v.Set("skip", strconv.Itoa((q.Page-1)*limit))
	}
	// full=true is what populates siblings, giving a file list without a
	// second round trip per repo.
	v.Set("full", "true")
	return v
}

func hfLimit(q Query) int {
	if q.Limit <= 0 {
		return 24
	}
	if q.Limit > 100 {
		return 100
	}
	return q.Limit
}

func hfTypeFilter(t string) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "lora":
		return "lora"
	case "controlnet":
		return "controlnet"
	case "embedding":
		return "textual_inversion"
	case "checkpoint":
		return "text-to-image"
	default:
		return ""
	}
}

// hfListing converts a repo into a Listing.
//
// A HuggingFace repo has no version concept, so VersionID is the commit-less
// repo id and update detection for HF relies on lastModified rather than a
// version identity.
func hfListing(m hfSearchModel, site string) Listing {
	l := Listing{
		Provider:    ProviderHuggingFaceID,
		ID:          m.ID,
		Name:        repoName(m.ID),
		Author:      firstNonEmpty(m.Author, repoOwner(m.ID)),
		Type:        hfType(HFModel{Tags: m.Tags, PipelineTag: m.PipelineTag}),
		Downloads:   m.Downloads,
		Likes:       m.Likes,
		PublishedAt: m.CreatedAt,
		UpdatedAt:   m.LastModified,
		PageURL:     site + "/" + m.ID,
		Tags:        dedupeStrings(filterHFTags(append(append([]string{}, m.Tags...), m.CardData.Tags...))),
	}
	l.BaseModel = hfBaseModel(HFModel{
		Tags:     m.Tags,
		CardData: struct {
			License        string   `json:"license"`
			BaseModel      any      `json:"base_model"`
			Tags           []string `json:"tags"`
			InstancePrompt string   `json:"instance_prompt"`
		}{BaseModel: m.CardData.BaseModel},
	})
	if m.CardData.InstancePrompt != "" {
		l.TriggerWords = []string{m.CardData.InstancePrompt}
	}

	// Gated repos are reported as false, "auto" or "manual". Anything other
	// than false needs a token, and saying so up front beats a 401 later.
	gated := hfGated(m.Gated)

	for _, s := range m.Siblings {
		if !isModelWeightFile(s.Filename) {
			continue
		}
		l.Files = append(l.Files, RemoteFile{
			Name:         s.Filename,
			Format:       formatFromName(s.Filename),
			DownloadURL:  hfResolveURL(m.ID, s.Filename),
			RequiresAuth: gated || m.Private,
		})
	}
	return l
}

func hfGated(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != "" && !strings.EqualFold(t, "false")
	default:
		return false
	}
}

// Files lists a repo's weight files with sizes and SHA256s from the tree API.
func (p *HuggingFaceProvider) Files(ctx context.Context, l Listing) ([]RemoteFile, error) {
	c := p.client()
	repo := strings.Trim(l.ID, "/")
	if repo == "" {
		return nil, fmt.Errorf("huggingface: listing has no repo id")
	}

	// recursive=true because LoRAs are routinely published in a subdirectory,
	// and a non-recursive listing would report the repo as having no weights.
	endpoint := fmt.Sprintf("%s/models/%s/tree/main?recursive=true", c.huggingFaceBase(), repo)
	raw, _, err := c.getJSON(ctx, endpoint)
	if err != nil {
		return nil, fmt.Errorf("huggingface: listing %s: %w", repo, err)
	}
	if raw == nil {
		return l.Files, nil
	}

	var entries []hfTreeEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("huggingface: decoding tree for %s: %w", repo, err)
	}

	requiresAuth := false
	for _, f := range l.Files {
		if f.RequiresAuth {
			requiresAuth = true
			break
		}
	}

	var out []RemoteFile
	for _, e := range entries {
		if e.Type != "file" || !isModelWeightFile(e.Path) {
			continue
		}
		rf := RemoteFile{
			Name:         e.Path,
			SizeBytes:    e.Size,
			Format:       formatFromName(e.Path),
			DownloadURL:  hfResolveURL(repo, e.Path),
			RequiresAuth: requiresAuth,
		}
		// Only an LFS oid is a SHA256. A plain git oid is a SHA1 over a blob
		// header, which would be actively wrong to record here.
		if e.LFS != nil && len(e.LFS.OID) == 64 {
			rf.SHA256 = strings.ToUpper(e.LFS.OID)
			if e.LFS.Size > 0 {
				rf.SizeBytes = e.LFS.Size
			}
		}
		out = append(out, rf)
	}
	if len(out) == 0 {
		return l.Files, nil
	}
	return out, nil
}

// hfResolveURL builds the download URL for a file.
func hfResolveURL(repo, path string) string {
	var escaped []string
	for _, seg := range strings.Split(path, "/") {
		escaped = append(escaped, url.PathEscape(seg))
	}
	return fmt.Sprintf("https://huggingface.co/%s/resolve/main/%s?download=true",
		repo, strings.Join(escaped, "/"))
}

// isModelWeightFile keeps the file list to things worth downloading as a model.
//
// A repo often contains dozens of configs, tokenizers and images; listing them
// as downloadable candidates would bury the one file the user came for.
func isModelWeightFile(name string) bool {
	switch strings.ToLower(pathExt(name)) {
	case ".safetensors", ".gguf", ".ckpt", ".pt", ".pth", ".bin":
		return true
	}
	return false
}

func formatFromName(name string) string {
	switch strings.ToLower(pathExt(name)) {
	case ".safetensors":
		return "SafeTensor"
	case ".gguf":
		return "GGUF"
	case ".ckpt", ".pt", ".pth", ".bin":
		return "PickleTensor"
	}
	return ""
}

func pathExt(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i:]
	}
	return ""
}

func repoOwner(id string) string {
	if i := strings.Index(id, "/"); i > 0 {
		return id[:i]
	}
	return ""
}

func (p *HuggingFaceProvider) client() *Client {
	if p.Client != nil {
		return p.Client
	}
	return NewClient()
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
