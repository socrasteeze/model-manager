import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from 'react'
import { activeEnrich, cancelEnrich, startEnrich, type EnrichJob, type Filters } from '../api'

interface EnrichJobState {
  job: EnrichJob | null
  available: boolean
  error: string | null
  starting: boolean
  start: (filters?: Filters) => Promise<void>
  cancel: () => void
  clearError: () => void
  /** Registers a callback for "the job just left the running state". Returns an unsubscribe function. */
  subscribeFinished: (cb: () => void) => () => void
}

const Ctx = createContext<EnrichJobState | null>(null)

/**
 * One poller and one piece of job/error state for the whole app, not one per
 * mounted button.
 *
 * EnrichRunner is rendered in two places at once: the library toolbar (only
 * while the Library tab is showing) and Settings, which -- like Browse --
 * stays mounted-but-hidden across tab switches rather than unmounting. So
 * both are simultaneously alive on the Library tab. The daemon runs at most
 * one sweep regardless of which button started it; before this Provider each
 * instance ran its own 1-second poll loop and held its own `error` state, so
 * there were two independent polls for the same server-side truth, and a 409
 * shown on one instance had nothing to clear it once the other instance's
 * retry succeeded.
 *
 * Each EnrichRunner still calls its own onFinished when the shared job stops
 * running -- that part is intentionally per-instance (the toolbar refreshes
 * search results, Settings refreshes its own view) and is not something this
 * Provider tries to deduplicate. What it shares is the poll and the job/error
 * state those callbacks read.
 *
 * Mounted once at the app root; every EnrichRunner reads the one shared
 * answer instead of asking the server itself.
 */
export function EnrichJobProvider({ children }: { children: ReactNode }) {
  const [job, setJob] = useState<EnrichJob | null>(null)
  const [available, setAvailable] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [starting, setStarting] = useState(false)

  const poll = useCallback(async () => {
    try {
      const { job: current, available: ok } = await activeEnrich()
      setAvailable(ok)
      setJob(current)
      // A successful poll is current truth, so any error from a previous
      // start() attempt -- most commonly "a run is already in progress",
      // which this same poll adopts -- stops being relevant. Without this,
      // that message would sit under the adopted run's progress bar and
      // then survive the run's completion, since nothing else ever clears it
      // outside of pressing Start again.
      setError(null)
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

  const running = job?.state === 'running'
  const listeners = useRef(new Set<() => void>())
  const wasRunning = useRef(false)
  useEffect(() => {
    if (running) {
      wasRunning.current = true
      return
    }
    if (wasRunning.current) {
      wasRunning.current = false
      listeners.current.forEach((cb) => cb())
    }
  }, [running])

  useEffect(() => {
    if (!running) return
    const timer = setInterval(() => void poll(), 1000)
    return () => clearInterval(timer)
  }, [running, poll])

  const start = useCallback(
    async (filters?: Filters) => {
      setStarting(true)
      setError(null)
      try {
        setJob(await startEnrich(filters))
      } catch (e) {
        setError((e as Error).message)
        // A 409 means somebody else's run is already going; adopt it rather
        // than leaving every instance with an error and no progress to watch.
        void poll()
        throw e
      } finally {
        setStarting(false)
      }
    },
    [poll],
  )

  const cancel = useCallback(() => {
    if (!job) return
    void cancelEnrich(job.id).then(poll).catch(() => {})
  }, [job, poll])

  const clearError = useCallback(() => setError(null), [])

  const subscribeFinished = useCallback((cb: () => void) => {
    listeners.current.add(cb)
    return () => listeners.current.delete(cb)
  }, [])

  return (
    <Ctx.Provider value={{ job, available, error, starting, start, cancel, clearError, subscribeFinished }}>
      {children}
    </Ctx.Provider>
  )
}

export function useEnrichJob(): EnrichJobState {
  const ctx = useContext(Ctx)
  if (!ctx) throw new Error('useEnrichJob must be used within an EnrichJobProvider')
  return ctx
}

/** Runs `cb` once each time the shared sweep leaves the running state. */
export function useEnrichFinished(cb: () => void): void {
  const { subscribeFinished } = useEnrichJob()
  const cbRef = useRef(cb)
  cbRef.current = cb
  useEffect(() => subscribeFinished(() => cbRef.current()), [subscribeFinished])
}

/**
 * Formats an ISO timestamp as a short relative time, so a finished sweep's
 * summary says how long ago it ran instead of implying it just happened.
 * Falls back to a locale date once "N days ago" stops being useful at a
 * glance.
 */
export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ''
  const seconds = Math.round((Date.now() - then) / 1000)
  if (seconds < 5) return 'just now'
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.round(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.round(hours / 24)
  if (days < 7) return `${days}d ago`
  return new Date(iso).toLocaleDateString()
}
