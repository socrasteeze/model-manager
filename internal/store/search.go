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

	// HasPreview, NSFW, Present and NeedsUpdate are tri-state: nil means "do
	// not filter".
	HasPreview *bool
	NSFW       *bool
	Present    *bool

	// NeedsUpdate selects models the last sweep found a newer upstream version
	// for. False selects the rest, which deliberately includes models that have
	// never been checked: an unchecked model is not known to need an update,
	// and a third filter value for "unknown" would be a control nobody could
	// read. The honest place to say "this answer is three weeks old" is
	// SearchHit.UpdateCheckedAt, on the badge itself.
	NeedsUpdate *bool

	// NeedsAttention selects models with no name, no base model, or no preview
	// -- use case 10, spotting the gaps.
	NeedsAttention bool

	// Group collapses versions of one upstream model into a single row:
	// "architecture" (same model and base model), "model" (same model, any
	// base model), or "" / "off" for no collapsing.
	//
	// Deliberately NOT applied by SearchSHAs; see the note there.
	Group string

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

	// --- update badge ------------------------------------------------------
	//
	// Enough for the card to render "v3 -> v5" without a second request. The
	// have-side is load-bearing: a badge that can only say "update" gives the
	// user nothing to decide with.
	UpdateAvailable   bool   `json:"update_available,omitempty"`
	HaveVersionName   string `json:"have_version_name,omitempty"`
	LatestVersionName string `json:"latest_version_name,omitempty"`

	// UpdateCheckedAt is when the provider was last asked. Carried so the UI
	// can age the badge instead of presenting a three-week-old answer with the
	// same confidence as a fresh one -- the same concern RateLimited exists for
	// on a truncated sweep, applied to a stored result.
	UpdateCheckedAt string `json:"update_checked_at,omitempty"`

	// UpdateBaseModelChanged marks an update that retargets a different base
	// model. Computed on read rather than stored: it compares the record's own
	// base_model, which the user can edit, against the upstream one.
	UpdateBaseModelChanged bool `json:"update_base_model_changed,omitempty"`

	// GroupSize is how many models this row stands for once versions are
	// collapsed, so a card can say "3 versions" rather than silently hiding
	// two. 1 when grouping is off or the row is alone in its group.
	GroupSize int `json:"group_size,omitempty"`
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
	return filterSQLAs(q, skip, "f", "r")
}

