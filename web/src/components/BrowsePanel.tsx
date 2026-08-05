import { useCallback, useEffect, useState } from 'react'
import {
  browse,
  cancelDownload,
  listUpdates,
  startUpdateSweep,
  config,
  downloadRoots,
  emptyBrowseQuery,
  formatBytes,
  isJobActive,
  isJobTerminalFailure,
  listDownloads,
  relativeTimeOrEmpty,
  remoteImageURL,
  startDownload,
  type BrowseQuery,
  type BrowseResults,
  type DownloadJob,
  type Listing,
  type RemoteFile,
  MODEL_TYPES,
  type Update,
} from '../api'
import { CopyButton } from './CopyButton'
import { DestinationHint } from './DestinationHint'

const PROVIDERS = [
  { id: 'civitai', label: 'Civitai' },
  { id: 'civarchive', label: 'CivArchive' },
  { id: 'huggingface', label: 'HuggingFace' },
]

// The canonical set, from the server. It used to be a shorter hand-written
// list here, and this same string decided a directory name -- so a type the
// server did not recognise became a folder named after it.
const TYPES = MODEL_TYPES

interface Props {
  hidden?: boolean

  /**
   * Whether adult results are included, from the stored preference.
   *
   * A prop rather than local state because it is a standing preference, not a
   * per-search filter -- it only ever felt like a filter because it defaulted
   * off and did not survive a reload, so the checkbox that used to live in the
   * filter row is gone and Settings owns it.
   */
  includeNSFW: boolean
}

