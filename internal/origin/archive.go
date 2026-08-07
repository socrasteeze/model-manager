package origin

// Deliberate archive intake.
//
// Enrichment asks "what is this file I already have?", keyed by a hash computed
// locally. This asks the other direction: fetch this model FROM the provider,
// completely enough that the provider can vanish and nothing is lost.
//
// The whole reason this file exists rather than reusing CivitaiProvider.Files is
// the raw body. Every existing ID-keyed fetcher unmarshals a response and throws
// the JSON away, which is right for a browse -- a Listing is a claim about a
// file that does not exist here yet, and nothing about it is worth keeping --
// and exactly wrong for an intake, whose premise is §12.1's: for a taken-down
// model, this local copy may be the only one left.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Key spaces inside origin_cache.
//
// lookup_key is a content hash for the three public provider values, and every
// row written before archive intake existed is one. An intake caches bodies
// keyed by a provider's own model and version ids -- a different key space
// entirely -- and loadOwnedVersions reads lookup_key AS a hash for every found
// civitai or civarchive row. An id-keyed row filed under those providers would
// inject an ownedVersion whose SHA256 is the literal string "67890", carrying a
// real version id alongside it, and the library would then claim to own a model
// nobody has downloaded.
//
// A separate provider value rather than a key prefix, because every query that
// reads this table already filters on provider and none filters on the shape of
// the key. The exclusion is therefore structural: a row can only ever reach a
// query that names its provider. A prefix would have needed a length or LIKE
// predicate added to four existing queries and remembered by every future one.
const (
	ProviderCivitaiVersionID  = "civitai-version"
	ProviderCivitaiModelID    = "civitai-model"
	ProviderCivArchiveModelID = "civarchive-model"
)

// FetchCivitaiVersion returns a model version's raw body alongside its decoded
// form.
//
// status is returned rather than folded into err because 404 is a definite
// answer here, not a failure: it is the takedown signal, and the caller records
// it rather than retrying.
func (c *Client) FetchCivitaiVersion(ctx context.Context, versionID string) (json.RawMessage, *CivitaiVersion, int, error) {
	if strings.TrimSpace(versionID) == "" {
		return nil, nil, 0, fmt.Errorf("origin: no version id given")
	}
	raw, status, err := c.getJSON(ctx, c.civitaiBase()+"/model-versions/"+url.PathEscape(versionID))
	if err != nil {
		return nil, nil, status, fmt.Errorf("origin: fetching version %s: %w", versionID, err)
	}
	if raw == nil {
		return nil, nil, status, nil
	}
	var v CivitaiVersion
	if err := json.Unmarshal(raw, &v); err != nil {
		// The raw body is still returned. It is the thing worth keeping, and a
		// shape this build cannot decode is exactly the case where archiving the
		// bytes matters most -- a later build can read what this one could not.
		return raw, nil, status, fmt.Errorf("origin: decoding version %s: %w", versionID, err)
	}
	return raw, &v, status, nil
}

// FetchCivitaiModel returns a model's raw body alongside its versions as
// listings, newest first.
func (c *Client) FetchCivitaiModel(ctx context.Context, modelID string) (json.RawMessage, []Listing, int, error) {
	if strings.TrimSpace(modelID) == "" {
		return nil, nil, 0, fmt.Errorf("origin: no model id given")
	}
	raw, status, err := c.getJSON(ctx, c.civitaiBase()+"/models/"+url.PathEscape(modelID))
	if err != nil {
		return nil, nil, status, err
	}
	if raw == nil {
		return nil, nil, status, nil
	}
	var m civitaiSearchModel
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, nil, status, fmt.Errorf("origin: decoding model %s: %w", modelID, err)
	}
	return raw, civitaiListings(m), status, nil
}

// ArchiveVersionBody fetches and archives a version's raw response.
//
// Returns the body, the decoded form, and whether the provider still has it.
// The archive write happens before anything is decoded from it, because the
// bytes are the part that has to survive: a decode this build gets wrong can be
// redone, a body nobody kept cannot.
func (c *Client) ArchiveVersionBody(ctx context.Context, cache *Cache, modelID, versionID string) (json.RawMessage, *CivitaiVersion, bool, error) {
	raw, v, status, err := c.FetchCivitaiVersion(ctx, versionID)
	if raw == nil {
		if err != nil {
			return nil, nil, false, err
		}
		// A definite 404. Recorded as a miss under the version key space so a
		// re-run does not re-ask, and reported as gone so the caller can stamp
		// the item.
		if cache != nil {
			if putErr := cache.PutMissing(ProviderCivitaiVersionID, versionID, status); putErr != nil {
				return nil, nil, false, putErr
			}
		}
		return nil, nil, false, nil
	}
	if cache != nil {
		if putErr := cache.PutFound(ProviderCivitaiVersionID, versionID, raw, status); putErr != nil {
			return raw, v, true, putErr
		}
	}
	// err here is a decode failure with the body already safely archived, which
	// the caller reports as an incompleteness rather than a lost intake.
	return raw, v, true, err
}

// ArchiveModelBody fetches and archives a model's raw response.
//
// The model body carries the version list, the author and the tag set, none of
// which the version body repeats in full -- so a takedown that left only the
// version response would lose the context around it.
func (c *Client) ArchiveModelBody(ctx context.Context, cache *Cache, modelID string) (json.RawMessage, []Listing, bool, error) {
	raw, listings, status, err := c.FetchCivitaiModel(ctx, modelID)
	if raw == nil {
		if err != nil {
			return nil, nil, false, err
		}
		if cache != nil {
			if putErr := cache.PutMissing(ProviderCivitaiModelID, modelID, status); putErr != nil {
				return nil, nil, false, putErr
			}
		}
		return nil, nil, false, nil
	}
	if cache != nil {
		if putErr := cache.PutFound(ProviderCivitaiModelID, modelID, raw, status); putErr != nil {
			return raw, listings, true, putErr
		}
	}
	return raw, listings, true, err
}

// IsRateLimit reports whether an error means the provider asked us to slow down.
//
// Exported because a run that hits the limit must stop rather than push on --
// everything already recorded stays recorded, and the re-run resumes from what
// is still incomplete. Callers outside this package could otherwise only match
// on a message.
func IsRateLimit(err error) bool { return isRateLimit(err) }

// PickVersion chooses which version of a model an intake should take.
//
// An explicit id wins. With none given it is the newest, which is what the API
// returns first -- deliberately not re-sorted by date, because publishedAt is
// null for drafts and sorting on it moves them to the wrong end.
func PickVersion(listings []Listing, versionID string) (Listing, bool) {
	if versionID != "" {
		for _, l := range listings {
			if l.VersionID == versionID {
				return l, true
			}
		}
		return Listing{}, false
	}
	if len(listings) == 0 {
		return Listing{}, false
	}
	return listings[0], true
}
