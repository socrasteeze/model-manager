import { useCallback, useEffect, useRef, useState } from 'react'
import {
  MODEL_TYPES,
  activeScan,
  addRoot,
  cancelScan,
  config,
  deleteSetting,
  detectInstalls,
  folderDefaults,
  formatBytes,
  getSettings,
  listRoots,
  patchRoot,
  putSetting,
  removeRoot,
  startScan,
  SETTING_COMFY_CHECKPOINT,
  SETTING_COMFY_OUTPUT,
  SETTING_COMFY_URL,
  SETTING_COMFY_WORKFLOW,
  SETTING_DEFAULT_ROOT,
  SETTING_FOLDER_MAP,
  comfyStatus,
  type ComfyStatus,
  type FolderDefaults,
  type FolderMap,
  type Root,
  type ScanJob,
} from '../api'

const TOOL_LABELS: Record<string, string> = {
  'stability-matrix': 'Stability Matrix',
  swarmui: 'SwarmUI',
  comfyui: 'ComfyUI',
}

/**
 * Settings: managed directories, per-type download folders, and the ComfyUI
 * output folder used when picking a thumbnail from a generated image.
 *
 * Directories are the load-bearing part. A root here is both what gets scanned
 * and where a download is allowed to land, so the server canonicalizes the path
 * and refuses one that overlaps an existing root — an overlap makes the
 * per-root present-sweep ambiguous, and files under it would flap between
 * present and absent on every scan.
 */
