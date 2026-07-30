package origin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/socrasteeze/model-manager/internal/provenance"
	"github.com/socrasteeze/model-manager/internal/store"
)

// CivitaiBaseURL is the default API root. Overridable for testing, and at
// runtime per-client via MM_CIVITAI_API.
//
// Civitai serves the same API over several hosts that differ in which catalogue
// they expose: civitai.com is the main site, civitai.red is the adult split and
// civitai.green the safe-for-work one. Which host you point at therefore decides
// what a search can return, so this is a setting rather than a constant.
//
// Pointing directly at the right host also matters for authentication. Go strips
// the Authorization header when a redirect crosses to a different registered
// domain (net/http.Client.makeHeadersCopier), so reaching .red by way of a .com
// redirect would silently drop the API key and fail as a bare 401 with nothing
// to explain it.
var CivitaiBaseURL = "https://civitai.com/api/v1"

// ErrRateLimited means the server asked us to slow down.
var ErrRateLimited = errors.New("origin: rate limited")

// Client talks to an origin provider.
type Client struct {
	HTTP *http.Client

	// APIKey authenticates for models gated behind a Civitai login. Optional for
	// metadata, required for some downloads.
	APIKey string

	// HFToken authenticates to HuggingFace. Only needed for gated or private
	// repos; public metadata and downloads work without one.
	HFToken string

	// Per-client API roots. Empty means "use the package default", which is what
	// keeps the existing test overrides of the package vars working.
	CivitaiBase     string
	HuggingFaceBase string
	CivArchiveBase  string

	// MinInterval throttles requests. 19k lookups against a public API is a
	// volume that gets an IP blocked if it arrives all at once (spec §12).
	MinInterval time.Duration

	// MaxRetries bounds the backoff loop.
	MaxRetries int

	UserAgent string

	lastRequest time.Time
}

// NewClient returns a client with sane defaults.
//
// Credentials and API roots are read from the environment rather than stored in
// the database. A token in the master DB would end up in every backup and every
// copy of the library; leaving it in the environment keeps the secret owned by
// the shell that launched the process.
func NewClient() *Client {
	return &Client{
		HTTP:            &http.Client{Timeout: 30 * time.Second},
		MinInterval:     350 * time.Millisecond,
		MaxRetries:      4,
		UserAgent:       "model-manager/1.0 (+https://github.com/socrasteeze/model-manager)",
		APIKey:          os.Getenv("CIVITAI_API_KEY"),
		HFToken:         firstEnv("HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"),
		CivitaiBase:     os.Getenv("MM_CIVITAI_API"),
		HuggingFaceBase: os.Getenv("MM_HUGGINGFACE_API"),
		CivArchiveBase:  os.Getenv("MM_CIVARCHIVE_API"),
	}
}

// civitaiBase resolves the Civitai API root for this client.
func (c *Client) civitaiBase() string {
	return strings.TrimRight(orDefaultStr(c.CivitaiBase, CivitaiBaseURL), "/")
}

// huggingFaceBase resolves the HuggingFace API root for this client.
func (c *Client) huggingFaceBase() string {
	return strings.TrimRight(orDefaultStr(c.HuggingFaceBase, HuggingFaceBaseURL), "/")
}

// civArchiveBase resolves the CivArchive API root for this client.
func (c *Client) civArchiveBase() string {
	return strings.TrimRight(orDefaultStr(c.CivArchiveBase, CivArchiveBaseURL), "/")
}

