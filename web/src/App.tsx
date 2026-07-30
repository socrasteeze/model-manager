import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  config,
  emptyFilters,
  getFacets,
  searchModels,
  type Facets,
  type Filters,
  type SearchHit,
} from './api'
import { BrowsePanel } from './components/BrowsePanel'
import { FilterPanel } from './components/FilterPanel'
import { ModelCard } from './components/ModelCard'
import { ModelDetailPanel } from './components/ModelDetailPanel'

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
  const [tab, setTab] = useState<'library' | 'browse'>('library')

  // Debounce the text box rather than the whole filter object: clicking a facet
  // should feel instant, while typing should not fire a request per keystroke.
  useEffect(() => {
    const timer = setTimeout(() => setFilters((f) => ({ ...f, q: query })), 200)
    return () => clearTimeout(timer)
  }, [query])

  const loadFacets = useCallback(() => {
    getFacets().then(setFacets).catch(() => {
      /* facets are a convenience; their absence must not break search */
    })
  }, [])

  useEffect(loadFacets, [loadFacets])

  // A request counter, so a slow early response cannot overwrite a fast later
  // one and show results for a query the user has already moved on from.
  const requestId = useRef(0)

  const runSearch = useCallback(
    (nextOffset: number, append: boolean) => {
      const id = ++requestId.current
      setLoading(true)
      searchModels(filters, nextOffset, PAGE_SIZE)
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
    [filters],
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
      (filters.needs_attention ? 1 : 0),
    [filters],
  )

  const hasMore = hits.length < total

  return (
    <div className="app">
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

      {config.readOnly && (
        <div className="banner">
          Read-only. Start the daemon with <code>--writable</code> to edit metadata.
        </div>
      )}

      {tab === 'browse' && <BrowsePanel />}

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
          </div>

          {error && (
            <div className="error">
              {error}
              <button onClick={() => runSearch(0, false)}>Retry</button>
            </div>
          )}

          {!loading && !error && hits.length === 0 && (
            <div className="empty">
              <p>Nothing matches.</p>
              {total === 0 && facets?.total === 0 ? (
                <p className="hint">
                  The index is empty. Run <code>mm scan --root /path/to/models</code>, then{' '}
                  <code>mm interpret</code>.
                </p>
              ) : (
                <p className="hint">Try clearing a filter or shortening the search.</p>
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