export function SettingsPanel({ hidden, onLibraryChanged }: {
  hidden: boolean
  onLibraryChanged: () => void
}) {
  const [roots, setRoots] = useState<Root[]>([])
  const [scan, setScan] = useState<ScanJob | null>(null)
  const [defaults, setDefaults] = useState<FolderDefaults | null>(null)
  const [folderMap, setFolderMap] = useState<FolderMap>({})
  const [defaultRoot, setDefaultRoot] = useState('')
  const [comfyOut, setComfyOut] = useState('')
  const [comfyUrl, setComfyUrl] = useState('')
  const [comfyCkpt, setComfyCkpt] = useState('')
  const [workflow, setWorkflow] = useState('')
  const [comfy, setComfy] = useState<ComfyStatus | null>(null)
  const [newPath, setNewPath] = useState('')
  const [suggestions, setSuggestions] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [editingFolders, setEditingFolders] = useState<number | null>(null)

  const refresh = useCallback(() => {
    listRoots().then(setRoots).catch((e: Error) => setError(e.message))
  }, [])

  useEffect(() => {
    if (hidden) return
    refresh()
    folderDefaults().then(setDefaults).catch(() => {})
    getSettings()
      .then((s) => {
        setFolderMap((s[SETTING_FOLDER_MAP] as FolderMap) ?? {})
        setDefaultRoot((s[SETTING_DEFAULT_ROOT] as string) ?? '')
        setComfyOut((s[SETTING_COMFY_OUTPUT] as string) ?? '')
        setComfyUrl((s[SETTING_COMFY_URL] as string) ?? '')
        setComfyCkpt((s[SETTING_COMFY_CHECKPOINT] as string) ?? '')
        const wf = s[SETTING_COMFY_WORKFLOW]
        setWorkflow(typeof wf === 'string' ? wf : wf ? JSON.stringify(wf, null, 2) : '')
      })
      .catch(() => {})
    comfyStatus().then(setComfy).catch(() => setComfy(null))
    detectInstalls()
      .then((d) => setSuggestions(d.model_roots ?? []))
      .catch(() => {
        /* detection is a convenience; typing a path still works */
      })
  }, [hidden, refresh])

  // Poll only while a scan is actually running. A completed scan is a static
  // record, and polling one forever is a request per second for nothing.
  const scanRef = useRef(scan)
  scanRef.current = scan
  useEffect(() => {
    if (hidden) return
    let stop = false
    const tick = () => {
      activeScan()
        .then((s) => {
          if (stop) return
          const finished = scanRef.current?.state === 'running' && s?.state !== 'running'
          setScan(s)
          if (finished) {
            refresh()
            onLibraryChanged()
          }
        })
        .catch(() => {})
    }
    tick()
    const timer = setInterval(tick, 1000)
    return () => {
      stop = true
      clearInterval(timer)
    }
  }, [hidden, refresh, onLibraryChanged])

  const run = async (fn: () => Promise<unknown>, ok?: string) => {
    setBusy(true)
    setError(null)
    setNotice(null)
    try {
      await fn()
      if (ok) setNotice(ok)
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  const onAdd = (path: string) =>
    run(async () => {
      const res = await addRoot(path.trim())
      setNewPath('')
      refresh()
      if (res.scan_deferred) setNotice(`Added. ${res.scan_deferred}.`)
    }, 'Directory added — scanning it now.')

  const saveFolderMap = (next: FolderMap) => {
    setFolderMap(next)
    void run(() => putSetting(SETTING_FOLDER_MAP, next))
  }

  const readOnly = config.readOnly

  return (
    <div className="settings" hidden={hidden}>
      {readOnly && (
        <div className="banner">
          Read-only. Start the daemon with <code>--writable</code> to add directories or
          change settings.
        </div>
      )}
      {error && <div className="error">{error}</div>}
      {notice && <div className="notice">{notice}</div>}

      <section className="settings-block">
        <h2>Model directories</h2>
        <p className="hint">
          Every directory here is scanned into the library and is a legal download
          destination. Removing one never touches the disk — it forgets the folder and
          keeps the metadata, so re-adding it later restores what was known.
        </p>

        <div className="root-add">
          <input
            type="text"
            placeholder="E:\\StabilityMatrix\\Data\\Models"
            value={newPath}
            onChange={(e) => setNewPath(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && newPath.trim()) onAdd(newPath)
            }}
            spellCheck={false}
            disabled={readOnly || busy}
          />
          <button disabled={readOnly || busy || !newPath.trim()} onClick={() => onAdd(newPath)}>
            Add directory
          </button>
        </div>

        {suggestions.filter((p) => !roots.some((r) => r.path === p)).length > 0 && (
          <div className="root-suggestions">
            <span className="hint">Detected on this machine:</span>
            {suggestions
              .filter((p) => !roots.some((r) => r.path === p))
              .map((p) => (
                <button key={p} className="chip" disabled={readOnly || busy} onClick={() => onAdd(p)}>
                  + {p}
                </button>
              ))}
          </div>
        )}

        {scan?.state === 'running' && (
          <div className="scan-progress">
            <div className="scan-bar">
              <div
                className="scan-fill"
                style={{
                  width: `${scan.files_total ? (scan.files_done / scan.files_total) * 100 : 0}%`,
                }}
              />
            </div>
            <span>
              Scanning {scan.files_done.toLocaleString()} / {scan.files_total.toLocaleString()}{' '}
              files
              {scan.errors > 0 && ` · ${scan.errors} error${scan.errors === 1 ? '' : 's'}`}
            </span>
            <button onClick={() => run(() => cancelScan(scan.id))}>Cancel</button>
          </div>
        )}

        <ul className="root-list">
          {roots.map((r) => (
            <li key={r.id} className={r.enabled ? '' : 'off'}>
              <div className="root-main">
                <code className="root-path">{r.path}</code>
                <span className="root-meta">
                  {r.files.toLocaleString()} file{r.files === 1 ? '' : 's'} ·{' '}
                  {formatBytes(r.bytes)}
                  {r.tool && ` · ${TOOL_LABELS[r.tool] ?? r.tool} layout`}
                  {r.last_scanned_at
                    ? ` · scanned ${new Date(r.last_scanned_at).toLocaleDateString()}`
                    : ' · never scanned'}
                </span>
              </div>
              <div className="root-actions">
                <select
                  value={r.tool ?? ''}
                  disabled={readOnly || busy}
                  title="Which tool's folder names this directory uses"
                  onChange={(e) =>
                    run(async () => {
                      await patchRoot(r.id, { tool: e.target.value })
                      refresh()
                    })
                  }
                >
                  <option value="">No layout</option>
                  {(defaults?.tools ?? []).map((t) => (
                    <option key={t} value={t}>
                      {TOOL_LABELS[t] ?? t}
                    </option>
                  ))}
                </select>
                <button
                  disabled={readOnly || busy}
                  onClick={() => setEditingFolders(editingFolders === r.id ? null : r.id)}
                >
                  Folders
                </button>
                <button
                  disabled={readOnly || busy || scan?.state === 'running'}
                  onClick={() => run(() => startScan([r.path]), 'Scan started.')}
                >
                  Rescan
                </button>
                <button
                  disabled={readOnly || busy}
                  onClick={() =>
                    run(async () => {
                      await patchRoot(r.id, { enabled: !r.enabled })
                      refresh()
                    })
                  }
                >
                  {r.enabled ? 'Disable' : 'Enable'}
                </button>
                <button
                  className="danger"
                  disabled={readOnly || busy}
                  onClick={() => {
                    if (
                      !window.confirm(
                        `Forget ${r.path}?\n\nNothing on disk is touched. The models stay in the ` +
                          `library with their metadata; they are just marked as no longer present.`,
                      )
                    )
                      return
                    void run(async () => {
                      await removeRoot(r.id)
                      refresh()
                      onLibraryChanged()
                    }, 'Directory forgotten. No files were changed.')
                  }}
                >
                  Forget
                </button>
              </div>

              {editingFolders === r.id && defaults && (
                <FolderEditor
                  root={r}
                  defaults={defaults}
                  map={folderMap[r.path] ?? {}}
                  disabled={readOnly || busy}
                  onChange={(byType) => saveFolderMap({ ...folderMap, [r.path]: byType })}
                  onReset={() => {
                    const next = { ...folderMap }
                    delete next[r.path]
                    saveFolderMap(next)
                  }}
                />
              )}
            </li>
          ))}
          {roots.length === 0 && (
            <li className="empty-row">
              No directories yet. Add one above, or run <code>mm scan --root DIR</code>.
            </li>
          )}
        </ul>

        <div className="root-footer">
          <button
            disabled={readOnly || busy || scan?.state === 'running' || roots.length === 0}
            onClick={() => run(() => startScan(), 'Scanning every directory.')}
          >
            Rescan everything
          </button>
        </div>
      </section>

      <section className="settings-block">
        <h2>Downloads</h2>
        <label className="setting-row">
          <span>Default destination</span>
          <select
            value={defaultRoot}
            disabled={readOnly}
            onChange={(e) => {
              setDefaultRoot(e.target.value)
              void run(() =>
                e.target.value
                  ? putSetting(SETTING_DEFAULT_ROOT, e.target.value)
                  : deleteSetting(SETTING_DEFAULT_ROOT),
              )
            }}
          >
            <option value="">First directory</option>
            {roots
              .filter((r) => r.enabled)
              .map((r) => (
                <option key={r.id} value={r.path}>
                  {r.path}
                </option>
              ))}
          </select>
        </label>
        <p className="hint">
          A download always lands inside a directory from the list above — never a path
          typed into the browser. The per-type subfolder comes from that directory's
          layout, which is why the same lora goes to <code>Lora</code> under Stability
          Matrix and <code>loras</code> under ComfyUI.
        </p>
      </section>

      <section className="settings-block">
        <h2>Thumbnails</h2>
        <label className="setting-row">
          <span>ComfyUI output folder</span>
          <input
            type="text"
            placeholder="E:\\ComfyUI\\output"
            value={comfyOut}
            disabled={readOnly}
            spellCheck={false}
            onChange={(e) => setComfyOut(e.target.value)}
            onBlur={() =>
              run(() =>
                comfyOut.trim()
                  ? putSetting(SETTING_COMFY_OUTPUT, comfyOut.trim())
                  : deleteSetting(SETTING_COMFY_OUTPUT),
              )
            }
          />
        </label>
        <p className="hint">
          Set this to pick a thumbnail from an image you just rendered. Only this one
          folder is readable, and a ComfyUI PNG's workflow is kept alongside the picture
          so it can be dragged back into ComfyUI.
        </p>
      </section>

      <section className="settings-block">
        <h2>Render thumbnails with ComfyUI</h2>
        <p className="hint">
          The one feature that needs ComfyUI actually running. Everything else here —
          the folder names, the workflow inside a PNG, the output folder above — works
          whether it is up or not. Leave the address blank and nothing is ever
          contacted.
        </p>

        <label className="setting-row">
          <span>ComfyUI address</span>
          <input
            type="text"
            placeholder="http://127.0.0.1:8188"
            value={comfyUrl}
            disabled={readOnly}
            spellCheck={false}
            onChange={(e) => setComfyUrl(e.target.value)}
            onBlur={() =>
              run(async () => {
                await (comfyUrl.trim()
                  ? putSetting(SETTING_COMFY_URL, comfyUrl.trim())
                  : deleteSetting(SETTING_COMFY_URL))
                setComfy(await comfyStatus())
              })
            }
          />
        </label>

        <p className={`hint${comfy?.configured && !comfy.reachable ? ' error-text' : ''}`}>
          {!comfy?.configured
            ? 'Not configured.'
            : comfy.reachable
              ? `Connected — ComfyUI ${comfy.version || '(version unreported)'}.`
              : comfy.error || 'Configured, but nothing is answering.'}
        </p>

        <label className="setting-row">
          <span>Base checkpoint</span>
          <input
            type="text"
            placeholder="sd_xl_base_1.0.safetensors"
            value={comfyCkpt}
            disabled={readOnly}
            spellCheck={false}
            onChange={(e) => setComfyCkpt(e.target.value)}
            onBlur={() =>
              run(() =>
                comfyCkpt.trim()
                  ? putSetting(SETTING_COMFY_CHECKPOINT, comfyCkpt.trim())
                  : deleteSetting(SETTING_COMFY_CHECKPOINT),
              )
            }
          />
        </label>
        <p className="hint">
          A lora cannot render anything by itself, so a preview has to be generated on
          top of a checkpoint. Name it exactly as ComfyUI lists it.
        </p>

        <details className="workflow-editor">
          <summary>Workflow</summary>
          <p className="hint">
            ComfyUI&rsquo;s <strong>API format</strong>, not the editor format — in
            ComfyUI, enable <em>Settings &rsaquo; Dev mode</em> and use{' '}
            <em>Save (API Format)</em>. Placeholders:{' '}
            {(comfy?.placeholders ?? []).map((p) => (
              <code key={p}>{`{{${p}}}`}</code>
            ))}
            . Leave blank for the built-in default.
          </p>
          <textarea
            rows={14}
            spellCheck={false}
            value={workflow}
            disabled={readOnly}
            placeholder="(built-in default)"
            onChange={(e) => setWorkflow(e.target.value)}
            onBlur={() =>
              run(() =>
                workflow.trim()
                  ? putSetting(SETTING_COMFY_WORKFLOW, workflow)
                  : deleteSetting(SETTING_COMFY_WORKFLOW),
              )
            }
          />
        </details>
      </section>
    </div>
  )
}

function FolderEditor({ root, defaults, map, disabled, onChange, onReset }: {
  root: Root
  defaults: FolderDefaults
  map: Record<string, string>
  disabled: boolean
  onChange: (byType: Record<string, string>) => void
  onReset: () => void
}) {
  const builtIn = defaults.defaults[root.tool ?? ''] ?? {}
  return (
    <div className="folder-editor">
      <p className="hint">
        Where each type lands under <code>{root.path}</code>. Blank means the directory
        itself.
      </p>
      <div className="folder-grid">
        {MODEL_TYPES.map((t) => (
          <label key={t}>
            <span>{t}</span>
            <input
              type="text"
              value={map[t] ?? ''}
              placeholder={builtIn[t] || '(directory itself)'}
              disabled={disabled}
              spellCheck={false}
              onChange={(e) => onChange({ ...map, [t]: e.target.value })}
            />
          </label>
        ))}
      </div>
      <button disabled={disabled} onClick={onReset}>
        Reset to the {TOOL_LABELS[root.tool ?? ''] ?? 'built-in'} defaults
      </button>
    </div>
  )
}