func orDefaultStr(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// CivitaiVersion is the subset of the model-version response worth typing.
//
// The full body is archived separately and verbatim, so this struct being
// incomplete costs nothing: a field added upstream is already preserved and can
// be extracted later without re-fetching (§12.1).
type CivitaiVersion struct {
	ID           int64    `json:"id"`
	ModelID      int64    `json:"modelId"`
	Name         string   `json:"name"`
	BaseModel    string   `json:"baseModel"`
	Description  string   `json:"description"`
	TrainedWords []string `json:"trainedWords"`
	DownloadURL  string   `json:"downloadUrl"`

	Model struct {
		Name        string   `json:"name"`
		Type        string   `json:"type"`
		NSFW        *bool    `json:"nsfw"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
	} `json:"model"`

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
		URL    string `json:"url"`
		NSFW   any    `json:"nsfw"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"images"`
}

// LookupCivitaiByHash fetches a model version by file hash.
//
// The hash that identifies the file locally is the same key Civitai indexes by,
// which is the bonus §2 calls out: no name matching, no guessing, no fuzzy
// resolution that could bind the wrong record.
func (c *Client) LookupCivitaiByHash(ctx context.Context, sha256 string) (json.RawMessage, int, error) {
	url := fmt.Sprintf("%s/model-versions/by-hash/%s", c.civitaiBase(), strings.ToUpper(sha256))
	return c.getJSON(ctx, url)
}

// getJSON performs a throttled, retried GET.
func (c *Client) getJSON(ctx context.Context, url string) (json.RawMessage, int, error) {
	var lastStatus int

	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		if err := c.throttle(ctx); err != nil {
			return nil, 0, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.UserAgent)
		// Scoped by host: see auth.go. Never send one provider's key to another.
		if token := c.tokenFor(url); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, 0, ctx.Err()
			}
			// A transport error is usually transient; back off and try again.
			if attempt == c.MaxRetries {
				return nil, 0, fmt.Errorf("origin: fetching %s: %w", url, err)
			}
			if waitErr := sleepCtx(ctx, backoffFn(attempt)); waitErr != nil {
				return nil, 0, waitErr
			}
			continue
		}

		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		lastStatus = resp.StatusCode

		switch {
		case resp.StatusCode == http.StatusNotFound:
			// A definite answer, not a failure. The caller caches it.
			return nil, resp.StatusCode, nil

		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			if attempt == c.MaxRetries {
				return nil, lastStatus, fmt.Errorf("%w: %s returned %d", ErrRateLimited, url, resp.StatusCode)
			}
			// Honour Retry-After when the server sends it; the server knows
			// better than any backoff curve we would invent.
			wait := backoffFn(attempt)
			if hint := retryAfter(resp.Header.Get("Retry-After")); hint > 0 {
				wait = hint
			}
			if err := sleepCtx(ctx, wait); err != nil {
				return nil, lastStatus, err
			}
			continue

		case resp.StatusCode >= 400:
			return nil, lastStatus, fmt.Errorf("origin: %s returned %d", url, resp.StatusCode)
		}

		if readErr != nil {
			return nil, lastStatus, fmt.Errorf("origin: reading %s: %w", url, readErr)
		}
		if !json.Valid(body) {
			return nil, lastStatus, fmt.Errorf("origin: %s returned invalid JSON", url)
		}
		return json.RawMessage(body), lastStatus, nil
	}
	return nil, lastStatus, fmt.Errorf("%w: gave up on %s", ErrRateLimited, url)
}

func (c *Client) throttle(ctx context.Context) error {
	if c.MinInterval <= 0 {
		return nil
	}
	elapsed := time.Since(c.lastRequest)
	if elapsed < c.MinInterval {
		if err := sleepCtx(ctx, c.MinInterval-elapsed); err != nil {
			return err
		}
	}
	c.lastRequest = time.Now()
	return nil
}

// backoffFn is indirected so tests can collapse the wait without changing the
// production curve.
var backoffFn = backoff

// backoff grows exponentially from one second.
func backoff(attempt int) time.Duration {
	d := time.Second << attempt
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return d
}

func retryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds >= 0 {
		if seconds > 300 {
			seconds = 300
		}
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		if d := time.Until(when); d > 0 && d < 5*time.Minute {
			return d
		}
	}
	return 0
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ObservationsFromCivitai extracts typed fields from an archived response.
//
// Separate from fetching on purpose: this runs over the stored blob, so
// improving the extraction never costs another API call, and the 19k-lookup
// budget is spent exactly once (§12.1).
func ObservationsFromCivitai(raw json.RawMessage) ([]store.FieldObservation, []string, map[string]string, []string) {
	var v CivitaiVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, nil, nil, nil
	}

	var obs []store.FieldObservation
	add := func(field string, value any) {
		if s, ok := value.(string); ok && strings.TrimSpace(s) == "" {
			return
		}
		if ss, ok := value.([]string); ok && len(ss) == 0 {
			return
		}
		obs = append(obs, store.FieldObservation{Field: field, Value: value})
	}

	add(provenance.FieldName, v.Model.Name)
	add(provenance.FieldVersion, v.Name)
	add(provenance.FieldBaseModel, normalizeCivitaiBase(v.BaseModel))
	add(provenance.FieldType, normalizeCivitaiType(v.Model.Type))
	add(provenance.FieldTriggerWords, cleanList(v.TrainedWords))
	add(provenance.FieldOrigin, "civitai")

	description := firstNonEmpty(v.Description, v.Model.Description)
	add(provenance.FieldDescription, stripHTML(description))

	if v.Model.NSFW != nil {
		add(provenance.FieldNSFW, *v.Model.NSFW)
	}

	// Hashes come from the primary file, or the only one if none is flagged.
	hashes := map[string]string{}
	for _, f := range v.Files {
		if f.Primary || len(v.Files) == 1 {
			for k, val := range f.Hashes {
				hashes[k] = val
			}
			break
		}
	}

	var imageURLs []string
	for _, img := range v.Images {
		if img.URL != "" {
			imageURLs = append(imageURLs, img.URL)
		}
	}

	return obs, cleanList(v.Model.Tags), hashes, imageURLs
}

func normalizeCivitaiType(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "lora":
		return "lora"
	case "locon", "dora":
		return "lycoris"
	case "checkpoint":
		return "checkpoint"
	case "textualinversion":
		return "embedding"
	case "vae":
		return "vae"
	case "controlnet":
		return "controlnet"
	case "upscaler":
		return "upscaler"
	}
	return ""
}

// normalizeCivitaiBase collapses Civitai's spellings onto this app's vocabulary.
// Pony and Illustrious are checked first because their official names contain
// "XL", and classifying them as SDXL would merge three buckets that behave
// nothing alike in practice.
func normalizeCivitaiBase(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	switch {
	case s == "":
		return ""
	case strings.Contains(s, "pony"):
		return "Pony"
	case strings.Contains(s, "illustrious"):
		return "Illustrious"
	case strings.Contains(s, "noobai"):
		return "NoobAI"
	case strings.Contains(s, "sdxl") || strings.Contains(s, "sd xl"):
		return "SDXL"
	case strings.Contains(s, "flux"):
		return "Flux"
	case strings.Contains(s, "sd 3") || strings.Contains(s, "sd3"):
		return "SD 3"
	case strings.Contains(s, "sd 1"):
		return "SD 1.5"
	case strings.Contains(s, "sd 2"):
		return "SD 2.x"
	}
	return strings.TrimSpace(v)
}

func cleanList(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		t := strings.TrimSpace(s)
		if t == "" || seen[t] || len(t) > 200 {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// stripHTML removes the markup Civitai descriptions carry, which would otherwise
// render as literal tags or be injected into the UI.
func stripHTML(s string) string {
	stripped := s
	if strings.ContainsRune(s, '<') {
		var b strings.Builder
		depth := 0
		for _, r := range s {
			switch {
			case r == '<':
				depth++
			case r == '>':
				if depth > 0 {
					depth--
				}
			case depth == 0:
				b.WriteRune(r)
			}
		}
		stripped = b.String()
	}
	out := strings.NewReplacer(
		"&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
	).Replace(stripped)
	return strings.TrimSpace(strings.Join(strings.Fields(out), " "))
}