export function BrowsePanel({ hidden, includeNSFW }: Props) {
  const [query, setQuery] = useState<BrowseQuery>(emptyBrowseQuery)
  const [draft, setDraft] = useState('')
  const [results, setResults] = useState<BrowseResults | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [updates, setUpdates] = useState<Update[] | null>(null)
  const [checking, setChecking] = useState(false)
  const [roots, setRoots] = useState<string[]>([])
  const [destRoot, setDestRoot] = useState('')
  const [jobs, setJobs] = useState<DownloadJob[]>([])

  // Downloading needs a writable server and at least one scanned root to put
  // things in; without both the UI offers the command to run instead.
  const canDownload = !config.readOnly && roots.length > 0

  useEffect(() => {
    downloadRoots()
      .then((r) => {
        setRoots(r)
        setDestRoot((cur) => cur || r[0] || '')
      })
      .catch(() => setRoots([]))
  }, [])

  // Poll only while something is in flight. A finished queue must not keep the
  // daemon busy answering for a tab nobody is looking at.
  const active = jobs.some(isJobActive)
  useEffect(() => {
    if (!canDownload) return
    const tick = () => listDownloads().then(setJobs).catch(() => {})
    tick()
    if (!active) return
    const timer = setInterval(tick, 1000)
    return () => clearInterval(timer)
  }, [canDownload, active])

  const [jobIdByUrl, setJobIdByUrl] = useState<Record<string, string>>({})

  const run = useCallback((q: BrowseQuery) => {
    setLoading(true)
    setError(null)
    browse(q)
      .then(setResults)
      .catch((e: Error) => setError(e.message))
      .finally(() => setLoading(false))
  }, [])

  // Show whatever the last sweep recorded as soon as the tab is opened. This
  // read is free -- it hits stored data, not the provider -- so there is no
  // reason to make the user press a button to find out what is already known.
  const [checkProgress, setCheckProgress] = useState('')
  useEffect(() => {
    if (hidden) return
    listUpdates()
      .then((r) => setUpdates(r.updates.length > 0 ? r.updates : null))
      .catch(() => {})
  }, [hidden])

  // Start a sweep, then poll the same endpoint until it stops running.
  //
  // The button used to call a GET that performed the whole check inline and
  // returned the answer. Now the check is a background job, so this is
  // POST-then-poll -- and the result it lands on is stored, which is what lets
  // the library badge every affected model rather than showing a list that
  // vanishes when this panel unmounts.
  const runUpdateCheck = useCallback(async () => {
    setChecking(true)
    setError(null)
    setCheckProgress('')
    try {
      await startUpdateSweep()
    } catch (e) {
      // A 409 means one is already running -- adopt it and poll, rather than
      // reporting an error for something that is going to produce an answer.
      const msg = (e as Error).message
      if (!/already in progress/i.test(msg)) {
        setError(msg)
        setChecking(false)
        return
      }
    }

    try {
      for (;;) {
        const r = await listUpdates()
        setUpdates(r.updates)
        if (!r.job || r.job.state !== 'running') {
          if (r.job?.rate_limited) {
            setError(
              'The origin started rate limiting, so not every model was checked. Run it again to continue.',
            )
          }
          return
        }
        if (r.job.models_total > 0) {
          setCheckProgress(` ${r.job.models_done}/${r.job.models_total}`)
        }
        await new Promise((resolve) => setTimeout(resolve, 1000))
      }
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setChecking(false)
      setCheckProgress('')
    }
  }, [])

  // The preference is the only source of truth for adult results. This was a
  // checkbox in the filter row, which meant a choice that lasted until reload
  // and then silently disagreed with the one in Settings.
  useEffect(() => {
    setQuery((q) => (q.nsfw === includeNSFW ? q : { ...q, nsfw: includeNSFW, page: 1 }))
  }, [includeNSFW])

  useEffect(() => {
    // An untouched query is not a search: auto-firing an empty three-provider
    // sweep on mount costs real requests against rate-limited public APIs for
    // a tab the user may only be glancing at. Anything beyond the defaults --
    // text, a filter, a page, a sort change -- runs normally.
    //
    // nsfw is deliberately not part of this test. It used to be, back when it
    // was a checkbox in the filter row and ticking it was a search action. It
    // is a standing preference now, so it says nothing about whether the user
    // has asked for anything -- and testing it either way is a bug: against
    // false it reads as touched the moment the preference is on, and against
    // the preference it reads as touched for the one render before the two
    // agree. Both fire the empty sweep this guard exists to prevent.
    if (
      query.q === '' &&
      query.providers.length === 0 &&
      query.type.length === 0 &&
      query.base_model.length === 0 &&
      query.page === 1 &&
      query.sort === emptyBrowseQuery.sort
    ) {
      setResults(null)
      return
    }
    run(query)
  }, [query, run])

  // Functional updates: two quick clicks in one React batch must not build
  // the second state from the first's stale snapshot.
  const toggle = (key: 'providers' | 'type', value: string) => {
    setQuery((q) => ({
      ...q,
      [key]: q[key].includes(value) ? q[key].filter((v) => v !== value) : [...q[key], value],
      page: 1,
    }))
  }

  return (
    <div className="browse" hidden={hidden}>
      <form
        className="browse-search"
        onSubmit={(e) => {
          e.preventDefault()
          setQuery((q) => ({ ...q, q: draft, page: 1 }))
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
            onChange={(e) => setQuery((q) => ({ ...q, sort: e.target.value, page: 1 }))}
            aria-label="Sort order"
          >
            <option value="downloads">Most downloaded</option>
            <option value="newest">Newest</option>
            <option value="updated">Recently updated</option>
            <option value="rating">Highest rated</option>
            <option value="relevance">Relevance</option>
          </select>
          {/* Shown only when results are being withheld. The default is on, so
              saying so in that case would be noise in the most-used row. */}
          {!includeNSFW && (
            <span className="source-note">Adult results are hidden — change this in Settings.</span>
          )}
          <button
            className="link"
            disabled={checking}
            onClick={() => void runUpdateCheck()}
          >
            {checking ? `Checking${checkProgress}…` : 'Check for updates'}
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

      {canDownload ? (
        <div className="dest-row">
          <label>
            Download to
            <select value={destRoot} onChange={(e) => setDestRoot(e.target.value)}>
              {roots.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </label>
          {/* The per-type subfolder is resolved server-side, so the first
              search result's type is what the hint previews. */}
          <DestinationHint root={destRoot} type={results?.items[0]?.type} />
          {jobs.length > 0 && <DownloadQueue jobs={jobs} />}
        </div>
      ) : (
        <p className="source-note">
          {config.readOnly
            ? 'Read-only: start the daemon with --writable to download from here.'
            : 'No scanned model roots yet, so there is nowhere to download to. Run a scan first.'}
        </p>
      )}

      <div className="listing-grid">
        {results?.items.map((l) => (
          <ListingCard
            key={`${l.provider}:${l.id}:${l.version_id ?? ''}`}
            listing={l}
            destRoot={destRoot}
            canDownload={canDownload}
            job={jobFor(jobs, l, jobIdByUrl)}
            onStarted={(id, url) => {
              if (id && url) setJobIdByUrl((m) => ({ ...m, [url]: id }))
              listDownloads().then(setJobs).catch(() => {})
            }}
          />
        ))}
      </div>

      {results && results.items.length === 0 && !loading && (
        <p className="source-note">No results.</p>
      )}

      {results && (results.items.length > 0 || query.page > 1) && (
        <div className="browse-paging">
          <button
            disabled={query.page <= 1 || loading}
            onClick={() => setQuery((q) => ({ ...q, page: q.page - 1 }))}
          >
            Previous
          </button>
          <span>Page {query.page}</span>
          <button disabled={loading} onClick={() => setQuery((q) => ({ ...q, page: q.page + 1 }))}>
            Next
          </button>
        </div>
      )}
    </div>
  )
}

function ListingCard({
  listing,
  destRoot,
  canDownload,
  job,
  onStarted,
}: {
  listing: Listing
  destRoot: string
  canDownload: boolean
  job?: DownloadJob
  onStarted: (id?: string, url?: string) => void
}) {
  const status = listing.local?.status ?? 'new'
  const file = pickFile(listing.files)
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  // Shared by the Download and Retry buttons: same request, different label.
  const start = async () => {
    if (!file?.download_url) return
    setBusy(true)
    setErr(null)
    try {
      const res = await startDownload({
        url: file.download_url,
        dest_root: destRoot,
        // No subdir: the server decides it from (root, type). The browser used
        // to pluralize the provider's type string into `${type}s`, which
        // produced `vaes/` and `lycoriss/` and assumed one folder vocabulary
        // when the three tools on this machine use three.
        type: listing.type,
        filename: file.name,
        sha256: file.sha256,
        size: file.size_bytes,
      })
      onStarted(res.id, file.download_url)
    } catch (e) {
      setErr((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <article className={`listing ${status}`}>
      {/* The frame is always rendered. A listing with no preview used to
          render no element at all, so the card collapsed to a text block and
          the row went ragged; the library has shown type initials in this case
          since the start, and browse now matches it. */}
      <div className="listing-thumb">
        {listing.thumbnail_url ? (
          <img src={remoteImageURL(listing.thumbnail_url)} alt="" loading="lazy" />
        ) : (
          <div className="placeholder" aria-hidden="true">
            {(listing.type || '?').slice(0, 2).toUpperCase()}
          </div>
        )}
      </div>

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
          {/* Describes the file, not an action. It sat in the button row,
              where it was the one thing that could still wrap it. */}
          {file?.requires_auth && <span>needs an API key</span>}
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

        {job && <JobProgress job={job} />}
        {err && <div className="error inline">{err}</div>}
      </div>

      {/* A sibling of the body, not a child: grid areas only apply to direct
          children, and this row needs the card's full width to fit its three
          buttons on one line. */}
      <div className="listing-actions">
          {file?.download_url && status !== 'have' && canDownload && !job && (
            <button className="primary" disabled={busy} onClick={start}>
              {busy ? 'Starting…' : 'Download'}
            </button>
          )}
          {/* A failed, quarantined or cancelled job is not the end of the
              story: the server allows a terminal ID to start again (with a
              quarantined partial already moved aside), so the UI must offer
              the retry rather than hiding the button forever. */}
          {job && isJobTerminalFailure(job) && canDownload && (
            <button className="primary" disabled={busy} onClick={start}>
              {busy ? 'Starting…' : 'Retry'}
            </button>
          )}
          {job && isJobActive(job) && (
            <button
              className="ghost"
              disabled={busy}
              onClick={async () => {
                setBusy(true)
                try {
                  await cancelDownload(job.id)
                  onStarted()
                } finally {
                  setBusy(false)
                }
              }}
            >
              Cancel
            </button>
          )}
          {file?.download_url && status !== 'have' && (
            <CopyButton
              value={`mm get ${file.download_url}`}
              label="Copy command"
              className="ghost"
            />
          )}
          {listing.page_url && (
            <a href={listing.page_url} target="_blank" rel="noreferrer noopener" className="ghost">
              Open page
            </a>
          )}
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
        // Keyed by the local file, not the remote model: two owned versions of
        // one model are two rows, and a model key would collide.
        <div key={u.sha256} className="update-row">
          <div>
            <strong>{u.name || u.local_path?.split(/[/\\]/).pop() || u.model_id}</strong>
            <span className="source-note">
              {' '}
              {u.have_version_name || 'current'} → {u.latest_version_name || u.latest_version_id}
              {u.checked_at && ` (checked ${relativeTimeOrEmpty(u.checked_at)})`}
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

// jobFor resolves a listing's download job: by the ID the server returned at
// start when we have it, else by URL for jobs started elsewhere (another tab,
// the CLI). URL matching alone was fragile -- the server stores the re-encoded
// URL, and any mismatch resurrected the Download button mid-transfer.
function jobFor(
  jobs: DownloadJob[],
  listing: Listing,
  idByUrl: Record<string, string>,
): DownloadJob | undefined {
  const urls = (listing.files ?? []).map((f) => f.download_url).filter(Boolean) as string[]
  for (const u of urls) {
    const id = idByUrl[u]
    if (id) {
      const byID = jobs.find((j) => j.id === id)
      if (byID) return byID
    }
  }
  const set = new Set(urls)
  return jobs.find((j) => set.has(j.url))
}

function JobProgress({ job }: { job: DownloadJob }) {
  const pct = job.total > 0 ? Math.min(100, (job.downloaded / job.total) * 100) : 0

  if (job.state === 'complete') {
    return (
      <div>
        <p className="source-note">Downloaded to {job.final_path}</p>
        {job.index_error && (
          <p className="warn-note">
            Downloaded but not indexed yet — it will appear after the next scan. ({job.index_error})
          </p>
        )}
      </div>
    )
  }
  if (job.state === 'cancelled') {
    return <p className="source-note">Cancelled — partial kept; Retry resumes where it stopped.</p>
  }
  if (job.state === 'failed' || job.state === 'quarantined') {
    // Quarantined means the bytes arrived but the hash did not match, so the
    // file was never published into the model root.
    return (
      <p className="warn-note">
        {job.state === 'quarantined' ? 'Checksum mismatch — not installed. ' : 'Failed. '}
        {job.error}
      </p>
    )
  }
  return (
    <div className="progress">
      <div className="progress-bar">
        <span style={{ width: `${pct}%` }} />
      </div>
      <span className="progress-label">
        {job.state === 'verifying'
          ? 'verifying…'
          : `${formatBytes(job.downloaded)}${job.total > 0 ? ` / ${formatBytes(job.total)}` : ''}`}
      </span>
    </div>
  )
}

function DownloadQueue({ jobs }: { jobs: DownloadJob[] }) {
  const done = jobs.filter((j) => j.state === 'complete').length
  const failed = jobs.filter((j) => isJobTerminalFailure(j)).length
  const running = jobs.filter(isJobActive).length
  return (
    <span className="source-note">
      {running > 0 && `${running} downloading`}
      {running > 0 && (done > 0 || failed > 0) && ' · '}
      {done > 0 && `${done} done`}
      {failed > 0 && ` · ${failed} failed/stopped`}
    </span>
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
