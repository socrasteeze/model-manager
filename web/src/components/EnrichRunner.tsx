import { config } from '../api'
import { relativeTime, useEnrichFinished, useEnrichJob } from '../hooks/useEnrichJob'
import type { Filters } from '../api'

interface Props {
  /**
   * Filters to sweep. Omitted means the whole library.
   *
   * Only the filters are sent, never a list of hashes: the server re-runs the
   * same query, so the sweep covers every match rather than the page on screen.
   */
  filters?: Filters

  /** How many models the sweep is expected to cover, for the confirm step. */
  expected?: number

  label: string
  className?: string

  /** Called when a run finishes, so the caller can re-read what changed. */
  onFinished?: () => void
}

/**
 * A button for the shared enrichment sweep, plus its progress and outcome.
 *
 * The polling, job state and error state all live in EnrichJobProvider (see
 * hooks/useEnrichJob.tsx) rather than here: this component is rendered in two
 * places at once (the library toolbar and Settings, which stays mounted while
 * hidden), and the daemon only ever runs one sweep regardless of which button
 * started it. This component is just a view onto that shared state, plus the
 * one thing that IS genuinely per-instance -- which filters *this* button's
 * sweep should cover, and what to do when it finishes.
 */
export function EnrichRunner({ filters, expected, label, className, onFinished }: Props) {
  const { job, available, error, starting, start, cancel, clearError } = useEnrichJob()
  useEnrichFinished(() => onFinished?.())

  if (config.readOnly || !available) return null

  const running = job?.state === 'running'

  const handleStart = async () => {
    // A library-wide sweep is one throttled request per model against a public
    // API -- minutes to hours. Worth confirming rather than starting on a click.
    if (expected === undefined || expected > 50) {
      const scope = expected === undefined ? 'every model in the library' : `${expected} models`
      const rough = expected === undefined ? '' : `\n\nRoughly ${estimate(expected)} at the polite request rate.`
      if (
        !window.confirm(
          `Look up ${scope} at the origin and merge what comes back?` +
            rough +
            '\n\nValues you have edited are never overwritten, and thumbnails you chose are kept. ' +
            'You can stop it at any time; everything fetched so far is kept.',
        )
      ) {
        return
      }
    }

    clearError()
    try {
      await start(filters)
    } catch {
      // Recorded in the shared error state already; nothing more to do here.
    }
  }

  const pct =
    job && job.models_total > 0 ? Math.round((job.models_done / job.models_total) * 100) : 0

  return (
    <div className={`enrich-runner${className ? ` ${className}` : ''}`}>
      {!running && (
        <button disabled={starting} onClick={() => void handleStart()}>
          {starting ? 'Starting…' : label}
        </button>
      )}

      {running && job && (
        <div className="enrich-progress">
          <progress value={job.models_done} max={job.models_total || 1} />
          <span className="source-note">
            {job.models_done.toLocaleString()} / {job.models_total.toLocaleString()} ({pct}%)
            {job.found > 0 && ` — ${job.found.toLocaleString()} matched`}
            {job.images > 0 && `, ${job.images.toLocaleString()} images`}
          </span>
          <button onClick={cancel}>Stop</button>
        </div>
      )}

      {!running && job?.state === 'complete' && (
        <span className="source-note">
          Swept {job.models_done.toLocaleString()}: {job.found.toLocaleString()} matched,{' '}
          {job.missing.toLocaleString()} not listed
          {job.images > 0 && `, ${job.images.toLocaleString()} images`}
          {job.errors > 0 && `, ${job.errors.toLocaleString()} errors`}.
          {job.rate_limited &&
            ' The origin rejected a request during this run — run it again to make sure everything was covered.'}
          {job.finished_at && ` (${relativeTime(job.finished_at)})`}
        </span>
      )}

      {!running && job?.state === 'cancelled' && (
        <span className="source-note">
          Stopped after {job.models_done.toLocaleString()}. Everything fetched was kept — running
          again continues from here.
          {job.finished_at && ` (${relativeTime(job.finished_at)})`}
        </span>
      )}

      {!running && job?.state === 'failed' && (
        <span className="error inline">
          {job.error || 'The run failed.'}
          {job.finished_at && ` (${relativeTime(job.finished_at)})`}
        </span>
      )}

      {/* The per-model failure the run last saw. The counter alone says
          something went wrong without saying what, and there is no console
          behind this UI to go and look in. */}
      {job?.last_error && job.errors > 0 && (
        <span className="source-note">Last error: {job.last_error}</span>
      )}

      {error && <span className="error inline">{error}</span>}
    </div>
  )
}

// estimate turns a model count into rough wall-clock at the client's default
// 350ms between requests. Deliberately coarse: it is there to set expectations
// before a long run, not to be accurate.
function estimate(models: number): string {
  const seconds = models * 0.4
  if (seconds < 90) return 'under two minutes'
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `about ${minutes} minutes`
  const hours = (minutes / 60).toFixed(1).replace(/\.0$/, '')
  return `about ${hours} hours`
}
