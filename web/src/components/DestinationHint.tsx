import { useEffect, useState } from 'react'
import { resolveDestination } from '../api'

/**
 * Shows where a download will actually land, before it is pressed.
 *
 * The subfolder is the server's decision, not the browser's — it depends on
 * which tool's vocabulary the destination root uses, and the same lora goes to
 * `Lora` under Stability Matrix and `loras` under ComfyUI. So the only honest
 * way to display it is to ask.
 *
 * A destination you cannot see is a destination you cannot object to, which is
 * the whole reason this is on screen rather than implicit.
 */
export function DestinationHint({ root, type }: { root: string; type?: string }) {
  const [dest, setDest] = useState<string | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!root) {
      setDest(null)
      return
    }
    let stale = false
    resolveDestination(root, type)
      .then((r) => {
        if (stale) return
        setDest(r.dest_dir)
        setError(null)
      })
      .catch((e: Error) => {
        if (!stale) setError(e.message)
      })
    return () => {
      stale = true
    }
  }, [root, type])

  if (error) return <span className="dest-hint error-text">{error}</span>
  if (!dest) return null
  return (
    <span className="dest-hint">
      lands in <code>{dest}</code>
    </span>
  )
}
