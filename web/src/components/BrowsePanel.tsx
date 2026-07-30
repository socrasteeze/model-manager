import { useCallback, useEffect, useState } from 'react'
import {
  browse,
  checkUpdates,
  emptyBrowseQuery,
  formatBytes,
  remoteImageURL,
  type BrowseQuery,
  type BrowseResults,
  type Listing,
  type RemoteFile,
  type Update,
} from '../api'
import { CopyButton } from './CopyButton'

const PROVIDERS = [
  { id: 'civitai', label: 'Civitai' },
  { id: 'civarchive', label: 'CivArchive' },
  { id: 'huggingface', label: 'HuggingFace' },
]

const TYPES = ['lora', 'checkpoint', 'lycoris', 'embedding', 'controlnet', 'vae']

export function BrowsePanel() {
  const [query, setQuery] = useState<BrowseQuery>(emptyBrowseQuery)
  const [draft, setDraft] = useState('')
  const [results, setResults] = useState<BrowseResults | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [updates, setUpdates] = useState<Update[] | null>(null)
  const [checking, setChecking] = useState(false)

  const run = useCallback((q: BrowseQuery) => {
    setLoading(true)
    setError(null)
    browse(q)
      .then(setResults)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false))
  }, [])

  useEffect(() => {
    run(query)
  }, [query, run])

  const toggle = (key: 'providers' | 'type', value: string) => {
    const current = query[key]
    setQuery({
      ...query,
      [key]: current.includes(value) ? current.filter((v) => v !== value) : [...current, value],
      page: 1,
    })
  }

  return (
    <div className="browse">
      <form
        className="browse-search"
        onSubmit={(e) => {
          e.preventDefault()
          setQuery({ ...query, q: draft, page: 1 })
        }}
      >
        <input
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
          placeholder="Search Civitai, CivArchive and HuggingFace…"
          aria-label="Search remote providers"
        />
        <button type="submit" disabled={loading}>
          {loading ? 'Searching…' : 'Search'}
        </button>
      </form>

      <div className="browse-filters">
        <div className="chip-row">
          {PROVIDERS.map((p) => (
            <button
              key={p.id}
              className={`chip${query.providers.includes(p.id) ? ' on' : ''}`}
              onClick={() => toggle('providers', p.id)}
            >
              {p.label}
            </button>
          ))}
        </div>
        <div className="chip-row">
          {TYPES.map((t) => (
            <button
              key={t}
              className={`chip${query.type.includes(t) ? ' on' : ''}`}
              onClick={() => toggle('type', t)}
            >
              {t}
            </button>
          ))}
        </div>
        <div className="chip-row">
          <select
            value={query.sort}
            onChange={(e) => setQuery({ ...query, sort: e.target.value, page: 1 })}
            aria-label="Sort order"
          >
            <option value="downloads">Most downloaded</option>
            <option value="newest">Newest</option>
            <option value="updated">Recently updated</option>
            <option value="rating">Highest rated</option>
            <option value="relevance">Relevance</option>
          </select>
          <label className="check inline">
            <input
              type="checkbox"
              checked={query.nsfw}
              onChange={(e) => setQuery({ ...query, nsfw: e.target.checked, page: 1 })}
            />
            <span>Include adult content</span>
          </label>
          <button
            className="link"
            disabled={checking}
            onClick={() => {
              setChecking(true)
              checkUpdates()
                .then((r) => setUpdates(r.updates))
                .catch((e: Error) => setError(e.message))
                .finally(() => setChecking(false))
            }}
          >
            {checking ? 'Checking…' : 'Check for updates'}
          </button>
        </div>
      </div>

      {error && <div className="error">{error}</div>}

      {/* A provider being unreachable has to be visible. Silently dropping it
          looks identical to that provider having no matches. */}
      {results?.errors &&
        Object.entries(results.errors).map(([id, msg]) => (
          <div key={id} className="error inline">
            {id} unavailable: {msg}
          </div>
        ))}

      {updates && <UpdateList updates={updates} onClose={() => setUpdates(null)} />}

      <div className="listing-grid">
        {results?.items.map((l) => (
          <ListingCard key={`${l.provider}:${l.id}:${l.version_id ?? ''}`} listing={l} />
        ))}
      </div>

      {results && results.items.length === 0 && !loading && (
        <p className="source-note">No results.</p>
      )}

      {results && results.items.length > 0 && (
        <div className="browse-paging">
          <button
            disabled={query.page <= 1 || loading}
            onClick={() => setQuery({ ...query, page: query.page - 1 })}
          >
            Previous
          </button>
          <span>Page {query.page}</span>
          <button disabled={loading} onClick={() => setQuery({ ...query, page: query.page + 1 })}>
            Next
          </button>
        </div>
      )}
    </div>
  )
}

