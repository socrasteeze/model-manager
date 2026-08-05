package origin

// Relating remote listings to the local library.
//
// This is the payoff of content addressing. Every other model browser answers
// "do I already have this?" by comparing filenames, which fails in both
// directions: the same file renamed looks new, and a different file with the
// same name looks owned. Because every local file here is already keyed by its
// SHA256 (§2), and every provider reports a SHA256 for its files, the question
// is a hash lookup and the answer is exact.
//
// The same index answers "what can be updated": if a listing's model is one we
// hold *some* version of, but this particular version's hash is not on disk,
// that is an available update rather than a new model.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/socrasteeze/model-manager/internal/store"
)

// LocalIndex is a snapshot of what the library already holds, in the terms a
// remote listing can be compared against.
type LocalIndex struct {
	// bySHA maps an uppercase SHA256 to a representative local path.
	bySHA map[string]string

	// ownedVersions maps "provider/modelID" to the versions held locally.
	ownedVersions map[string][]ownedVersion
}

type ownedVersion struct {
	VersionID   string
	VersionName string
	SHA256      string
}

// BuildLocalIndex reads the library into a comparison index.
//
// Both halves come from data already recorded: model_file for content hashes,
// and the archived origin responses for the model/version identity that hash
// was published under. Nothing here re-queries the network.
func BuildLocalIndex(st *store.Store) (*LocalIndex, error) {
	idx := &LocalIndex{
		bySHA:         map[string]string{},
		ownedVersions: map[string][]ownedVersion{},
	}

	// Content hashes, with one present path each for display. A model with no
	// present path is still indexed: it is still owned, just not mounted, and
	// reporting it as new would prompt a pointless re-download.
	rows, err := st.DB().Query(`
        SELECT f.sha256,
               COALESCE((SELECT p.path FROM model_file_path p
                          WHERE p.sha256 = f.sha256 AND p.provisional = 0
                          ORDER BY p.present DESC, p.id LIMIT 1), '')
          FROM model_file f`)
	if err != nil {
		return nil, fmt.Errorf("origin: indexing local hashes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var sha, path string
		if err := rows.Scan(&sha, &path); err != nil {
			return nil, err
		}
		idx.bySHA[strings.ToUpper(sha)] = path
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := idx.loadOwnedVersions(st); err != nil {
		return nil, err
	}
	return idx, nil
}

// loadOwnedVersions reconstructs which remote model each local file came from.
//
// Two sources, in order. The persisted model_origin rows are the fast path:
// indexed, joinable, and what the library's update badge and version grouping
// are actually built on.
//
// The archive scan behind it is kept rather than replaced, because it is what
// buys the property that made deriving-from-the-archive right in the first
// place: update detection works retroactively for everything enriched before
// any of this existed, with no re-fetch. It is anti-joined against the
// persisted rows, so on a backfilled database it matches nothing and costs one
// indexed lookup; on a database that has not been backfilled it is byte-for-
// byte the old behaviour.
func (idx *LocalIndex) loadOwnedVersions(st *store.Store) error {
	persisted, err := st.DB().Query(`
        SELECT provider, origin_model_id, origin_version_id, origin_version_name, sha256
          FROM model_origin
         WHERE provider IN (?, ?)`, ProviderCivitaiID, ProviderCivArchiveID)
	if err != nil {
		return fmt.Errorf("origin: reading persisted origins: %w", err)
	}
	for persisted.Next() {
		var provider, modelID, versionID, versionName, sha string
		if err := persisted.Scan(&provider, &modelID, &versionID, &versionName, &sha); err != nil {
			persisted.Close()
			return err
		}
		mk := provider + "/" + modelID
		idx.ownedVersions[mk] = append(idx.ownedVersions[mk], ownedVersion{
			VersionID:   versionID,
			VersionName: versionName,
			SHA256:      strings.ToUpper(sha),
		})
	}
	persisted.Close()
	if err := persisted.Err(); err != nil {
		return err
	}

	rows, err := st.DB().Query(`
        SELECT c.provider, c.lookup_key, c.raw_response
          FROM origin_cache c
         WHERE c.found = 1 AND c.raw_response IS NOT NULL
           AND c.provider IN (?, ?)
           AND NOT EXISTS (SELECT 1 FROM model_origin mo
                            WHERE mo.sha256 = lower(c.lookup_key)
                              AND mo.provider = c.provider)`,
		ProviderCivitaiID, ProviderCivArchiveID)
	if err != nil {
		return fmt.Errorf("origin: indexing owned versions: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var provider, key string
		var raw sql.NullString
		if err := rows.Scan(&provider, &key, &raw); err != nil {
			return err
		}
		if !raw.Valid || raw.String == "" {
			continue
		}

		modelID, versionID, versionName := decodeOwnedVersion(provider, raw.String)
		if modelID == "" || modelID == "0" {
			continue
		}
		mk := provider + "/" + modelID
		idx.ownedVersions[mk] = append(idx.ownedVersions[mk], ownedVersion{
			VersionID:   versionID,
			VersionName: versionName,
			SHA256:      strings.ToUpper(key),
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Newest version first, by numeric id. The rows arrive in whatever order
	// SQLite emits them, and everything downstream that says "the version you
	// have" reads element 0 -- without this the reported have-version (and the
	// LocalPath shown for it) changes across runs for no visible reason.
	for _, versions := range idx.ownedVersions {
		sort.Slice(versions, func(i, j int) bool {
			return numericID(versions[i].VersionID) > numericID(versions[j].VersionID)
		})
	}
	return nil
}

// BackfillModelOrigin decodes archived responses into persisted identity rows.
//
// Idempotent and cheap: it only looks at rows model_origin does not already
// cover, so re-running it costs one anti-join. Called at the start of an update
// sweep, after enrichment, and once on a writable daemon's startup.
//
// Joined against model_file rather than trusting the archive's key, because
// origin_cache is deliberately never pruned -- an archived response can outlive
// the file it described, and model_origin.sha256 is a foreign key. Skipping
// those orphans here is also why the backfill has to be re-runnable: a scan
// that re-indexes a file which came back must be able to pick it up.
func BackfillModelOrigin(st *store.Store) (int, error) {
	rows, err := st.DB().Query(`
        SELECT c.provider, c.lookup_key, c.raw_response
          FROM origin_cache c
          JOIN model_file f ON f.sha256 = lower(c.lookup_key)
         WHERE c.found = 1 AND c.raw_response IS NOT NULL
           AND c.provider IN (?, ?)
           AND NOT EXISTS (SELECT 1 FROM model_origin mo
                            WHERE mo.sha256 = lower(c.lookup_key)
                              AND mo.provider = c.provider)`,
		ProviderCivitaiID, ProviderCivArchiveID)
	if err != nil {
		return 0, fmt.Errorf("origin: selecting rows to backfill: %w", err)
	}

	type pending struct{ provider, sha, raw string }
	var todo []pending
	for rows.Next() {
		var p pending
		var raw sql.NullString
		if err := rows.Scan(&p.provider, &p.sha, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		if raw.Valid && raw.String != "" {
			p.raw = raw.String
			todo = append(todo, p)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Read fully before writing: the store serializes writers on one mutex, and
	// holding a read cursor open across every write is how a single-writer
	// SQLite setup finds new ways to block on itself.
	written := 0
	for _, p := range todo {
		modelID, versionID, versionName := decodeOwnedVersion(p.provider, p.raw)
		// A body this decoder cannot read is skipped, not fatal. It is one
		// model's identity, and failing the whole backfill over it would take
		// the update sweep with it.
		if modelID == "" || modelID == "0" {
			continue
		}
		if err := st.PutModelOrigin(store.ModelOrigin{
			SHA256: strings.ToLower(p.sha), Provider: p.provider,
			ModelID: modelID, VersionID: versionID, VersionName: versionName,
			Source: store.OriginSourceArchive,
		}); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

// decodeOwnedVersion extracts (modelID, versionID, versionName) from an
// archived response body.
//
// Civitai's by-hash body is a model version: {id, modelId, name}. CivArchive
// mirrors the same records but may keep its own spellings and envelopes, so
// its rows go through the same tolerant decode as its listings -- a
// Civitai-only struct here silently dropped every civarchive row, making the
// provider's half of the ownership index dead.
func decodeOwnedVersion(provider, raw string) (modelID, versionID, versionName string) {
	if provider == ProviderCivArchiveID {
		records, _, err := decodeCivArchive(json.RawMessage(raw))
		if err != nil || len(records) == 0 {
			var one caRecord
			if err := json.Unmarshal([]byte(raw), &one); err != nil {
				return "", "", ""
			}
			records = []caRecord{one}
		}
		r := records[0]
		// Same id semantics as caListing: top-level id is the model unless a
		// modelId disambiguates, and version identity comes from the aliases.
		return firstNonEmptyNum(r.ModelID, r.ModelIDAlt, r.ID),
			firstNonEmptyNum(r.VersionIDAlt, r.VersionID),
			r.VersionName
	}

	var v struct {
		ID      json.Number `json:"id"`
		ModelID json.Number `json:"modelId"`
		Name    string      `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", "", ""
	}
	return v.ModelID.String(), v.ID.String(), v.Name
}

// numericID parses a version id for ordering; non-numeric ids sort last.
func numericID(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// Annotate fills in Listing.Local for each item.
func (idx *LocalIndex) Annotate(items []Listing) {
	for i := range items {
		items[i].Local = idx.match(items[i])
	}
}

// match classifies one listing against the local library.
func (idx *LocalIndex) match(l Listing) *LocalMatch {
	// Exact content match wins over everything: if these bytes are on disk, the
	// listing is owned no matter what its version identity says.
	for _, f := range l.Files {
		if f.SHA256 == "" {
			continue
		}
		if path, ok := idx.bySHA[strings.ToUpper(f.SHA256)]; ok {
			return &LocalMatch{
				Status: MatchHave,
				SHA256: strings.ToUpper(f.SHA256),
				Path:   path,
			}
		}
	}

	// No content match. If another version of the same model is held, this is
	// an update rather than a discovery.
	//
	// CivArchive mirrors Civitai ids, so a model owned via Civitai is the same
	// model when seen on CivArchive; both keys are checked for that reason.
	if l.ID != "" {
		for _, provider := range []string{l.Provider, ProviderCivitaiID, ProviderCivArchiveID} {
			owned := idx.ownedVersions[provider+"/"+l.ID]
			if len(owned) == 0 {
				continue
			}
			for _, ov := range owned {
				// Same version already held but its file hash was not among the
				// listing's files: treat as owned, not as an update. This is the
				// common case where the listing omitted hashes entirely.
				if ov.VersionID != "" && ov.VersionID == l.VersionID {
					return &LocalMatch{
						Status:          MatchHave,
						SHA256:          ov.SHA256,
						Path:            idx.bySHA[ov.SHA256],
						HaveVersionID:   ov.VersionID,
						HaveVersionName: ov.VersionName,
					}
				}
			}
			// A different version of a model we own. Only a listing NEWER than
			// the newest held is an update; Civitai returns every version of a
			// model, and badging v1-v4 of an owned v5 as "update" invites the
			// user to downgrade. Older (or unordered) versions report as new,
			// with the held version attached so a UI can still say "you have
			// v5" without calling it an upgrade.
			newest := owned[0]
			status := MatchNew
			if numericID(l.VersionID) > numericID(newest.VersionID) && numericID(newest.VersionID) >= 0 {
				status = MatchOutdated
			}
			return &LocalMatch{
				Status:          status,
				SHA256:          newest.SHA256,
				Path:            idx.bySHA[newest.SHA256],
				HaveVersionID:   newest.VersionID,
				HaveVersionName: newest.VersionName,
			}
		}
	}

	return &LocalMatch{Status: MatchNew}
}

// OwnedModelIDs lists every remote model the library holds a version of.
//
// This is the input to update checking: rather than searching for everything
// and filtering, the update check asks each of these models directly what its
// newest version is.
func (idx *LocalIndex) OwnedModelIDs(provider string) []string {
	prefix := provider + "/"
	var out []string
	for key := range idx.ownedVersions {
		if id, ok := strings.CutPrefix(key, prefix); ok {
			out = append(out, id)
		}
	}
	return out
}

// OwnedVersionIDs returns the version ids held for a model.
func (idx *LocalIndex) OwnedVersionIDs(provider, modelID string) []string {
	var out []string
	for _, ov := range idx.ownedVersions[provider+"/"+modelID] {
		if ov.VersionID != "" {
			out = append(out, ov.VersionID)
		}
	}
	return out
}

// Has reports whether a content hash is present locally.
func (idx *LocalIndex) Has(sha256 string) bool {
	_, ok := idx.bySHA[strings.ToUpper(sha256)]
	return ok
}
