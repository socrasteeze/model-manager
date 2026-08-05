import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties } from 'react'
import {
  DEFAULT_GROUPING,
  DEFAULT_INCLUDE_NSFW,
  DEFAULT_THUMB_ASPECT,
  MODEL_TYPES,
  SETTING_BROWSE_NSFW,
  SETTING_FILTERS,
  SETTING_GROUPING,
  SETTING_THUMB_ASPECT,
  asGroupingMode,
  asThumbAspect,
  config,
  deleteSetting,
  emptyFilters,
  getFacets,
  getSettings,
  putSetting,
  searchModels,
  type Facets,
  type Filters,
  type GroupingMode,
  type SearchHit,
  type ThumbAspect,
} from './api'
import { BrowsePanel } from './components/BrowsePanel'
import { EnrichRunner } from './components/EnrichRunner'
import { FilterPanel } from './components/FilterPanel'
import { ModelCard } from './components/ModelCard'
import { ModelDetailPanel } from './components/ModelDetailPanel'
import { SettingsPanel } from './components/SettingsPanel'

const PAGE_SIZE = 60

export function App() {
  const [filters, setFilters] = useState<Filters>(emptyFilters)
  const [query, setQuery] = useState('')
  const [hits, setHits] = useState<SearchHit[]>([])
  const [total, setTotal] = useState(0)
  const [offset, setOffset] = useState(0)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [facets, setFacets] = useState<Facets | null>(null)
  const [selected, setSelected] = useState<string | null>(null)
  const [filtersOpen, setFiltersOpen] = useState(false)

  // Library and Browse are separate modes rather than one blended result list:
  // one is an inventory of what is here, the other is a catalogue of what is
  // not, and merging them would make "have" ambiguous.
  const [tab, setTab] = useState<'library' | 'browse' | 'settings'>('library')

  // Saved filters live on the server, not in localStorage: the same daemon
  // answers the desktop and the phone over the tailnet, so a view configured on
  // one should be the view on the other. Loaded once; until it lands, the
  // default filters are used rather than blocking the first search.
  const [filtersLoaded, setFiltersLoaded] = useState(false)

  // The two preferences the shell owns rather than the Settings tab: the
  // thumbnail ratio drives a CSS variable on this element, and adult results
  // are passed down to Browse. Both are read in the same request as the saved
  // filters -- one round trip, not three.
  const [thumbAspect, setThumbAspect] = useState<ThumbAspect>(DEFAULT_THUMB_ASPECT)
  const [includeNSFW, setIncludeNSFW] = useState(DEFAULT_INCLUDE_NSFW)
  const [grouping, setGrouping] = useState<GroupingMode>(DEFAULT_GROUPING)

  useEffect(() => {
    getSettings()
      .then((s) => {
        const saved = s[SETTING_FILTERS] as Partial<Filters> | undefined
        if (saved) setFilters((f) => ({ ...f, ...saved, q: f.q }))
        setThumbAspect(asThumbAspect(s[SETTING_THUMB_ASPECT]))
        // Absent means the default, which is on. Only an explicit false is off.
        setIncludeNSFW(s[SETTING_BROWSE_NSFW] === undefined ? DEFAULT_INCLUDE_NSFW : !!s[SETTING_BROWSE_NSFW])
        setGrouping(asGroupingMode(s[SETTING_GROUPING]))
      })
      .catch(() => {
        /* a preference that will not load is not a reason to show nothing */
      })
      .finally(() => setFiltersLoaded(true))
  }, [])

  // Applied by the Settings tab writing the value, then telling us, rather than
  // us re-reading every setting: that response also carries library.filters,
  // and re-applying it would clobber in-session filter state against this
  // component's own debounced write below.
  const onPreferenceChanged = useCallback((key: string, value: unknown) => {
    if (key === SETTING_THUMB_ASPECT) setThumbAspect(asThumbAspect(value))
    if (key === SETTING_BROWSE_NSFW) setIncludeNSFW(!!value)
    if (key === SETTING_GROUPING) setGrouping(asGroupingMode(value))
  }, [])

  // Persist after the load has settled, so the initial default state cannot
  // race ahead and overwrite what was saved.
  useEffect(() => {
    if (!filtersLoaded || config.readOnly) return
    const { q: _q, ...persistable } = filters
    const timer = setTimeout(() => {
      void putSetting(SETTING_FILTERS, persistable).catch(() => {})
    }, 400)
    return () => clearTimeout(timer)
  }, [filters, filtersLoaded])

  // Debounce the text box rather than the whole filter object: clicking a facet
  // should feel instant, while typing should not fire a request per keystroke.
  useEffect(() => {
    const timer = setTimeout(() => setFilters((f) => ({ ...f, q: query })), 200)
    return () => clearTimeout(timer)
  }, [query])

  // Counts follow the query. They used to be fetched once on mount with no
  // filters at all, so the sidebar could promise 412 loras beside twelve
  // results.
  const loadFacets = useCallback(() => {
    getFacets(filters, grouping).then(setFacets).catch(() => {
      /* facets are a convenience; their absence must not break search */
    })
  }, [filters, grouping])

  useEffect(loadFacets, [loadFacets])

  // A request counter, so a slow early response cannot overwrite a fast later
  // one and show results for a query the user has already moved on from.
  const requestId = useRef(0)

  const runSearch = useCallback(
    (nextOffset: number, append: boolean) => {
      const id = ++requestId.current
      setLoading(true)
      searchModels(filters, nextOffset, PAGE_SIZE, grouping)
        .then((res) => {
          if (id !== requestId.current) return
          setHits((prev) => (append ? [...prev, ...res.hits] : res.hits))
          setTotal(res.total)
          setOffset(nextOffset)
          setError(null)
        })
        .catch((e: Error) => {
          if (id !== requestId.current) return
          setError(e.message)
        })
        .finally(() => {
          if (id === requestId.current) setLoading(false)
        })
    },
    [filters, grouping],
  )

  useEffect(() => {
    runSearch(0, false)
  }, [runSearch])

  const activeFilterCount = useMemo(
    () =>
      filters.type.length +
      filters.base_model.length +
      filters.tag.length +
      (filters.present !== undefined ? 1 : 0) +
      (filters.needs_attention ? 1 : 0) +
      (filters.needs_update ? 1 : 0),
    [filters],
  )

  const hasMore = hits.length < total

  return (
    // One variable on the shell rather than a class per ratio: both grids read
    // it, and the fallback inside var() means a settings request that never
    // lands still gets portrait rather than an unstyled tile.
    <div className="app" style={{ '--thumb-aspect': thumbAspect } as CSSProperties}>
      <header className="topbar">
        <div className="brand">
          <svg viewBox="0 0 64 64" aria-hidden="true" className="brand-mark">
            <path
              d="M32 12 L50 22 L50 42 L32 52 L14 42 L14 22 Z"
              fill="none"
              stroke="currentColor"
              strokeWidth="3"
              strokeLinejoin="round"
            />
            <circle cx="32" cy="32" r="7" fill="currentColor" />
          </svg>
          <span>Model Manager</span>
        </div>

        <nav className="tabs">
          <button
            className={tab === 'library' ? 'on' : ''}
            onClick={() => setTab('library')}
          >
            Library
          </button>
          <button className={tab === 'browse' ? 'on' : ''} onClick={() => setTab('browse')}>
            Browse
          </button>
          <button className={tab === 'settings' ? 'on' : ''} onClick={() => setTab('settings')}>
            Settings
          </button>
        </nav>

        {tab === 'library' && (
          <>
            <input
              className="search"
              type="search"
              placeholder="Search name, trigger word, tag, filename…"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              autoComplete="off"
              spellCheck={false}
            />

            <button
              className={`filter-toggle${activeFilterCount ? ' has-filters' : ''}`}
              onClick={() => setFiltersOpen((v) => !v)}
              aria-expanded={filtersOpen}
            >
              Filters{activeFilterCount ? ` (${activeFilterCount})` : ''}
            </button>
          </>
        )}
      </header>

      {/* One-click type visibility. The sidebar still holds the full facet
          lists; this is the filter reached often enough that opening a panel
          for it is the wrong cost. A type with no models is dimmed rather than
          hidden, so the row does not reflow as the library fills up. */}
      {tab === 'library' && (
        <div className="type-chips">
          <button
            className={filters.type.length === 0 ? 'chip on' : 'chip'}
            onClick={() => setFilters({ ...filters, type: [] })}
          >
            All
          </button>
          {MODEL_TYPES.map((t) => {
            const count = facets?.types?.[t] ?? 0
            const on = filters.type.includes(t)
            return (
              <button
                key={t}
                className={`chip${on ? ' on' : ''}${count === 0 && !on ? ' faded' : ''}`}
                onClick={() =>
                  setFilters({
                    ...filters,
                    type: on ? filters.type.filter((x) => x !== t) : [...filters.type, t],
                  })
                }
              >
                {t}
                {count > 0 && <span className="chip-count">{count.toLocaleString()}</span>}
              </button>
            )
          })}
          {activeFilterCount > 0 && (
            <button
              className="chip chip-clear"
              onClick={() => {
                setFilters({ ...emptyFilters, q: filters.q, sort: filters.sort, order: filters.order })
                if (!config.readOnly) void deleteSetting(SETTING_FILTERS).catch(() => {})
              }}
            >
              Clear filters
            </button>
          )}
        </div>
      )}

      {config.readOnly && (
        <div className="banner">
          Read-only. Start the daemon with <code>--writable</code> to edit metadata.
        </div>
      )}

      {/* Kept mounted, like the library below: unmounting on tab switch would
          replay the mount effects (and their provider requests) on every
          Library-Browse round trip, and lose results, destination choice and
          the visible download queue mid-transfer. */}
      <BrowsePanel hidden={tab !== 'browse'} includeNSFW={includeNSFW} grouping={grouping} />

      <SettingsPanel
        hidden={tab !== 'settings'}
        onLibraryChanged={() => {
          runSearch(0, false)
          loadFacets()
        }}
        onPreferenceChanged={onPreferenceChanged}
      />

      <div className="body" hidden={tab !== 'library'}>
        <aside className={`sidebar${filtersOpen ? ' open' : ''}`}>
          <FilterPanel
            facets={facets}
            filters={filters}
            onChange={(f) => {
              setFilters(f)
              setFiltersOpen(false)
            }}
          />
        </aside>

        <main className="results">
          <div className="results-header">
            <span>
              {loading && hits.length === 0
                ? 'Searching…'
                : `${total.toLocaleString()} model${total === 1 ? '' : 's'}`}
            </span>
            <select
              value={`${filters.sort}:${filters.order}`}
              onChange={(e) => {
                const [sort, order] = e.target.value.split(':')
                setFilters({ ...filters, sort, order: order as 'asc' | 'desc' })
              }}
            >
              <option value="name:asc">Name A–Z</option>
              <option value="name:desc">Name Z–A</option>
              <option value="size:desc">Largest first</option>
              <option value="size:asc">Smallest first</option>
              <option value="added:desc">Recently added</option>
              <option value="recent:desc">Recently verified</option>
            </select>

            {/* Sweeps everything the current filters select, not the loaded
                page: only the filters are sent, and the server re-runs the
                query. Pairs with the "needs attention" filter, which is
                precisely the set worth pointing this at. */}
            {total > 0 && (
              <EnrichRunner
                filters={filters}
                expected={total}
                label={`Refresh these ${total.toLocaleString()} from origin`}
                className="inline"
                onFinished={() => {
                  runSearch(0, false)
                  loadFacets()
                }}
              />
            )}
          </div>

          {error && (
            <div className="error">
              {error}
              <button onClick={() => runSearch(0, false)}>Retry</button>
            </div>
          )}

          {!loading && !error && hits.length === 0 && (
            <div className="empty">
              {/* An empty library is the first thing a new user sees, so it is a
                  setup screen rather than a search result. "Nothing matches"
                  answers a question they did not ask -- they have not searched
                  for anything yet. */}
              {facets?.library_total === 0 ? (
                <>
                  <p>No model directories yet.</p>
                  <p className="hint">
                    Point Model Manager at a folder and it indexes what is
                    already there. Nothing is moved, renamed, or modified.
                  </p>
                  {config.readOnly ? (
                    <p className="hint">
                      This daemon is read-only, so directories cannot be added
                      from here. Restart it with <code>--writable</code>, or run{' '}
                      <code>mm scan --root /path/to/models</code>.
                    </p>
                  ) : (
                    <button className="primary" onClick={() => setTab('settings')}>
                      Add a model directory
                    </button>
                  )}
                </>
              ) : (
                <>
                  <p>Nothing matches.</p>
                  <p className="hint">Try clearing a filter or shortening the search.</p>
                </>
              )}
            </div>
          )}

          <div className="grid">
            {hits.map((hit) => (
              <ModelCard
                key={hit.sha256}
                hit={hit}
                selected={hit.sha256 === selected}
                onSelect={() => setSelected(hit.sha256)}
              />
            ))}
          </div>

          {hasMore && (
            <button
              className="load-more"
              disabled={loading}
              onClick={() => runSearch(offset + PAGE_SIZE, true)}
            >
              {loading ? 'Loading…' : `Load ${Math.min(PAGE_SIZE, total - hits.length)} more`}
            </button>
          )}
        </main>

        {selected && (
          <ModelDetailPanel
            sha={selected}
            onClose={() => setSelected(null)}
            onChanged={() => {
              runSearch(0, false)
              loadFacets()
            }}
          />
        )}
      </div>
    </div>
  )
}
