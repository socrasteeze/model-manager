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

// filterSQL builds the WHERE clause shared by Search and FacetCounts.
//
// It is shared rather than duplicated because the two used to disagree: facet
// counts were global while the result list was filtered, so the sidebar
// promised 400 loras next to a list of 12. `skip` names a facet dimension to
// leave unfiltered, which is what makes multi-select work -- once you pick
// "lora", the type facet must still count the other types or there is no way to
// add a second one.
func filterSQL(q SearchQuery, skip string) (string, []any) {
	where := []string{"1=1"}
	args := []any{}

	if text := strings.TrimSpace(q.Text); text != "" {
		where = append(where, `f.sha256 IN (SELECT sha256 FROM model_search WHERE model_search MATCH ?)`)
		args = append(args, buildFTSQuery(text))
	}

	addIn := func(dimension, column string, values []string) {
		if len(values) == 0 || dimension == skip {
			return
		}
		placeholders := make([]string, len(values))
		for i, v := range values {
			placeholders[i] = "?"
			args = append(args, v)
		}
		where = append(where, fmt.Sprintf("%s IN (%s)", column, strings.Join(placeholders, ",")))
	}
	addIn("types", "r.type", q.Types)
	addIn("base_models", "r.base_model", q.BaseModels)
	addIn("origins", "r.origin", q.Origins)
	addIn("formats", "f.format", q.Formats)

	if len(q.Tags) > 0 && skip != "tags" {
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

	return strings.Join(where, " AND "), args
}

// Search runs a query.
func (s *Store) Search(q SearchQuery) (*SearchResults, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500
	}

	whereSQL, args := filterSQL(q, "")

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
               COALESCE((SELECT COALESCE(NULLIF(pi.thumb_sha256, ''), pi.image_sha256)
                           FROM preview_image pi
                          WHERE pi.sha256 = f.sha256
                          ORDER BY (pi.source = 'manual') DESC, pi.position, pi.id
                          LIMIT 1), ''),
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
		h.Filename = FilenameOf(h.Path)
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
	filename := strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(FilenameOf(path))

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

	// Total is how many models the current query matches.
	Total int `json:"total"`

	// LibraryTotal is how many models exist at all, ignoring every filter.
	// Kept separate because the two answer different questions: "nothing
	// matches" and "there is nothing here yet" need different advice, and once
	// Total became filter-aware there was nothing left to tell them apart.
	LibraryTotal int `json:"library_total"`
}

// FacetCounts summarizes what the current query matches, for building filter UI.
//
// It takes the query. It used to take nothing, so the counts described the
// whole library while the list beside them described a filtered subset -- a
// sidebar that said "lora 412" next to twelve results. Each dimension is
// counted with its own filter lifted, which is what lets a second value be
// added to a facet that is already narrowing the search.
func (s *Store) FacetCounts(q SearchQuery) (*Facets, error) {
	f := &Facets{
		Types: map[string]int{}, BaseModels: map[string]int{},
		Origins: map[string]int{}, Formats: map[string]int{},
		Tags: map[string]int{},
	}

	load := func(dimension, expr, query string, dest map[string]int) error {
		whereSQL, args := filterSQL(q, dimension)
		sql := fmt.Sprintf(query, expr, whereSQL, expr)
		rows, err := s.db.Query(sql, args...)
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

	// One shape for every scalar facet: count over the same joined set Search
	// pages, so a facet count and a result count can never disagree about what
	// a row is.
	const scalarFacet = `
        SELECT %s, COUNT(*)
          FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
         WHERE %s
         GROUP BY %s`

	for _, facet := range []struct {
		dimension, expr string
		dest            map[string]int
	}{
		{"types", "COALESCE(r.type, '')", f.Types},
		{"base_models", "COALESCE(r.base_model, '')", f.BaseModels},
		{"origins", "COALESCE(r.origin, '')", f.Origins},
		{"formats", "f.format", f.Formats},
	} {
		if err := load(facet.dimension, facet.expr, scalarFacet, facet.dest); err != nil {
			return nil, fmt.Errorf("store: facet counts (%s): %w", facet.dimension, err)
		}
	}

	// Tags need the join, so they get their own shape rather than being bent
	// into the scalar one.
	tagWhere, tagArgs := filterSQL(q, "tags")
	tagRows, err := s.db.Query(`
        SELECT t.name, COUNT(*)
          FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
          JOIN model_tag mt ON mt.sha256 = f.sha256
          JOIN tag t ON t.id = mt.tag_id
         WHERE `+tagWhere+`
         GROUP BY t.id
         ORDER BY t.name`, tagArgs...)
	if err != nil {
		return nil, fmt.Errorf("store: facet counts (tags): %w", err)
	}
	defer tagRows.Close()
	for tagRows.Next() {
		var name string
		var n int
		if err := tagRows.Scan(&name, &n); err != nil {
			return nil, err
		}
		f.Tags[name] = n
	}
	if err := tagRows.Err(); err != nil {
		return nil, err
	}

	totalWhere, totalArgs := filterSQL(q, "")
	if err := s.db.QueryRow(`
        SELECT COUNT(*) FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
         WHERE `+totalWhere, totalArgs...).Scan(&f.Total); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM model_file`).Scan(&f.LibraryTotal); err != nil {
		return nil, err
	}
	return f, nil
}

// FilenameOf returns the basename of a path recorded in the index. Exported
// because it is the one place that gets this right for a path this app did not
// necessarily write itself, and every other package that needs a model's
// filename -- rendering it into a ComfyUI graph, among others -- should call
// this rather than keep its own copy.
func FilenameOf(path string) string {
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
