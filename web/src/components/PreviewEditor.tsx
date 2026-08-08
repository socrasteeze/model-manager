import { useEffect, useRef, useState } from 'react'
import {
  attachGeneratedPreview,
  cancelRender,
  comfyStatus,
  config,
  deletePreview,
  isRenderActive,
  listGenerated,
  listRenders,
  previewURL,
  renderPreview,
  reorderPreviews,
  uploadPreview,
  workflowURL,
  type ComfyStatus,
  type GeneratedImage,
  type PreviewImage,
  type RenderJob,
} from '../api'

/**
 * Thumbnails for one model: what is attached, and the three ways to change it.
 *
 * Previews are already sticky — the bytes were copied into the content-addressed
 * blob store when they were fetched, so a Civitai takedown cannot blank a local
 * thumbnail. What this adds is the user's own choice, and the guarantee that
 * enrichment cannot displace it: a `manual` preview outranks every fetched one,
 * which is the same tiering the field provenance uses.
 */
export function PreviewEditor({ sha, previews, onChanged }: {
  sha: string
  previews: PreviewImage[]
  onChanged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [picking, setPicking] = useState(false)
  const [generated, setGenerated] = useState<GeneratedImage[] | null>(null)
  const [genError, setGenError] = useState<string | null>(null)
  const [dragOver, setDragOver] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)

  const [comfy, setComfy] = useState<ComfyStatus | null>(null)
  const [render, setRender] = useState<RenderJob | null>(null)
  const [prompt, setPrompt] = useState('')
  const [showRender, setShowRender] = useState(false)

  const editable = !config.readOnly

  // Asked once per model, so the Render button is only offered when there is
  // something listening. A button that fails after thirty seconds of waiting is
  // worse than one that is not there.
  useEffect(() => {
    if (config.readOnly) return
    comfyStatus().then(setComfy).catch(() => setComfy(null))
  }, [sha])

  // Poll only while this model has a render in flight. A finished render is a
  // static record; polling one forever is a request per second for nothing.
  useEffect(() => {
    if (!render || !isRenderActive(render)) return
    let stop = false
    const timer = setInterval(() => {
      listRenders()
        .then((jobs) => {
          if (stop) return
          const mine = jobs.find((j) => j.id === render.id)
          // The job is gone -- most likely the daemon restarted and its
          // in-memory job table emptied. Clearing it here is what stops the
          // poll and re-enables the button; leaving the stale "running" state
          // in place would poll forever with no way to ever complete.
          setRender(mine ?? null)
          if (mine?.state === 'complete') onChanged()
        })
        .catch(() => {})
    }, 1500)
    return () => {
      stop = true
      clearInterval(timer)
    }
  }, [render, onChanged])

  useEffect(() => {
    if (!picking) return
    listGenerated()
      .then((r) => {
        setGenerated(r.images)
        setGenError(null)
      })
      .catch((e: Error) => setGenError(e.message))
  }, [picking])

  const run = async (fn: () => Promise<unknown>) => {
    setBusy(true)
    setError(null)
    try {
      await fn()
      onChanged()
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const upload = (files: FileList | null) => {
    const file = files?.[0]
    if (!file) return
    void run(() => uploadPreview(sha, file))
  }

  return (
    <div className="preview-editor">
      <div
        className={`preview-strip${dragOver ? ' drag-over' : ''}`}
        onDragOver={(e) => {
          if (!editable) return
          e.preventDefault()
          setDragOver(true)
        }}
        onDragLeave={() => setDragOver(false)}
        onDrop={(e) => {
          if (!editable) return
          e.preventDefault()
          setDragOver(false)
          upload(e.dataTransfer.files)
        }}
      >
        {previews.map((p, i) => (
          <figure key={p.id} className={p.source === 'manual' ? 'chosen' : ''}>
            {/* The grid copy when there is one; the full image is a click away
                and is what the workflow link hands back. */}
            <img
              src={previewURL(p.thumb_sha256 || p.image_sha256)}
              alt=""
              loading="lazy"
              decoding="async"
              width={p.width || undefined}
              height={p.height || undefined}
            />
            <figcaption>
              {p.source === 'manual' && <span className="badge">yours</span>}
              {p.workflow_sha256 && (
                <a
                  href={workflowURL(sha, p.image_sha256)}
                  download
                  title="Download the ComfyUI workflow this image carries"
                >
                  workflow
                </a>
              )}
              {/* Worded, not glyphs. iOS never shows a title tooltip, so on a
                  phone these were an unlabelled star and cross where one of
                  them detaches an image -- the meaning was reachable only with
                  a mouse. */}
              {editable && i > 0 && (
                <button
                  disabled={busy}
                  aria-label="Make this the thumbnail"
                  title="Make this the thumbnail"
                  onClick={() =>
                    run(() =>
                      reorderPreviews(sha, [
                        p.image_sha256,
                        ...previews.filter((x) => x.id !== p.id).map((x) => x.image_sha256),
                      ]),
                    )
                  }
                >
                  cover
                </button>
              )}
              {editable && (
                <button
                  className="danger"
                  disabled={busy}
                  aria-label="Detach this image (the file on disk is untouched)"
                  title="Detach this image (the file on disk is untouched)"
                  onClick={() => run(() => deletePreview(sha, p.image_sha256))}
                >
                  detach
                </button>
              )}
            </figcaption>
          </figure>
        ))}
        {previews.length === 0 && (
          <p className="hint">
            No thumbnail yet. {editable ? 'Drop an image here, or use the buttons below.' : ''}
          </p>
        )}
      </div>

      {error && <div className="error inline">{error}</div>}

      {editable && (
        <div className="preview-actions">
          <input
            ref={fileInput}
            type="file"
            accept="image/*"
            hidden
            onChange={(e) => {
              upload(e.target.files)
              e.target.value = ''
            }}
          />
          <button disabled={busy} onClick={() => fileInput.current?.click()}>
            Upload an image
          </button>
          <button disabled={busy} onClick={() => setPicking((v) => !v)}>
            {picking ? 'Hide picker' : 'Pick a generated image'}
          </button>
          {comfy?.configured && (
            <button
              disabled={busy || !comfy.reachable || (render ? isRenderActive(render) : false)}
              title={
                comfy.reachable
                  ? 'Render a thumbnail with ComfyUI'
                  : comfy.error || 'ComfyUI is not answering'
              }
              onClick={() => setShowRender((v) => !v)}
            >
              {showRender ? 'Hide render' : 'Render one'}
            </button>
          )}
        </div>
      )}

      {showRender && comfy?.configured && (
        <div className="render-panel">
          <input
            type="text"
            placeholder="Prompt (defaults to this model's trigger words)"
            value={prompt}
            onChange={(e) => setPrompt(e.target.value)}
            spellCheck={false}
          />
          <button
            disabled={busy || (render ? isRenderActive(render) : false)}
            onClick={() =>
              run(async () => {
                const job = await renderPreview(sha, prompt.trim() ? { prompt: prompt.trim() } : {})
                setRender(job)
              })
            }
          >
            Render
          </button>
          {render && (
            <span className={`render-state ${render.state}`}>
              {render.state}
              {isRenderActive(render) && (
                <button
                  onClick={() =>
                    run(async () => {
                      await cancelRender(render.id)
                    })
                  }
                >
                  cancel
                </button>
              )}
            </span>
          )}
          {render?.error && <div className="error inline">{render.error}</div>}
          {!comfy.reachable && (
            <div className="hint">
              {comfy.error ?? 'ComfyUI is not answering on ' + (comfy.url ?? 'the configured address')}
            </div>
          )}
        </div>
      )}

      {picking && (
        <div className="generated-picker">
          {genError && (
            <div className="hint">
              {genError} — set the ComfyUI output folder in Settings to pick from renders.
            </div>
          )}
          {generated && generated.length === 0 && (
            <div className="hint">No images in the configured output folder.</div>
          )}
          <div className="generated-grid">
            {(generated ?? []).map((g) => (
              <button
                key={g.rel}
                className="generated-item"
                disabled={busy}
                title={g.rel}
                onClick={() =>
                  run(async () => {
                    await attachGeneratedPreview(sha, g.rel)
                    setPicking(false)
                  })
                }
              >
                {g.name}
              </button>
            ))}
          </div>
        </div>
      )}
    </div>
  )
}
