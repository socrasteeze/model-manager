package store

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Search over the materialized record, using FTS5 for text (spec §5.1).
//
// Use case 1 is "find a model" across 19k files by name, base model, type,
// trigger word or tag -- currently impossible. Everything here exists for that.

// SearchQuery describes what to look for.
type SearchQuery struct {
	Text string

	Types      []string
	BaseModels []string
	Tags       []string
	Origins    []string
	Formats    []string

	// HasPreview, NSFW and Present are tri-state: nil means "do not filter".
	HasPreview *bool
	NSFW       *bool
	Present    *bool

	// NeedsAttention selects models with no name, no base model, or no preview
	// -- use case 10, spotting the gaps.
	NeedsAttention bool

	// Sort is one of name, size, recent, added. Defaults to name.
	Sort   string
	Desc   bool
	Limit  int
	Offset int
}

// SearchHit is one result row.
type SearchHit struct {
	SHA256       string   `json:"sha256"`
	Name         string   `json:"name,omitempty"`
	Type         string   `json:"type,omitempty"`
	BaseModel    string   `json:"base_model,omitempty"`
	Version      string   `json:"version,omitempty"`
	Origin       string   `json:"origin,omitempty"`
	Format       string   `json:"format"`
	Size         int64    `json:"size"`
	TriggerWords []string `json:"trigger_words,omitempty"`
	Tags         []string `json:"tags,omitempty"`
	PreviewImage string   `json:"preview_image,omitempty"`
	Filename     string   `json:"filename,omitempty"`
	Path         string   `json:"path,omitempty"`
	PathCount    int      `json:"path_count"`
	Present      bool     `json:"present"`
	NSFW         *bool    `json:"nsfw,omitempty"`
}

// SearchResults is a page of hits.
type SearchResults struct {
	Hits   []SearchHit `json:"hits"`
	Total  int         `json:"total"`
	Limit  int         `json:"limit"`
	Offset int         `json:"offset"`
}

