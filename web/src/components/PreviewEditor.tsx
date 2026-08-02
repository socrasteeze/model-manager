import { useEffect, useRef, useState } from 'react'
import {
  attachGeneratedPreview,
  config,
  deletePreview,
  listGenerated,
  previewURL,
  reorderPreviews,
  uploadPreview,
  workflowURL,
  type GeneratedImage,
  type PreviewImage,
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

  const editable = !config.readOnly

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
              {editable && i > 0 && (
                <button
                  disabled={busy}
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
                  ★
                </button>
              )}
              {editable && (
                <button
                  className="danger"
                  disabled={busy}
                  title="Detach this image (the file on disk is untouched)"
                  onClick={() => run(() => deletePreview(sha, p.image_sha256))}
                >
                  ×
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
            {picking ? 'Close' : 'Pick a generated image'}
          </button>
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
