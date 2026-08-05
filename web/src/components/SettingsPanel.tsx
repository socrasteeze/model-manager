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
  SETTING_COMFY_WORKFLOW_DIR,
  SETTING_BROWSE_NSFW,
  SETTING_DEFAULT_ROOT,
  SETTING_FOLDER_MAP,
  SETTING_GROUPING,
  SETTING_THUMB_ASPECT,
  THUMB_ASPECTS,
  GROUPING_MODES,
  DEFAULT_GROUPING,
  DEFAULT_INCLUDE_NSFW,
  DEFAULT_THUMB_ASPECT,
  asGroupingMode,
  asThumbAspect,
  type GroupingMode,
  type ThumbAspect,
  adoptWorkflow,
  comfyStatus,
  listWorkflows,
  workflowStatus,
  type ComfyStatus,
  type FamilyStatus,
  type WorkflowFile,
  type FolderDefaults,
  type FolderMap,
  type Root,
  type ScanJob,
} from '../api'
import { EnrichRunner } from './EnrichRunner'
import { ToggleRow } from './ToggleRow'

/**
 * Reads a setting that may be either a bare value ("use this for everything")
 * or a per-family map, and returns a map either way.
 *
 * Both shapes are accepted rather than migrated: a bare value is what earlier
 * versions stored and is still the right thing to write when there is only one
 * family to configure.
 */
function asFamilyMap(value: unknown): Record<string, string> {
  if (typeof value === 'string') return value.trim() ? { '': value } : {}
  if (value && typeof value === 'object') {
    const out: Record<string, string> = {}
    for (const [k, v] of Object.entries(value as Record<string, unknown>)) {
      if (typeof v === 'string') out[k] = v
      else if (v && typeof v === 'object') out[k] = JSON.stringify(v, null, 2)
    }
    return out
  }
  return {}
}