// Search runs a query.
func (s *Store) Search(q SearchQuery) (*SearchResults, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500
	}

	where := []string{"1=1"}
	args := []any{}

	if text := strings.TrimSpace(q.Text); text != "" {
		where = append(where, `f.sha256 IN (SELECT sha256 FROM model_search WHERE model_search MATCH ?)`)
		args = append(args, buildFTSQuery(text))
	}

	addIn := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		placeholders := make([]string, len(values))
		for i, v := range values {
			placeholders[i] = "?"
			args = append(args, v)
		}
		where = append(where, fmt.Sprintf("%s IN (%s)", column, strings.Join(placeholders, ",")))
	}
	addIn("r.type", q.Types)
	addIn("r.base_model", q.BaseModels)
	addIn("r.origin", q.Origins)
	addIn("f.format", q.Formats)

	if len(q.Tags) > 0 {
		// Every requested tag must be present, not any of them: narrowing is the
		// point of adding a second tag.
		placeholders := make([]string, len(q.Tags))
		for i, tag := range q.Tags {
			placeholders[i] = "?"
			args = append(args, tag)
		}
		where = append(where, fmt.Sprintf(`(
            SELECT COUNT(DISTINCT t.name) FROM model_tag mt
              JOIN tag t ON t.id = mt.tag_id
             WHERE mt.sha256 = f.sha256 AND t.name IN (%s)
        ) = %d`, strings.Join(placeholders, ","), len(q.Tags)))
	}

	if q.HasPreview != nil {
		clause := "EXISTS"
		if !*q.HasPreview {
			clause = "NOT EXISTS"
		}
		where = append(where,
			clause+` (SELECT 1 FROM preview_image pi WHERE pi.sha256 = f.sha256)`)
	}
	if q.NSFW != nil {
		where = append(where, "r.nsfw = ?")
		args = append(args, boolInt(*q.NSFW))
	}
	if q.Present != nil {
		clause := "EXISTS"
		if !*q.Present {
			clause = "NOT EXISTS"
		}
		where = append(where,
			clause+` (SELECT 1 FROM model_file_path p WHERE p.sha256 = f.sha256 AND p.present = 1)`)
	}
	if q.NeedsAttention {
		where = append(where, `(
            r.name IS NULL OR r.base_model IS NULL
            OR NOT EXISTS (SELECT 1 FROM preview_image pi WHERE pi.sha256 = f.sha256)
        )`)
	}

	whereSQL := strings.Join(where, " AND ")

	var total int
	countSQL := `SELECT COUNT(*) FROM model_file f
                   LEFT JOIN model_record r ON r.sha256 = f.sha256
                  WHERE ` + whereSQL
	if err := s.db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("store: counting search results: %w", err)
	}

	order := "COALESCE(r.name, '') COLLATE NOCASE"
	switch q.Sort {
	case "size":
		order = "f.size"
	case "recent":
		order = "f.last_verified"
	case "added":
		order = "f.first_seen"
	}
	direction := "ASC"
	if q.Desc {
		direction = "DESC"
	}

	// sha256 as the final key gives a total order, so paging cannot show or skip
	// a row because two records tied on the sort column.
	querySQL := fmt.Sprintf(`
        SELECT f.sha256, f.format, f.size,
               COALESCE(r.name, ''), COALESCE(r.type, ''), COALESCE(r.base_model, ''),
               COALESCE(r.version, ''), COALESCE(r.origin, ''), r.nsfw, r.trigger_words,
               COALESCE((SELECT pi.image_sha256 FROM preview_image pi
                          WHERE pi.sha256 = f.sha256
                          ORDER BY pi.position, pi.id LIMIT 1), ''),
               COALESCE((SELECT p.path FROM model_file_path p
                          WHERE p.sha256 = f.sha256
                          ORDER BY p.present DESC, p.id LIMIT 1), ''),
               (SELECT COUNT(*) FROM model_file_path p WHERE p.sha256 = f.sha256 AND p.present = 1)
          FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
         WHERE %s
         ORDER BY %s %s, f.sha256 ASC
         LIMIT ? OFFSET ?`, whereSQL, order, direction)

	args = append(args, q.Limit, q.Offset)
	rows, err := s.db.Query(querySQL, args...)
	if err != nil {
		return nil, fmt.Errorf("store: searching: %w", err)
	}
	defer rows.Close()

	results := &SearchResults{Hits: []SearchHit{}, Total: total, Limit: q.Limit, Offset: q.Offset}
	for rows.Next() {
		var h SearchHit
		var nsfw *int64
		var triggers *string
		if err := rows.Scan(&h.SHA256, &h.Format, &h.Size, &h.Name, &h.Type,
			&h.BaseModel, &h.Version, &h.Origin, &nsfw, &triggers,
			&h.PreviewImage, &h.Path, &h.PathCount); err != nil {
			return nil, err
		}
		if nsfw != nil {
			b := *nsfw == 1
			h.NSFW = &b
		}
		if triggers != nil {
			_ = json.Unmarshal([]byte(*triggers), &h.TriggerWords)
		}
		h.Present = h.PathCount > 0
		h.Filename = filenameOf(h.Path)
		results.Hits = append(results.Hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Tags are fetched per hit rather than joined: joining them would multiply
	// rows and break LIMIT, and a page is at most 500 rows.
	for i := range results.Hits {
		tags, err := s.Tags(results.Hits[i].SHA256)
		if err != nil {
			return nil, err
		}
		results.Hits[i].Tags = tags
	}
	return results, nil
}

// buildFTSQuery turns user text into a safe FTS5 expression.
//
// Raw user input cannot go into MATCH: FTS5 has its own operator syntax, so a
// stray quote or a bare `AND` is a syntax error rather than a search, and a
// user typing `-` gets an error instead of results. Every token is quoted,
// which disables operators, and the final token gets a prefix wildcard so
// search feels responsive as it is typed.
func buildFTSQuery(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return `""`
	}

	var tokens []string
	for i, f := range fields {
		cleaned := strings.Map(func(r rune) rune {
			// Quotes and the FTS column-filter colon are the characters that
			// escape the quoting, so they are removed rather than escaped.
			if r == '"' || r == ':' {
				return -1
			}
			return r
		}, f)
		if cleaned == "" {
			continue
		}
		if i == len(fields)-1 {
			tokens = append(tokens, `"`+cleaned+`"*`)
		} else {
			tokens = append(tokens, `"`+cleaned+`"`)
		}
	}
	if len(tokens) == 0 {
		return `""`
	}
	return strings.Join(tokens, " AND ")
}

// ReindexSearch rebuilds the whole FTS table.
func (s *Store) ReindexSearch(progress func(done int)) (int, error) {
	s.wmu.Lock()
	if _, err := s.db.Exec(`DELETE FROM model_search`); err != nil {
		s.wmu.Unlock()
		return 0, fmt.Errorf("store: clearing search index: %w", err)
	}
	s.wmu.Unlock()

	rows, err := s.db.Query(`SELECT sha256 FROM model_file`)
	if err != nil {
		return 0, err
	}
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return 0, err
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for i, sha := range shas {
		if err := s.reindexOne(sha); err != nil {
			return i, err
		}
		if progress != nil && (i+1)%1000 == 0 {
			progress(i + 1)
		}
	}
	return len(shas), nil
}