function ListingCard({ listing }: { listing: Listing }) {
  const status = listing.local?.status ?? 'new'
  const file = pickFile(listing.files)

  return (
    <article className={`listing ${status}`}>
      {listing.thumbnail_url && (
        <img
          className="listing-thumb"
          src={remoteImageURL(listing.thumbnail_url)}
          alt=""
          loading="lazy"
        />
      )}

      <div className="listing-body">
        <div className="listing-head">
          <h3>{listing.name}</h3>
          <StatusBadge listing={listing} />
        </div>

        <div className="listing-meta">
          <span className="provider-badge">{listing.provider}</span>
          {listing.version_name && <span>{listing.version_name}</span>}
          {listing.type && <span>{listing.type}</span>}
          {listing.base_model && <span>{listing.base_model}</span>}
          {file?.size_bytes ? <span>{formatBytes(file.size_bytes)}</span> : null}
          {listing.nsfw && <span className="nsfw-badge">nsfw</span>}
        </div>

        {listing.trigger_words && listing.trigger_words.length > 0 && (
          <div className="trigger-list">
            {listing.trigger_words.slice(0, 4).map((wrd) => (
              <CopyButton key={wrd} value={wrd} className="trigger" />
            ))}
          </div>
        )}

        {/* A pickle executes code when loaded. Whether to accept that is the
            user's call, but it must not be a surprise made after the fact. */}
        {file && isPickle(file) && (
          <p className="warn-note">
            This is a pickle format file, which runs code when loaded. Prefer a safetensors
            version where one exists.
          </p>
        )}

        <div className="listing-actions">
          {file?.download_url && status !== 'have' && (
            <CopyButton
              value={`mm get ${file.download_url}`}
              label="Copy download command"
              className="ghost"
            />
          )}
          {listing.page_url && (
            <a href={listing.page_url} target="_blank" rel="noreferrer noopener" className="ghost">
              Open page
            </a>
          )}
          {file?.requires_auth && <span className="source-note">needs an API key</span>}
        </div>
      </div>
    </article>
  )
}

function StatusBadge({ listing }: { listing: Listing }) {
  const local = listing.local
  if (!local || local.status === 'new') return <span className="badge new-badge">new</span>

  if (local.status === 'have') {
    return (
      <span className="badge have-badge" title={local.path || 'already in the library'}>
        have
      </span>
    )
  }
  return (
    <span
      className="badge update-badge"
      title={local.have_version_name ? `you have ${local.have_version_name}` : 'newer version'}
    >
      update
    </span>
  )
}

function UpdateList({ updates, onClose }: { updates: Update[]; onClose: () => void }) {
  return (
    <section className="updates">
      <div className="updates-head">
        <h3>{updates.length === 0 ? 'Everything is up to date' : `${updates.length} update(s)`}</h3>
        <button className="link" onClick={onClose}>
          Dismiss
        </button>
      </div>
      {updates.map((u) => (
        <div key={`${u.provider}:${u.model_id}`} className="update-row">
          <div>
            <strong>{u.name}</strong>
            <span className="source-note">
              {' '}
              {u.have_version_name || 'current'} → {u.latest_version_name || u.latest_version_id}
            </span>
            {/* A LoRA rebuilt onto a different base is published as a new
                version of the same model but is not a drop-in replacement. */}
            {u.base_model_changed && (
              <div className="warn-note">
                Base model changed to {u.base_model} — not a drop-in replacement.
              </div>
            )}
          </div>
          {u.download_url && (
            <CopyButton value={`mm get ${u.download_url}`} label="Copy command" className="ghost" />
          )}
        </div>
      ))}
    </section>
  )
}

// pickFile mirrors the server's preference: the provider's primary file, then a
// safetensors, then the largest. Kept in sync deliberately so the size and
// warning shown are for the file the copied command would actually fetch.
function pickFile(files?: RemoteFile[]): RemoteFile | undefined {
  if (!files || files.length === 0) return undefined
  const score = (f: RemoteFile) => (f.primary ? 4 : 0) + (isSafe(f) ? 2 : 0)
  return [...files].sort((a, b) => score(b) - score(a) || (b.size_bytes ?? 0) - (a.size_bytes ?? 0))[0]
}

function isSafe(f: RemoteFile): boolean {
  const name = f.name?.toLowerCase() ?? ''
  return (
    f.format?.toLowerCase() === 'safetensor' ||
    name.endsWith('.safetensors') ||
    name.endsWith('.gguf')
  )
}

function isPickle(f: RemoteFile): boolean {
  const name = f.name?.toLowerCase() ?? ''
  return (
    f.format?.toLowerCase() === 'pickletensor' ||
    name.endsWith('.ckpt') ||
    name.endsWith('.pt') ||
    name.endsWith('.pth') ||
    name.endsWith('.bin')
  )
}
