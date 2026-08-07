package origin

// The per-version image gallery.
//
// The version body carries the images its uploader attached, which is what
// enrichment has always used. A model's gallery is larger than that, and for an
// archive the difference matters: once a model is taken down its images go with
// it, no mirror reproduces them, and a preview is reconstructible from nothing
// else.
//
// The response shape here could not be verified against the live service, so it
// is treated the way civarchive.go treats its endpoints: one tolerant template
// struct, a defensive decode, and zero images with a named error on a mismatch
// rather than a confident empty result.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

const (
	// DefaultPreviewLimit is how many images an intake takes by default.
	//
	// Twelve, and the page size is set to match, so the ordinary intake costs
	// exactly one extra request per version. Each page is one slot of the
	// throttle shared with browse, enrichment and the update sweep, so
	// pagination exists for an operator who raises the cap rather than as the
	// normal path.
	DefaultPreviewLimit = 12

	// MaxPreviewPages bounds the walk regardless of the limit, so a
	// misconfiguration cannot page through a gallery of thousands.
	MaxPreviewPages = 5
)

// GalleryImage is one image from the gallery endpoint.
type GalleryImage struct {
	ID  string
	URL string

	// NSFWLevel is carried through unparsed. Providers spell this several ways
	// and none of them is worth a typed enum here.
	NSFWLevel string
}

// galleryResponse is the tolerant template. Every field is optional: a shape
// this does not recognise decodes to an empty page rather than an error the
// caller cannot act on.
type galleryResponse struct {
	Items []struct {
		ID        json.Number `json:"id"`
		URL       string      `json:"url"`
		NSFWLevel string      `json:"nsfwLevel"`
	} `json:"items"`
	Metadata struct {
		NextPage string `json:"nextPage"`
	} `json:"metadata"`
}

// GalleryImages returns up to limit images for a model version.
//
// nsfw is derived by the caller from the model's own flag rather than
// configured. If the model is marked adult its gallery is on-topic; if it is
// not, the parameter is omitted entirely and the server's default applies --
// sending a restrictive value would drop every image whose flag was never set,
// the same tri-state trap the upstream provider avoids for search.
func (c *Client) GalleryImages(ctx context.Context, versionID string, limit int, nsfw bool) ([]GalleryImage, error) {
	if strings.TrimSpace(versionID) == "" {
		return nil, fmt.Errorf("origin: no version id given")
	}
	if limit <= 0 {
		limit = DefaultPreviewLimit
	}
	pageSize := limit
	if pageSize > 100 {
		pageSize = 100
	}

	var out []GalleryImage
	seen := map[string]bool{}

	for page := 1; page <= MaxPreviewPages && len(out) < limit; page++ {
		v := url.Values{}
		v.Set("modelVersionId", versionID)
		v.Set("limit", strconv.Itoa(pageSize))
		v.Set("page", strconv.Itoa(page))
		if nsfw {
			v.Set("nsfw", "true")
		}

		raw, _, err := c.getJSON(ctx, c.civitaiBase()+"/images?"+v.Encode())
		if err != nil {
			return out, fmt.Errorf("origin: gallery for version %s: %w", versionID, err)
		}
		if raw == nil {
			break
		}

		var body galleryResponse
		if err := json.Unmarshal(raw, &body); err != nil {
			return out, fmt.Errorf("origin: gallery for version %s returned an unexpected shape: %w",
				versionID, err)
		}
		if len(body.Items) == 0 {
			break
		}
		for _, it := range body.Items {
			if it.URL == "" {
				continue
			}
			key := imageKey(it.ID.String(), it.URL)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, GalleryImage{ID: it.ID.String(), URL: it.URL, NSFWLevel: it.NSFWLevel})
			if len(out) >= limit {
				break
			}
		}
		if body.Metadata.NextPage == "" && len(body.Items) < pageSize {
			break
		}
	}
	return out, nil
}

// FetchImage downloads one preview image.
//
// Exported so archive intake can stage preview bytes before there is a
// model_file row to attach them to -- the case enrichment never has, because it
// only ever runs against files that are already indexed. Capped at
// blobstore.MaxBlobBytes and host-scoped for credentials, exactly as the
// enrichment path is.
func (c *Client) FetchImage(ctx context.Context, imageURL string) ([]byte, error) {
	return c.fetchBytes(ctx, imageURL)
}

// StillURL asks for a width-transformed still of a preview.
//
// Two rejections sit between an animated preview and the blob store: the MIME
// sniff refuses anything that is not an image regardless of size, and the 32 MiB
// cap refuses what is left. Requesting the transformed still converts most
// oversize cases into something storable, which is the difference between an
// archive that has the picture and one that records a gap.
//
// A provider-specific URL convention, hence the guard: a URL that does not carry
// one is returned unchanged rather than rewritten into a guess.
func StillURL(rawURL string, width int) string {
	if width <= 0 || !widthTransform.MatchString(rawURL) {
		return rawURL
	}
	return widthTransform.ReplaceAllString(rawURL, "/width="+strconv.Itoa(width))
}

// widthTransform matches the size segment Civitai puts in an image URL.
var widthTransform = regexp.MustCompile(`/(width|height|original)=[^/]+`)

// imageKey identifies an image across the two places it can be listed.
//
// The provider serves one picture at several widths under a single id, so two
// URLs that differ only in a /width=NNN/ segment are the same image -- and a
// raw-URL comparison would store it three times. The id is preferred where both
// sides carry one; the stripped path is the fallback for the version body, which
// lists URLs without ids.
func imageKey(id, rawURL string) string {
	if id != "" && id != "0" {
		return "id:" + id
	}
	stripped := widthTransform.ReplaceAllString(rawURL, "")
	if i := strings.LastIndex(stripped, "/"); i >= 0 {
		return "path:" + stripped[i+1:]
	}
	return "path:" + stripped
}

// MergePreviewURLs combines the version body's images with the gallery's,
// deduplicated, uploader order first.
//
// The version body comes first deliberately: that is the order the uploader
// chose, and it is what preview_image.position should reflect. The gallery is
// additional coverage, not a replacement.
func MergePreviewURLs(bodyURLs []string, gallery []GalleryImage, limit int) []string {
	if limit <= 0 {
		limit = DefaultPreviewLimit
	}
	seen := map[string]bool{}
	out := make([]string, 0, limit)

	add := func(id, u string) {
		if u == "" || len(out) >= limit {
			return
		}
		key := imageKey(id, u)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, u)
	}

	for _, u := range bodyURLs {
		add("", u)
	}
	for _, g := range gallery {
		add(g.ID, g.URL)
	}
	return out
}
