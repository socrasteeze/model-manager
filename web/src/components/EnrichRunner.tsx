import { useCallback, useEffect, useRef, useState } from 'react'
import {
  activeEnrich,
  cancelEnrich,
  config,
  startEnrich,
  type EnrichJob,
  type Filters,
} from '../api'

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
 * Starts a background enrichment sweep and follows it.
 *
 * The daemon runs at most one sweep at a time, so this mounts in two places (the
 * library toolbar and Settings) and both show the same run. On mount it adopts
 * whatever is already in flight rather than assuming there is nothing — a phone
 * opening the UI mid-sweep should see the progress, not an idle button.
 */
export function EnrichRunner({ filters, expected, label, className, onFinished }: Props) {
  const [job, setJob] = useState<EnrichJob | null>(null)
  const [available, setAvailable] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [starting, setStarting] = useState(false)

  // Held in a ref so the polling effect does not re-subscribe whenever the
  // caller re-renders with a new closure.
  const finished = useRef(onFinished)
  finished.current = onFinished

  const poll = useCallback(async () => {
    try {
      const { job: current, available: ok } = await activeEnrich()
      setAvailable(ok)
      setJob(current)
      return current
    } catch {
      // A poll that fails is not a run that failed; the next tick may well
      // succeed, and blanking the progress would be a lie about the sweep.
      return null
    }
  }, [])

  useEffect(() => {
    void poll()
  }, [poll])

  // Poll only while something is running. A finished run is terminal, so there
  // is nothing to learn by asking again every second forever.
  const running = job?.state === 'running'
  const wasRunning = useRef(false)
  useEffect(() => {
    if (!running) {
      if (wasRunning.current) {
        wasRunning.current = false
        finished.current?.()
      }
      return
    }
    wasRunning.current = true
    const timer = setInterval(() => void poll(), 1000)
    return () => clearInterval(timer)
  }, [running, poll])

  if (config.readOnly || !available) return null

  const start = async () => {
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

    setStarting(true)
    setError(null)
    try {
      setJob(await startEnrich(filters))
    } catch (e) {
      setError((e as Error).message)
      // A 409 means somebody else's run is already going; adopt it rather than
      // leaving the user with an error and no progress to watch.
      void poll()
    } finally {
      setStarting(false)
    }
  }

  const pct =
    job && job.models_total > 0 ? Math.round((job.models_done / job.models_total) * 100) : 0

  return (
    <div className={`enrich-runner${className ? ` ${className}` : ''}`}>
      {!running && (
        <button disabled={starting} onClick={() => void start()}>
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
          <button onClick={() => void cancelEnrich(job.id).then(poll).catch(() => {})}>Stop</button>
        </div>
      )}

      {!running && job?.state === 'complete' && (
        <span className="source-note">
          Swept {job.models_done.toLocaleString()}: {job.found.toLocaleString()} matched,{' '}
          {job.missing.toLocaleString()} not listed
          {job.images > 0 && `, ${job.images.toLocaleString()} images`}
          {job.errors > 0 && `, ${job.errors.toLocaleString()} errors`}.
        </span>
      )}

      {!running && job?.state === 'cancelled' && (
        <span className="source-note">
          Stopped after {job.models_done.toLocaleString()}. Everything fetched was kept — running
          again continues from here.
        </span>
      )}

      {!running && job?.state === 'failed' && (
        <span className="error inline">{job.error || 'The run failed.'}</span>
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