/** Writes a family map back, dropping blank slots and resetting when empty. */
function saveMap(
  key: string,
  map: Record<string, string>,
  run: (fn: () => Promise<unknown>) => Promise<void> | void,
) {
  const cleaned: Record<string, string> = {}
  for (const [k, v] of Object.entries(map)) {
    if (v.trim()) cleaned[k] = v
  }
  void run(() =>
    Object.keys(cleaned).length ? putSetting(key, cleaned) : deleteSetting(key),
  )
}

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
export function SettingsPanel({ hidden, onLibraryChanged, onPreferenceChanged }: {
  hidden: boolean
  onLibraryChanged: () => void

  /**
   * Announces a preference this panel just wrote, so the shell can apply it
   * without re-reading every setting. Re-reading would also return
   * library.filters, and re-applying that would clobber in-session filter
   * state against App's own debounced write.
   */
  onPreferenceChanged?: (key: string, value: unknown) => void
}) {
  const [roots, setRoots] = useState<Root[]>([])
  const [scan, setScan] = useState<ScanJob | null>(null)
  const [defaults, setDefaults] = useState<FolderDefaults | null>(null)
  const [folderMap, setFolderMap] = useState<FolderMap>({})
  const [defaultRoot, setDefaultRoot] = useState('')
  const [thumbAspect, setThumbAspect] = useState<ThumbAspect>(DEFAULT_THUMB_ASPECT)
  const [includeNSFW, setIncludeNSFW] = useState(DEFAULT_INCLUDE_NSFW)
  const [grouping, setGrouping] = useState<GroupingMode>(DEFAULT_GROUPING)
  const [comfyOut, setComfyOut] = useState('')
  const [comfyUrl, setComfyUrl] = useState('')
  const [checkpoints, setCheckpoints] = useState<Record<string, string>>({})
  const [workflows, setWorkflows] = useState<Record<string, string>>({})
  const [family, setFamily] = useState('')
  const [comfy, setComfy] = useState<ComfyStatus | null>(null)
  const [workflowDir, setWorkflowDir] = useState('')
  const [files, setFiles] = useState<WorkflowFile[]>([])
  const [filesError, setFilesError] = useState<string | null>(null)
  const [famStatus, setFamStatus] = useState<FamilyStatus[]>([])
  const adoptInput = useRef<HTMLInputElement>(null)
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
        setWorkflowDir((s[SETTING_COMFY_WORKFLOW_DIR] as string) ?? '')
        setComfyUrl((s[SETTING_COMFY_URL] as string) ?? '')
        // Both settings accept a bare value (meaning "for every family") or a
        // map. Normalized to a map here so the editor has one shape to deal
        // with, and written back the same way.
        setCheckpoints(asFamilyMap(s[SETTING_COMFY_CHECKPOINT]))
        setWorkflows(asFamilyMap(s[SETTING_COMFY_WORKFLOW]))
        setThumbAspect(asThumbAspect(s[SETTING_THUMB_ASPECT]))
        setGrouping(asGroupingMode(s[SETTING_GROUPING]))
        // Absent means the default, which is on. Only an explicit false is off.
        setIncludeNSFW(
          s[SETTING_BROWSE_NSFW] === undefined ? DEFAULT_INCLUDE_NSFW : !!s[SETTING_BROWSE_NSFW],
        )
      })
      .catch(() => {})
    comfyStatus().then(setComfy).catch(() => setComfy(null))
    void refreshWorkflows()
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

  const refreshWorkflows = useCallback(async () => {
    try {
      const res = await listWorkflows()
      setFiles(res.workflows)
      setFilesError(res.error ?? null)
    } catch (e) {
      setFilesError((e as Error).message)
    }
    workflowStatus().then(setFamStatus).catch(() => {})
  }, [])

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

  // Writes a preference and tells the shell, so a change here takes effect
  // immediately rather than on the next reload. The value is always written
  // explicitly, never deleted-to-restore-the-default the way the text settings
  // are: for a boolean defaulting to on, "never touched" and "deliberately
  // switched back on" would otherwise be the same stored state, and changing
  // the default later would silently reverse a real choice.
  const savePreference = (key: string, value: unknown) =>
    run(async () => {
      await putSetting(key, value)
      onPreferenceChanged?.(key, value)
    })

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
                  readOnly={readOnly}
                  busy={busy}
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
        <h2>Metadata and thumbnails from the origin</h2>
        <p className="hint">
          Looks every model up by content hash — an exact match, not a filename guess —
          and merges the published name, base model, trigger words, description and
          preview images.
        </p>
        <p className="hint">
          Nothing you have edited is overwritten: a manual value wins, and where the
          origin disagrees it is raised as a suggestion on that model instead. Blank
          fields take the best answer available, and a thumbnail you chose stays first.
          Responses are archived permanently, so a model later taken down keeps the
          metadata this fetched.
        </p>
        <EnrichRunner
          label="Refresh the whole library"
          onFinished={onLibraryChanged}
        />
        <p className="hint">
          Throttled to stay polite to the API, so a large library takes a while. Stopping
          is safe — everything fetched is kept, and running again continues from there
          rather than starting over. Models whose hash is still provisional are skipped;
          run <code>mm verify --provisional</code> to confirm them first.
        </p>
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
        <h2>Browsing</h2>
        <ToggleRow
          label="Include adult results"
          checked={includeNSFW}
          disabled={readOnly}
          onChange={(next) => {
            setIncludeNSFW(next)
            void savePreference(SETTING_BROWSE_NSFW, next)
          }}
          hint="On by default. The providers this searches are largely adult-adjacent, so turning it off hides most of what a search matched."
        />

        <label className="setting-row">
          <span>Group versions</span>
          <select
            value={grouping}
            disabled={readOnly}
            onChange={(e) => {
              const next = asGroupingMode(e.target.value)
              setGrouping(next)
              void savePreference(SETTING_GROUPING, next)
            }}
          >
            {GROUPING_MODES.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
        </label>
        <p className="hint">
          One Civitai model is published as many versions, and each one is its own
          search result — so a search can return eight cards with the same name.
          Grouping collapses them into one card with a version picker, in both Browse
          and the library. The default keeps different base models apart, since a LoRA
          rebuilt from SD 1.5 onto SDXL is not a drop-in replacement for it.
        </p>
      </section>

      <section className="settings-block">
        <h2>Thumbnails</h2>
        <label className="setting-row">
          <span>Shape</span>
          <select
            value={thumbAspect}
            disabled={readOnly}
            onChange={(e) => {
              const next = asThumbAspect(e.target.value)
              setThumbAspect(next)
              void savePreference(SETTING_THUMB_ASPECT, next)
            }}
          >
            {THUMB_ASPECTS.map((a) => (
              <option key={a.value} value={a.value}>
                {a.label}
              </option>
            ))}
          </select>
        </label>
        <p className="hint">
          Preview images are nearly always portrait — a Civitai preview is usually
          512×768 — so a square tile crops the top and bottom off most of them.
        </p>

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
          <span>Workflow folder</span>
          <input
            type="text"
            placeholder="E:\\ComfyUI\\user\\default\\workflows"
            value={workflowDir}
            disabled={readOnly}
            spellCheck={false}
            onChange={(e) => setWorkflowDir(e.target.value)}
            onBlur={() =>
              run(async () => {
                await (workflowDir.trim()
                  ? putSetting(SETTING_COMFY_WORKFLOW_DIR, workflowDir.trim())
                  : deleteSetting(SETTING_COMFY_WORKFLOW_DIR))
                await refreshWorkflows()
              })
            }
          />
        </label>
        <p className="hint">
          Where ComfyUI saves workflows. Pointing a family at a file here keeps the
          file yours: it is re-read on every render, so editing it in ComfyUI takes
          effect with no re-pasting. Save with <em>Save (API Format)</em>.
        </p>

        <p className="hint">
          A checkpoint and a workflow are kept <strong>per base model</strong>. An
          SDXL/Illustrious lora and a FLUX.2 one need different loaders, a different
          text encoder and a different VAE — a single graph would not render a worse
          picture, it would fail in ComfyUI. Pick a family below; the{' '}
          <em>Default</em> slot covers everything you have not set up.
        </p>

        <div className="family-tabs">
          {['', ...(comfy?.base_models ?? [])].map((f) => (
            <button
              key={f || 'default'}
              className={`chip${family === f ? ' on' : ''}${
                workflows[f] || checkpoints[f] ? ' configured' : ''
              }`}
              onClick={() => setFamily(f)}
            >
              {f || 'Default'}
            </button>
          ))}
        </div>

        <label className="setting-row">
          <span>Checkpoint for {family || 'everything else'}</span>
          <input
            type="text"
            placeholder={family === '' ? 'sd_xl_base_1.0.safetensors' : '(use the default)'}
            value={checkpoints[family] ?? ''}
            disabled={readOnly}
            spellCheck={false}
            onChange={(e) => setCheckpoints({ ...checkpoints, [family]: e.target.value })}
            onBlur={() => saveMap(SETTING_COMFY_CHECKPOINT, checkpoints, run)}
          />
        </label>
        <p className="hint">
          Leave this blank to keep whatever checkpoint the workflow already names —
          it evidently works, since the workflow does. Set one only to override it,
          named exactly as ComfyUI lists it.
        </p>

        {(() => {
          const status = famStatus.find((f) => f.family === family)
          const value = workflows[family] ?? ''
          const isFile = value.trim() !== '' && !value.trim().startsWith('{')
          const mode = value.trim() === '' ? 'inherit' : isFile ? 'file' : 'inline'

          return (
            <div className="workflow-source">
              <div className="chip-row">
                {(
                  [
                    ['inherit', family === '' ? 'Built-in default' : 'Inherit default'],
                    ['file', 'Pick a file'],
                    ['inline', 'Paste JSON'],
                  ] as const
                ).map(([m, label]) => (
                  <button
                    key={m}
                    className={`chip${mode === m ? ' on' : ''}`}
                    disabled={readOnly}
                    onClick={() => {
                      const next = { ...workflows }
                      if (m === 'inherit') delete next[family]
                      else if (m === 'file') next[family] = files[0]?.rel ?? ''
                      else next[family] = '{\n}'
                      setWorkflows(next)
                      // 'inline' is left unsaved on purpose -- the textarea's
                      // own onBlur/onChange persists it once there is real
                      // JSON to save, not the placeholder '{\n}'. 'inherit'
                      // and 'file' (when a file was actually picked) both set
                      // a complete, meaningful value here, so the dropdown
                      // never shows a selection the server does not have.
                      if (m === 'inherit' || (m === 'file' && files[0])) {
                        saveMap(SETTING_COMFY_WORKFLOW, next, run)
                      }
                    }}
                  >
                    {label}
                  </button>
                ))}
              </div>

              {mode === 'file' && (
                <>
                  <select
                    value={value}
                    disabled={readOnly}
                    onChange={(e) => {
                      const next = { ...workflows, [family]: e.target.value }
                      setWorkflows(next)
                      saveMap(SETTING_COMFY_WORKFLOW, next, run)
                    }}
                  >
                    <option value="">(choose a workflow)</option>
                    {files.map((f) => (
                      <option key={f.rel} value={f.rel} disabled={!f.api_format}>
                        {f.rel}
                        {f.api_format ? '' : ' — not API format, re-save from ComfyUI'}
                      </option>
                    ))}
                  </select>
                  {filesError && <p className="hint error-text">{filesError}</p>}
                  {!filesError && files.length === 0 && (
                    <p className="hint">
                      No workflows in that folder yet. In ComfyUI: enable{' '}
                      <em>Settings &rsaquo; Dev mode</em>, open a template, and use{' '}
                      <em>Save (API Format)</em>.
                    </p>
                  )}
                </>
              )}

              {mode === 'inline' && (
                <textarea
                  rows={12}
                  spellCheck={false}
                  value={value}
                  disabled={readOnly}
                  onChange={(e) => setWorkflows({ ...workflows, [family]: e.target.value })}
                  onBlur={() => saveMap(SETTING_COMFY_WORKFLOW, workflows, run)}
                />
              )}

              <div className="workflow-actions">
                <input
                  ref={adoptInput}
                  type="file"
                  accept="image/*"
                  hidden
                  onChange={(e) => {
                    const file = e.target.files?.[0]
                    e.target.value = ''
                    if (!file) return
                    void run(async () => {
                      const res = await adoptWorkflow(file)
                      const next = {
                        ...workflows,
                        [family]: JSON.stringify(res.workflow, null, 2),
                      }
                      setWorkflows(next)
                      saveMap(SETTING_COMFY_WORKFLOW, next, run)
                    }, 'Workflow adopted from the image.')
                  }}
                />
                <button disabled={readOnly || busy} onClick={() => adoptInput.current?.click()}>
                  Adopt from a rendered image
                </button>
                <button disabled={busy} onClick={() => void refreshWorkflows()}>
                  Refresh
                </button>
              </div>

              {status && (
                <p className={`hint${status.ok ? '' : ' error-text'}`}>
                  {status.error
                    ? status.error
                    : status.source === 'file'
                      ? `Using ${status.file}`
                      : status.source === 'inline'
                        ? 'Using a pasted workflow'
                        : family === ''
                          ? 'Using the built-in SDXL-shaped graph'
                          : 'Using the default slot'}
                  {(status.warnings ?? []).map((warn) => (
                    <span key={warn.code} className="warn-line">
                      {warn.message}
                    </span>
                  ))}
                </p>
              )}
            </div>
          )
        })()}

        <p className="hint">
          You do not need to edit the file. The lora, the seed and the prompt are
          rewritten per model at render time, on a copy — see{' '}
          <code>docs/comfyui-workflows.md</code>. Placeholders{' '}
          {(comfy?.placeholders ?? []).slice(0, 3).map((p) => (
            <code key={p}>{`{{${p}}}`}</code>
          ))}
          … are available when you want exact control instead.
        </p>

      </section>
    </div>
  )
}