// reindexOne refreshes one model's search row.
//
// Kept in Go rather than in triggers: the indexed text is assembled from four
// tables plus a filename, which is more than a trigger can express without
// becoming the least debuggable thing in the schema.
func (s *Store) reindexOne(sha string) error {
	var name, description, baseModel, typ, triggersJSON, path string
	err := s.db.QueryRow(`
        SELECT COALESCE(r.name, ''), COALESCE(r.description, ''),
               COALESCE(r.base_model, ''), COALESCE(r.type, ''),
               COALESCE(r.trigger_words, ''),
               COALESCE((SELECT p.path FROM model_file_path p
                          WHERE p.sha256 = f.sha256
                          ORDER BY p.present DESC, p.id LIMIT 1), '')
          FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
         WHERE f.sha256 = ?`, sha,
	).Scan(&name, &description, &baseModel, &typ, &triggersJSON, &path)
	if err != nil {
		return fmt.Errorf("store: reading %s for reindex: %w", sha, err)
	}

	var triggers []string
	if triggersJSON != "" {
		_ = json.Unmarshal([]byte(triggersJSON), &triggers)
	}
	tags, err := s.Tags(sha)
	if err != nil {
		return err
	}

	s.wmu.Lock()
	defer s.wmu.Unlock()

	if _, err := s.db.Exec(`DELETE FROM model_search WHERE sha256 = ?`, sha); err != nil {
		return fmt.Errorf("store: clearing search row for %s: %w", sha, err)
	}
	// Filenames are indexed with separators turned into spaces so that searching
	// "cinematic" finds `cinematic_style_v2.safetensors`, which is how local
	// models are actually named.
	filename := strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(filenameOf(path))

	_, err = s.db.Exec(`
        INSERT INTO model_search (sha256, name, description, base_model, type, trigger_words, tags, filename)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sha, name, description, baseModel, typ,
		strings.Join(triggers, " "), strings.Join(tags, " "), filename)
	if err != nil {
		return fmt.Errorf("store: indexing %s: %w", sha, err)
	}
	return nil
}

// Facets are the distinct values available to filter on, with counts.
type Facets struct {
	Types      map[string]int `json:"types"`
	BaseModels map[string]int `json:"base_models"`
	Origins    map[string]int `json:"origins"`
	Formats    map[string]int `json:"formats"`
	Tags       map[string]int `json:"tags"`
	Total      int            `json:"total"`
}

// FacetCounts summarizes what is in the library, for building filter UI.
func (s *Store) FacetCounts() (*Facets, error) {
	f := &Facets{
		Types: map[string]int{}, BaseModels: map[string]int{},
		Origins: map[string]int{}, Formats: map[string]int{},
	}

	load := func(query string, dest map[string]int) error {
		rows, err := s.db.Query(query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var key string
			var n int
			if err := rows.Scan(&key, &n); err != nil {
				return err
			}
			if key != "" {
				dest[key] = n
			}
		}
		return rows.Err()
	}

	queries := map[string]map[string]int{
		`SELECT COALESCE(type, ''), COUNT(*) FROM model_record GROUP BY type`:             f.Types,
		`SELECT COALESCE(base_model, ''), COUNT(*) FROM model_record GROUP BY base_model`: f.BaseModels,
		`SELECT COALESCE(origin, ''), COUNT(*) FROM model_record GROUP BY origin`:         f.Origins,
		`SELECT format, COUNT(*) FROM model_file GROUP BY format`:                         f.Formats,
	}
	for q, dest := range queries {
		if err := load(q, dest); err != nil {
			return nil, fmt.Errorf("store: facet counts: %w", err)
		}
	}

	tags, err := s.AllTags()
	if err != nil {
		return nil, err
	}
	f.Tags = tags

	if err := s.db.QueryRow(`SELECT COUNT(*) FROM model_file`).Scan(&f.Total); err != nil {
		return nil, err
	}
	return f, nil
}

func filenameOf(path string) string {
	if path == "" {
		return ""
	}
	// Handle both separators: a database written on Windows may be read on
	// Linux, and the paths inside it keep their original form.
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}
