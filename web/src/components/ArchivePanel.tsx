import { useCallback, useEffect, useState } from 'react'
import {
  addWatch,
  archiveComplete,
  archiveItems,
  archiveStatus,
  archivePull,
  listWatches,
  removeWatch,
  relativeTimeOrEmpty,
  type ArchiveItem,
  type ArchiveStatus,
  type ArchiveWatch,
} from '../api'

type View = 'all' | 'incomplete' | 'gone'

/**
 * The archive: what has been deliberately captured from a provider, how
 * complete each capture is, and what has since disappeared upstream.
 *
 * The gone view is a first-class filter rather than something to construct by
 * hand, because "what did I save that no longer exists" is the archive's reason
 * to exist — everything else here is bookkeeping in service of it.
 */
export function ArchivePanel() {
  const [status, setStatus] = useState<ArchiveStatus | null>(null)
  const [items, setItems] = useState<ArchiveItem[]>([])
  const [watches, setWatches] = useState<ArchiveWatch[]>([])
  const [view, setView] = useState<View>('all')
  const [modelID, setModelID] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const refresh = useCallback(() => {
    archiveStatus().then(setStatus).catch(() => setStatus(null))
    archiveItems({ incomplete: view === 'incomplete', gone: view === 'gone' })
      .then((r) => setItems(r.items))
      .catch(() => setItems([]))
    listWatches()
      .then((r) => setWatches(r.watches))
      .catch(() => setWatches([]))
  }, [view])

  useEffect(refresh, [refresh])

  // Poll only while a run is going, for the same reason the download queue does:
  // a finished job must not keep the daemon answering for a tab nobody is on.
  useEffect(() => {
    if (status?.job?.state !== 'running') return
    const t = setInterval(refresh, 1000)
    return () => clearInterval(t)
  }, [status?.job?.state, refresh])

  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      refresh()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const job = status?.job
  const counts = status?.counts

  return (
    <section className="settings-block">
      <h2>Archive</h2>
      <p className="hint">
        A model archived here keeps its file, the provider&rsquo;s raw responses, every metadata
        candidate and every preview — so if it is taken down later, nothing is lost. Intake is
        per model and deliberate; nothing is crawled.
      </p>

      {!status?.available && (
        <div className="hint">
          {status?.unavailable_because ??
            'Archiving is not available on this daemon.'}{' '}
          Start it with <code>--writable --allow-archive</code>.
        </div>
      )}

      {counts && (
        <dl className="upstream-status">
          <div>
            <dt>Archived</dt>
            <dd>{counts.items}</dd>
          </div>
          <div>
            <dt>Complete</dt>
            <dd>{counts.complete}</dd>
          </div>
          <div>
            <dt>Unfinished</dt>
            <dd>{counts.incomplete}</dd>
          </div>
          <div>
            <dt>Gone upstream</dt>
            <dd>{counts.gone}</dd>
          </div>
          <div>
            <dt>Watched</dt>
            <dd>{counts.watched}</dd>
          </div>
        </dl>
      )}

      {status?.available && (
        <div className="root-add">
          <input
            type="text"
            inputMode="numeric"
            placeholder="Civitai model id"
            value={modelID}
            onChange={(e) => setModelID(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && modelID.trim()) {
                run(() => archivePull({ model_id: modelID.trim(), watch: true }))
                setModelID('')
              }
            }}
            spellCheck={false}
            disabled={busy}
          />
          <button
            disabled={busy || !modelID.trim()}
            onClick={() => {
              run(() => archivePull({ model_id: modelID.trim(), watch: true }))
              setModelID('')
            }}
          >
            Archive
          </button>
        </div>
      )}

      {job && job.state === 'running' && (
        <div className="notice">
          Archiving {job.done}/{job.total}
          {job.rate_limited && ' — the provider is rate limiting; this will resume on re-run'}
        </div>
      )}
      {job?.last_error && <div className="hint">Last problem: {job.last_error}</div>}
      {error && <div className="error">{error}</div>}

      <div className="chip-row">
        {(['all', 'incomplete', 'gone'] as View[]).map((v) => (
          <button
            key={v}
            className={`chip${view === v ? ' on' : ''}`}
            onClick={() => setView(v)}
          >
            {v === 'all' && 'Everything'}
            {v === 'incomplete' && 'Unfinished'}
            {v === 'gone' && 'Gone upstream'}
          </button>
        ))}
      </div>

      {items.length === 0 ? (
        <p className="source-note">
          {view === 'gone'
            ? 'Nothing archived here has disappeared upstream yet.'
            : 'Nothing archived yet.'}
        </p>
      ) : (
        <table className="archive-table">
          <thead>
            <tr>
              <th>Model</th>
              <th>File</th>
              <th>Metadata</th>
              <th>Responses</th>
              <th>Previews</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {items.map((a) => (
              <tr key={`${a.provider}/${a.model_id}/${a.version_id}`}>
                <td>
                  <code>
                    {a.model_id}
                    <span className="muted">/{a.version_id}</span>
                  </code>
                  {a.upstream_gone_at && (
                    <div className="badge warn-badge" title={`Removed upstream ${a.upstream_gone_at}`}>
                      gone upstream {relativeTimeOrEmpty(a.upstream_gone_at)}
                    </div>
                  )}
                  {a.last_error && <div className="hint">{a.last_error}</div>}
                </td>
                {/* data-label carries the column heading into the cell, which
                    is what lets each row collapse into a labelled card on a
                    phone -- six columns of unbreakable headings cannot fit a
                    375px screen, and scrolling them sideways would separate
                    each flag from the model it describes. */}
                <td data-label="File">{a.file_ok ? 'yes' : 'no'}</td>
                <td data-label="Metadata">{a.meta_ok ? 'yes' : 'no'}</td>
                <td data-label="Responses">{a.origin_cache_ok ? 'yes' : 'no'}</td>
                <td data-label="Previews">
                  {a.previews_got}/{a.previews_total}
                </td>
                <td data-label="">
                  {!archiveComplete(a) && status?.available && (
                    <button
                      className="ghost"
                      disabled={busy}
                      title="Fetches only the parts that are still missing"
                      onClick={() =>
                        run(() =>
                          archivePull({
                            provider: a.provider,
                            model_id: a.model_id,
                            version_id: a.version_id,
                          }),
                        )
                      }
                    >
                      Finish
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      <h3>Watchlist</h3>
      <p className="hint">
        A watched model is checked for new versions on a timer. Automatic download is off by
        default: a watch is a subscription to information, not to unattended multi-gigabyte
        transfers.
      </p>
      {watches.length === 0 ? (
        <p className="source-note">Nothing watched.</p>
      ) : (
        <ul className="watch-list">
          {watches.map((w) => (
            <li key={`${w.provider}/${w.model_id}`}>
              <code>{w.model_id}</code>
              <span className="muted">
                {w.last_checked ? ` checked ${relativeTimeOrEmpty(w.last_checked)}` : ' never checked'}
              </span>
              <label className="setting-row">
                <input
                  type="checkbox"
                  checked={w.auto_pull}
                  disabled={busy || !status?.available}
                  onChange={(e) =>
                    run(() =>
                      addWatch({
                        provider: w.provider,
                        model_id: w.model_id,
                        auto_pull: e.target.checked,
                      }),
                    )
                  }
                />
                <span>fetch new versions automatically</span>
              </label>
              <button
                className="ghost"
                disabled={busy}
                onClick={() => run(() => removeWatch(w.provider, w.model_id))}
              >
                Stop watching
              </button>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}