// Folder maps are compared by value, treating a missing key and an empty
// string as the same thing -- which is what they mean here: blank is "the
// directory itself", the same as never having set one.
function sameFolders(a: Record<string, string>, b: Record<string, string>) {
  for (const k of new Set([...Object.keys(a), ...Object.keys(b)])) {
    if ((a[k] ?? '') !== (b[k] ?? '')) return false
  }
  return true
}

function FolderEditor({ root, defaults, map, readOnly, busy, onChange, onReset }: {
  root: Root
  defaults: FolderDefaults
  map: Record<string, string>
  readOnly: boolean
  // Gates the reset button only. The text fields deliberately stay enabled
  // while a save is in flight -- see the note on `draft` below, and note that
  // every other text setting on this panel is gated on readOnly alone.
  busy: boolean
  onChange: (byType: Record<string, string>) => void
  onReset: () => void
}) {
  const builtIn = defaults.defaults[root.tool ?? ''] ?? {}

  // The typed value is held here and persisted on blur, like every other text
  // setting on this panel.
  //
  // Saving on each keystroke instead made the field impossible to type in. The
  // write goes through run(), which sets busy for the duration of the request;
  // busy feeds this component's `disabled`; and a browser blurs an element the
  // moment it becomes disabled. So every character disabled the input, threw
  // the caret out, and re-enabled it -- one letter per click into the field.
  // It also meant one PUT per character.
  const [draft, setDraft] = useState<Record<string, string>>(map)

  // What we last handed to onChange, so the save coming back can be told apart
  // from a change made somewhere else.
  const sent = useRef<Record<string, string> | null>(null)

  // Re-sync when the stored map changes from outside: the reset button, or
  // settings arriving after the first paint.
  //
  // Keyed on the serialized value rather than the object, because the parent
  // passes a fresh `{}` on every render for a root with no overrides --
  // depending on identity would reset the draft continuously and wipe what was
  // being typed.
  //
  // The `sent` check is what stops this eating a keystroke. Committing one
  // field re-renders the parent with the value we just sent, which lands here
  // as a changed map; without the check it overwrites the draft, and any
  // character typed into the next field in those few milliseconds is lost. A
  // value comparison rather than a string one, because a map that has been
  // round-tripped through the server comes back with its keys reordered.
  const committed = JSON.stringify(map)
  useEffect(() => {
    if (sent.current && sameFolders(sent.current, map)) return
    setDraft(map)
    // map is fully described by `committed`; depending on it as well would
    // re-run this on every parent render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [committed])

  const commit = (t: string) => {
    if ((draft[t] ?? '') === (map[t] ?? '')) return
    sent.current = draft
    onChange(draft)
  }

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
              value={draft[t] ?? ''}
              placeholder={builtIn[t] || '(directory itself)'}
              disabled={readOnly}
              spellCheck={false}
              onChange={(e) => setDraft({ ...draft, [t]: e.target.value })}
              onBlur={() => commit(t)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') e.currentTarget.blur()
              }}
            />
          </label>
        ))}
      </div>
      <button disabled={readOnly || busy} onClick={onReset}>
        Reset to the {TOOL_LABELS[root.tool ?? ''] ?? 'built-in'} defaults
      </button>
    </div>
  )
}