// filterSQLAs is filterSQL against caller-chosen table aliases.
//
// Exists because version grouping needs the user's filters applied twice in
// one statement: once to the row being considered, and once inside a subquery
// deciding whether some *other* row of the same model should be shown instead.
// Both must mean the same thing, or filtering to SDXL would hide a model whose
// newest version is Pony -- the row that would have represented the group is
// filtered out, and nothing takes its place.
//
// The correlated subquery needs its own aliases, so the predicate cannot be
// hard-coded to f/r. Writing a second copy for the subquery is exactly the
// duplication filterSQL exists to prevent.
func filterSQLAs(q SearchQuery, skip, f, r string) (string, []any) {
	where := []string{"1=1"}
	args := []any{}

	if text := strings.TrimSpace(q.Text); text != "" {
		where = append(where, fmt.Sprintf(
			`%s.sha256 IN (SELECT sha256 FROM model_search WHERE model_search MATCH ?)`, f))
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
	addIn("types", r+".type", q.Types)
	addIn("base_models", r+".base_model", q.BaseModels)
	addIn("origins", r+".origin", q.Origins)
	addIn("formats", f+".format", q.Formats)

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
             WHERE mt.sha256 = %s.sha256 AND t.name IN (%s)
        ) = %d`, f, strings.Join(placeholders, ","), len(q.Tags)))
	}

	if q.HasPreview != nil {
		clause := "EXISTS"
		if !*q.HasPreview {
			clause = "NOT EXISTS"
		}
		where = append(where, fmt.Sprintf(
			`%s (SELECT 1 FROM preview_image pi WHERE pi.sha256 = %s.sha256)`, clause, f))
	}
	if q.NSFW != nil {
		where = append(where, r+".nsfw = ?")
		args = append(args, boolInt(*q.NSFW))
	}
	if q.Present != nil {
		clause := "EXISTS"
		if !*q.Present {
			clause = "NOT EXISTS"
		}
		where = append(where, fmt.Sprintf(
			`%s (SELECT 1 FROM model_file_path p WHERE p.sha256 = %s.sha256 AND p.present = 1)`,
			clause, f))
	}
	if q.NeedsUpdate != nil && skip != "needs_update" {
		clause := "EXISTS"
		if !*q.NeedsUpdate {
			clause = "NOT EXISTS"
		}
		// Through the model_update view rather than an inline join, so this
		// predicate and the columns Search selects for the badge are one
		// definition. filterSQL exists because Search and FacetCounts once
		// disagreed about what a row was; a second spelling of "needs update"
		// sitting beside it would reintroduce exactly that.
		where = append(where, fmt.Sprintf(
			`%s (SELECT 1 FROM model_update u WHERE u.sha256 = %s.sha256)`, clause, f))
	}
	if q.NeedsAttention {
		where = append(where, fmt.Sprintf(`(
            %[2]s.name IS NULL OR %[2]s.base_model IS NULL
            OR NOT EXISTS (SELECT 1 FROM preview_image pi WHERE pi.sha256 = %[1]s.sha256)
        )`, f, r))
	}

	return strings.Join(where, " AND "), args
}

// collapseSQL keeps only one row per group of versions of the same model.
//
// A row filter, not a GROUP BY. Keeping the representative row means LIMIT,
// OFFSET, COUNT(*) and every sort keep working untouched, and the facet counts
// stay computed over the same set the results come from -- which a GROUP BY
// would quietly break for both.
//
// The representative is the newest version owned, tie-broken by sha so the
// choice is stable across runs rather than dependent on row order.
//
// A model with no recorded origin identity is never collapsed: without knowing
// which upstream model it belongs to there is no group to collapse it into,
// and hiding it would be inventing a relationship from nothing.
//
// The user's own filters are repeated inside, against the subquery's aliases,
// so a row is only suppressed in favour of one that is itself visible. Without
// that, filtering to SDXL would hide a model whose newest version is Pony: the
// row that would have represented the group is filtered out of the results and
// nothing takes its place.
func collapseSQL(q SearchQuery, mode string) (string, []any) {
	if mode != "architecture" && mode != "model" {
		return "", nil
	}

	inner, args := filterSQLAs(q, "", "f2", "r2")

	sameArchitecture := ""
	if mode == "architecture" {
		// COALESCE, not =, so two versions that both lack a base model group
		// together rather than each becoming its own card: in SQL NULL = NULL
		// is NULL, which is not true.
		sameArchitecture = `AND COALESCE(r2.base_model, '') = COALESCE(r.base_model, '')`
	}

	return fmt.Sprintf(`NOT EXISTS (
        SELECT 1
          FROM model_origin mo_self
          JOIN model_origin mo_other
            ON mo_other.provider = mo_self.provider
           AND mo_other.origin_model_id = mo_self.origin_model_id
          JOIN model_file f2 ON f2.sha256 = mo_other.sha256
          LEFT JOIN model_record r2 ON r2.sha256 = f2.sha256
         WHERE mo_self.sha256 = f.sha256
           AND f2.sha256 <> f.sha256
           %s
           AND (%s)
           AND (CAST(mo_other.origin_version_id AS INTEGER)
                  > CAST(mo_self.origin_version_id AS INTEGER)
                OR (CAST(mo_other.origin_version_id AS INTEGER)
                      = CAST(mo_self.origin_version_id AS INTEGER)
                    AND f2.sha256 < f.sha256))
    )`, sameArchitecture, inner), args
}

// groupSizeSQL counts how many models the row stands for, so a collapsed card
// can say "3 versions" rather than silently hiding two.
func groupSizeSQL(q SearchQuery, mode string) (string, []any) {
	if mode != "architecture" && mode != "model" {
		return "1", nil
	}

	inner, args := filterSQLAs(q, "", "f3", "r3")
	sameArchitecture := ""
	if mode == "architecture" {
		sameArchitecture = `AND COALESCE(r3.base_model, '') = COALESCE(r.base_model, '')`
	}

	return fmt.Sprintf(`COALESCE((
        SELECT COUNT(*)
          FROM model_origin mo_self
          JOIN model_origin mo_other
            ON mo_other.provider = mo_self.provider
           AND mo_other.origin_model_id = mo_self.origin_model_id
          JOIN model_file f3 ON f3.sha256 = mo_other.sha256
          LEFT JOIN model_record r3 ON r3.sha256 = f3.sha256
         WHERE mo_self.sha256 = f.sha256
           %s
           AND (%s)
    ), 1)`, sameArchitecture, inner), args
}

// SearchSHAs returns every model matching a query, ignoring Limit and Offset.
//
// Search caps at 500 rows because it feeds a paged grid. A bulk action over "the
// models I am currently looking at" needs the whole matching set, not the page,
// and re-deriving that set from a different WHERE clause is how the facet counts
// once ended up describing a different library than the results. So this shares
// filterSQL and differs from Search only in what it selects and what it omits.
//
// Sorted by size descending to match the order enrichment already walks in, so a
// bulk run that stops early has dealt with the models that cost the most first.
func (s *Store) SearchSHAs(q SearchQuery) ([]string, error) {
	whereSQL, args := filterSQL(q, "")

	rows, err := s.db.Query(`
        SELECT f.sha256
          FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
         WHERE `+whereSQL+`
         ORDER BY f.size DESC, f.sha256 ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: listing search matches: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, err
		}
		out = append(out, sha)
	}
	return out, rows.Err()
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

	// The collapse is part of the WHERE clause, so the count, the page and the
	// sort all see the same set. Its args come first because they are
	// interpolated after the base predicate but bind in statement order.
	collapse, collapseArgs := collapseSQL(q, q.Group)
	if collapse != "" {
		whereSQL += " AND " + collapse
		args = append(args, collapseArgs...)
	}

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

	groupSize, groupSizeArgs := groupSizeSQL(q, q.Group)

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
               (SELECT COUNT(*) FROM model_file_path p WHERE p.sha256 = f.sha256 AND p.present = 1),
               COALESCE(u.have_version_name, ''), COALESCE(u.latest_version_name, ''),
               COALESCE(u.checked_at, ''), COALESCE(u.latest_base_model, ''),
               u.sha256 IS NOT NULL,
               %[4]s
          FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
          -- Safe as a LEFT JOIN only because the view guarantees at most one
          -- row per sha; a duplicate would multiply result rows and break the
          -- LIMIT below. Pinned by TestOneRowPerFileWithTwoOriginProviders.
          LEFT JOIN model_update u ON u.sha256 = f.sha256
         WHERE %[1]s
         ORDER BY %[2]s %[3]s, f.sha256 ASC
         LIMIT ? OFFSET ?`, whereSQL, order, direction, groupSize)

	// groupSize is in the SELECT list, so its parameters bind before the ones
	// in WHERE. Prepended rather than appended for that reason.
	args = append(append(append([]any{}, groupSizeArgs...), args...), q.Limit, q.Offset)
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
		var latestBase string
		if err := rows.Scan(&h.SHA256, &h.Format, &h.Size, &h.Name, &h.Type,
			&h.BaseModel, &h.Version, &h.Origin, &nsfw, &triggers,
			&h.PreviewImage, &h.Path, &h.PathCount,
			&h.HaveVersionName, &h.LatestVersionName, &h.UpdateCheckedAt,
			&latestBase, &h.UpdateAvailable, &h.GroupSize); err != nil {
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
		h.UpdateBaseModelChanged = h.UpdateAvailable && baseModelChanged(h.BaseModel, latestBase)
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

	// NeedsUpdate is how many models have a newer version upstream, with the
	// needs_update filter itself lifted -- the same rule every other facet
	// follows. Without lifting it, the count beside an active toggle would
	// equal the result count and turning it off would look like a no-op.
	NeedsUpdate int `json:"needs_update"`
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

	// Applied to every facet as well as the total: a sidebar counting rows the
	// grid collapses away is the same class of disagreement filterSQL was
	// introduced to fix.
	withCollapse := func(whereSQL string, args []any) (string, []any) {
		collapse, collapseArgs := collapseSQL(q, q.Group)
		if collapse == "" {
			return whereSQL, args
		}
		return whereSQL + " AND " + collapse, append(args, collapseArgs...)
	}

	load := func(dimension, expr, query string, dest map[string]int) error {
		whereSQL, args := filterSQL(q, dimension)
		whereSQL, args = withCollapse(whereSQL, args)
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
	tagWhere, tagArgs = withCollapse(tagWhere, tagArgs)
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
	totalWhere, totalArgs = withCollapse(totalWhere, totalArgs)
	if err := s.db.QueryRow(`
        SELECT COUNT(*) FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
         WHERE `+totalWhere, totalArgs...).Scan(&f.Total); err != nil {
		return nil, err
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM model_file`).Scan(&f.LibraryTotal); err != nil {
		return nil, err
	}

	// Its own filter lifted, like every other facet.
	nuWhere, nuArgs := filterSQL(q, "needs_update")
	nuWhere, nuArgs = withCollapse(nuWhere, nuArgs)
	if err := s.db.QueryRow(`
        SELECT COUNT(*) FROM model_file f
          LEFT JOIN model_record r ON r.sha256 = f.sha256
         WHERE `+nuWhere+`
           AND EXISTS (SELECT 1 FROM model_update u WHERE u.sha256 = f.sha256)`,
		nuArgs...).Scan(&f.NeedsUpdate); err != nil {
		return nil, fmt.Errorf("store: facet counts (needs_update): %w", err)
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
