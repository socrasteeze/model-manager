import { useCallback, useEffect, useState } from 'react'
import {
  formatBytes,
  listPulls,
  upstreamStatus,
  type PullsSummary,
  type UpstreamStatus,
} from '../api'

/**
 * Status of the upstream library, and what to do about it when it is not
 * working.
 *
 * Read-only by design. The upstream is configured through the environment
 * rather than through settings -- a base URL the API could write would turn a
 * settings request into "fetch an arbitrary URL and write it into a model
 * folder" -- so this panel explains the configuration and never edits it.
 *
 * Four separate questions get four separate answers, because they fail
 * independently and each has a different fix: is one named, can we reach it,
 * does it accept our token, and will it hand over files.
 */
export function UpstreamCard({ onFilterEvicted }: { onFilterEvicted?: () => void }) {
  const [status, setStatus] = useState<UpstreamStatus | null>(null)
  const [pulls, setPulls] = useState<PullsSummary | null>(null)
  const [checking, setChecking] = useState(false)

  const refresh = useCallback(() => {
    setChecking(true)
    upstreamStatus()
      .then(setStatus)
      .catch(() => setStatus(null))
      .finally(() => setChecking(false))
    listPulls()
      .then(setPulls)
      .catch(() => setPulls(null))
  }, [])

  useEffect(refresh, [refresh])

  return (
    <section className="settings-block">
      <h2>Upstream library</h2>
      <p className="hint">
        Another Model Manager — usually the machine holding the whole collection. With one
        configured you can browse it beside Civitai and pull models onto this machine, and
        only that machine needs a provider API key.
      </p>

      {!status?.configured ? (
        <div className="hint">
          <p>No upstream configured. Set these in the environment that launches the daemon:</p>
          <ul className="upstream-env">
            <li>
              <code>MM_UPSTREAM_URL</code> — e.g. <code>http://nas:8737</code>
            </li>
            <li>
              <code>MM_UPSTREAM_TOKEN</code> — the <code>api-token</code> file beside that
              daemon&rsquo;s database, needed whenever it is not on loopback
            </li>
            <li>
              <code>MM_UPSTREAM_NAME</code> — optional display name
            </li>
          </ul>
          <p>
            On that machine, start the daemon with <code>--serve-files</code> so it will hand
            model files over.
          </p>
        </div>
      ) : (
        <>
          <dl className="upstream-status">
            <div>
              <dt>Library</dt>
              <dd>
                {status.name}
                {status.host && status.host !== status.name && (
                  <span className="muted"> ({status.host})</span>
                )}
              </dd>
            </div>
            <div>
              <dt>Reachable</dt>
              <dd>{status.reachable ? 'yes' : 'no'}</dd>
            </div>
            <div>
              <dt>Credentials</dt>
              <dd>{status.authenticated ? 'accepted' : 'not accepted'}</dd>
            </div>
            <div>
              <dt>Serves files</dt>
              <dd>
                {status.files === 'yes' && 'yes'}
                {status.files === 'no' && 'no'}
                {status.files === 'unknown' && 'unknown (older version)'}
              </dd>
            </div>
            {status.version && (
              <div>
                <dt>Version</dt>
                <dd>{status.version}</dd>
              </div>
            )}
          </dl>

          {status.can_pull ? (
            <div className="notice">Ready. Pull models from the Browse tab.</div>
          ) : (
            status.error && <div className="error">{status.error}</div>
          )}
        </>
      )}

      <div className="root-add">
        <button onClick={refresh} disabled={checking}>
          {checking ? 'Checking…' : 'Test connection'}
        </button>
      </div>

      {pulls && pulls.pulls.length > 0 && (
        <p className="hint">
          {pulls.pulls.length} model{pulls.pulls.length === 1 ? '' : 's'} on this machine came
          from an upstream, taking {formatBytes(pulls.reclaimable_bytes)}.{' '}
          {pulls.evict_available ? (
            <>
              Any of them can be evicted from its detail panel to reclaim that space; the
              library keeps everything it knows and lists the model as available again.
            </>
          ) : (
            <>
              Start the daemon with <code>--writable --allow-evict</code> to reclaim that space
              without losing the records.
            </>
          )}{' '}
          {onFilterEvicted && (
            <button className="linklike" onClick={onFilterEvicted}>
              Show what is not on this machine
            </button>
          )}
        </p>
      )}
    </section>
  )
}
