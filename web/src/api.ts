// API client.
//
// The daemon injects window.__MM__ into the page it serves, carrying the bearer
// token when one is required. A browser cannot read the token file off disk, so
// same-origin injection is the only way the bundled UI can authenticate (spec
// §11).

export interface AppConfig {
  token: string
  readOnly: boolean
  version: string
  /** Whether the daemon has a remote client and a writable enrichment manager. */
  enrichAvailable: boolean
}

declare global {
  interface Window {
    __MM__?: AppConfig
  }
}

export const config: AppConfig = window.__MM__ ?? {
  token: '',
  readOnly: true,
  version: 'dev',
  enrichAvailable: false,
}

export interface SearchHit {
  sha256: string
  name?: string
  type?: string
  base_model?: string
  version?: string
  origin?: string
  format: string
  size: number
  trigger_words?: string[]
  tags?: string[]
  preview_image?: string
  filename?: string
  path?: string
  path_count: number
  present: boolean
  nsfw?: boolean
}

export interface SearchResults {
  hits: SearchHit[]
  total: number
  limit: number
  offset: number
}

export interface ModelRecord {
  sha256: string
  type?: string
  base_model?: string
  name?: string
  version?: string
  description?: string
  trigger_words?: string[]
  recommended_weight?: number
  recommended_settings?: string
  nsfw?: boolean
  origin?: string
  updated_at?: string
}

export interface FilePath {
  ID: number
  Path: string
  Root: string
  Present: boolean
  Provisional: boolean
  Size: number
}

export interface PreviewImage {
  id: number
  image_sha256: string
  mime: string
  bytes: number
  width?: number
  height?: number
  source: string
  position: number
  // A small derived copy for the grid. Absent means the full image was already
  // small enough to serve directly.
  thumb_sha256?: string
  // Present when the image was a ComfyUI render carrying its own graph.
  workflow_sha256?: string
}

export interface TrainingRecord {
  sha256: string
  dataset?: string
  dataset_size?: number
  base?: string
  config?: Record<string, unknown>
  trainer?: string
  notes?: string
  run_date?: string
  source: string
}

export interface Suggestion {
  id: number
  sha256: string
  field: string
  manual_value: string
  suggested_value: string
  source: string
  status: string
}

export interface ModelDetail {
  sha256: string
  weights_sha256?: string
  size: number
  format: string
  first_seen: string
  last_verified: string
  record?: ModelRecord
  paths: FilePath[]
  previews: PreviewImage[]
  tags: string[]
  training?: TrainingRecord
  suggestions: Suggestion[]
  header_truncated?: boolean
}

export interface CandidateEntry {
  value: unknown
  source: string
  tier: number
  tier_name: string
  observed_at: string
}

export interface CandidateView {
  field: string
  winner: CandidateEntry
  losers?: CandidateEntry[]
}

export interface Facets {
  types: Record<string, number>
  base_models: Record<string, number>
  origins: Record<string, number>
  formats: Record<string, number>
  tags: Record<string, number>
  // How many models the current query matches.
  total: number
  // How many models exist at all. Distinct from `total` because "nothing
  // matches" and "there is nothing here yet" need different advice.
  library_total: number
}

export interface Filters {
  q: string
  type: string[]
  base_model: string[]
  tag: string[]
  present?: boolean
  needs_attention?: boolean
  sort: string
  order: 'asc' | 'desc'
}

export const emptyFilters: Filters = {
  q: '',
  type: [],
  base_model: [],
  tag: [],
  sort: 'name',
  order: 'asc',
}

// The canonical model types, mirroring internal/modeltype. Kept in step with
// the server rather than invented here: this list drives the quick-filter chips
// and the per-type folder settings, and a type the server does not know is a
// filter that silently matches nothing.
export const MODEL_TYPES = [
  'checkpoint',
  'lora',
  'lycoris',
  'embedding',
  'vae',
  'controlnet',
  'upscaler',
  'hypernetwork',
] as const

function headers(): HeadersInit {
  const h: Record<string, string> = { 'Content-Type': 'application/json' }
  if (config.token) h.Authorization = `Bearer ${config.token}`
  return h
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, { ...init, headers: headers() })
  if (!res.ok) {
    // The daemon returns a structured error; surfacing its detail is the
    // difference between "something failed" and knowing the Host header was
    // rejected or the server is read-only.
    let detail = res.statusText
    try {
      const body = await res.json()
      detail = body.detail || body.error || detail
    } catch {
      // Not JSON. The security middleware writes plain text with the exact
      // fix in it ("add the hostname with --allow-host..."); showing a bare
      // "Forbidden" instead of that message defeats its purpose.
      try {
        const text = await res.clone().text()
        if (text.trim()) detail = text.trim()
      } catch {
        /* the status text will have to do */
      }
    }
    throw new Error(detail)
  }
  if (res.status === 204) return undefined as T
  return res.json() as Promise<T>
}

// filterParams serializes the filter set the server reads with searchQueryFrom.
//
// Written once because three callers need it -- the result list, the facet
// counts beside it, and a bulk action over "the models I am looking at" -- and
// the first two once drifted into describing different sets, so the sidebar
// promised 400 loras next to a list of 12. Sort and paging are excluded: they
// describe a page of results, not which models the filters select.
export function filterParams(filters?: Filters): URLSearchParams {
  const params = new URLSearchParams()
  if (!filters) return params
  if (filters.q) params.set('q', filters.q)
  for (const t of filters.type) params.append('type', t)
  for (const b of filters.base_model) params.append('base_model', b)
  for (const t of filters.tag) params.append('tag', t)
  if (filters.present !== undefined) params.set('present', String(filters.present))
  if (filters.needs_attention) params.set('needs_attention', 'true')
  return params
}

export function searchModels(filters: Filters, offset = 0, limit = 60): Promise<SearchResults> {
  const params = filterParams(filters)
  params.set('sort', filters.sort)
  params.set('order', filters.order)
  params.set('limit', String(limit))
  params.set('offset', String(offset))
  return request<SearchResults>(`/api/models?${params}`)
}

// Remote browsing.
//
// A Listing is a claim about a file that is not here yet, which is why it has
// no sha256 of its own and why `local` is the only field that relates it to the
// library. That relation is computed by content hash on the server, so "have"
// means the same bytes are on disk -- not that something with a similar name is.

export interface RemoteFile {
  name: string
  size_bytes?: number
  sha256?: string
  format?: string
  primary?: boolean
  download_url?: string
  requires_auth?: boolean
}

export interface LocalMatch {
  status: 'have' | 'outdated' | 'new'
  sha256?: string
  path?: string
  have_version_id?: string
  have_version_name?: string
}

export interface Listing {
  provider: string
  id: string
  version_id?: string
  version_name?: string
  name: string
  author?: string
  type?: string
  base_model?: string
  description?: string
  tags?: string[]
  nsfw?: boolean
  downloads?: number
  likes?: number
  published_at?: string
  updated_at?: string
  page_url?: string
  thumbnail_url?: string
  trigger_words?: string[]
  files?: RemoteFile[]
  local?: LocalMatch
}

export interface BrowseResults {
  items: Listing[]
  errors?: Record<string, string>
  providers: string[]
}

export interface BrowseQuery {
  q: string
  providers: string[]
  type: string[]
  base_model: string[]
  nsfw: boolean
  sort: string
  page: number
}

export const emptyBrowseQuery: BrowseQuery = {
  q: '',
  providers: [],
  type: [],
  base_model: [],
  nsfw: false,
  sort: 'downloads',
  page: 1,
}

export function browse(q: BrowseQuery, limit = 24): Promise<BrowseResults> {
  const params = new URLSearchParams()
  if (q.q) params.set('q', q.q)
  for (const p of q.providers) params.append('provider', p)
  for (const t of q.type) params.append('type', t)
  for (const b of q.base_model) params.append('base_model', b)
  if (q.nsfw) params.set('nsfw', 'true')
  if (q.sort) params.set('sort', q.sort)
  params.set('page', String(q.page))
  params.set('limit', String(limit))
  return request<BrowseResults>(`/api/browse?${params}`)
}

// Thumbnails go through the daemon rather than straight to the provider CDN.
// The page's CSP is img-src 'self', so a remote URL would be refused outright,
// and fetching it directly would tell that CDN who is browsing and for what.
export function remoteImageURL(url: string): string {
  const params = new URLSearchParams({ url })
  if (config.token) params.set('token', config.token)
  return `/api/remote-image?${params}`
}

// Downloads.
//
// The destination is never free text. The server only accepts a root it has
// already scanned, so the UI offers a choice from the list it publishes rather
// than letting anyone type a path.

export interface DownloadJob {
  id: string
  url: string
  dest_dir: string
  dest_root?: string
  filename: string
  expected_sha256?: string
  expected_size?: number
  state: 'pending' | 'downloading' | 'verifying' | 'complete' | 'failed' | 'quarantined' | 'cancelled'
  downloaded: number
  total: number
  error?: string
  actual_sha256?: string
  final_path?: string
  quarantine_path?: string
  // Set when the file downloaded and verified but could not be indexed: it
  // exists on disk, the library just does not show it yet.
  index_error?: string
  started_at: string
}

export const isJobActive = (j: DownloadJob) =>
  j.state === 'pending' || j.state === 'downloading' || j.state === 'verifying'

export const isJobTerminalFailure = (j: DownloadJob) =>
  j.state === 'failed' || j.state === 'quarantined' || j.state === 'cancelled'

export interface StartDownload {
  url: string
  dest_root: string
  // Left unset, the server picks the subfolder from (dest_root, type). Setting
  // it overrides that -- the escape hatch, not the normal path.
  subdir?: string
  // The provider's type string. Normalized server-side; anything unrecognised
  // falls back to the root rather than becoming a directory name.
  type?: string
  filename?: string
  sha256?: string
  size?: number
}

export const downloadRoots = () => request<string[]>('/api/downloads/roots')
export const listDownloads = () => request<DownloadJob[]>('/api/downloads')

// A 409 means the download is already running -- benign from the user's
// point of view -- and the response still carries the job id, so it is
// resolved rather than thrown.
export async function startDownload(req: StartDownload): Promise<{ status: string; id: string }> {
  const res = await fetch('/api/downloads', {
    method: 'POST',
    headers: headers(),
    body: JSON.stringify(req),
  })
  const body = await res.json().catch(() => ({}))
  if (res.status === 409 && body.id) return { status: 'in_progress', id: body.id }
  if (!res.ok) throw new Error(body.detail || body.error || res.statusText)
  return body
}

export const cancelDownload = (id: string) =>
  request<{ status: string }>(`/api/downloads/${encodeURIComponent(id)}`, { method: 'DELETE' })

export interface Update {
  provider: string
  model_id: string
  name: string
  have_version_id?: string
  have_version_name?: string
  latest_version_id: string
  latest_version_name?: string
  published_at?: string
  local_sha256?: string
  local_path?: string
  base_model?: string
  size_bytes?: number
  download_url?: string
  page_url?: string
  base_model_changed?: boolean
}

export interface UpdatesResults {
  updates: Update[]
  checked: number
  errors: number
}

// No limit by default: the check is one request per owned model and the server
// bounds it, so the UI does not also guess at a cap.
export const checkUpdates = () => request<UpdatesResults>('/api/updates')

export const getModel = (sha: string) => request<ModelDetail>(`/api/models/${sha}`)
export const getCandidates = (sha: string) => request<CandidateView[]>(`/api/models/${sha}/candidates`)
// Facets take the same filters as the search, so the counts describe the
// results rather than the whole library.
export const getFacets = (filters?: Filters) => {
  const qs = filterParams(filters).toString()
  return request<Facets>(qs ? `/api/facets?${qs}` : '/api/facets')
}
export const getSuggestions = () => request<Suggestion[]>('/api/suggestions')

// --- enrichment ---------------------------------------------------------------
//
// Pulling metadata and previews from the origin. Nothing here decides what wins:
// everything fetched is recorded as an ordinary origin-tier observation and
// resolved server-side by the usual rules, so a value you typed still wins, a
// blank field takes the best answer available, and a thumbnail you chose cannot
// be displaced by a fetched one.

export interface EnrichResult {
  found: boolean
  from_archive: boolean
  images_added: number
  previews_before: number
  previews_after: number
  errors: number
}

export interface EnrichJob {
  id: string
  scope: string
  state: 'running' | 'complete' | 'failed' | 'cancelled'
  started_at: string
  finished_at?: string
  models_total: number
  models_done: number
  fetched: number
  cache_hits: number
  found: number
  missing: number
  images: number
  errors: number
  /** The origin cut the run short before models_done reached models_total. */
  rate_limited: boolean
  last_error?: string
  error?: string
}

export interface EnrichOptions {
  /** Re-ask even when a response is already archived. */
  refresh?: boolean
  /** Fetch preview images too. Defaults to true server-side. */
  images?: boolean
  maxImages?: number
  /** Stop after this many models. 0 or omitted means all of them. */
  limit?: number
}

function enrichParams(opts: EnrichOptions | undefined, params: URLSearchParams): URLSearchParams {
  if (!opts) return params
  if (opts.refresh !== undefined) params.set('refresh', String(opts.refresh))
  if (opts.images !== undefined) params.set('images', String(opts.images))
  if (opts.maxImages !== undefined) params.set('max_images', String(opts.maxImages))
  if (opts.limit) params.set('limit', String(opts.limit))
  return params
}

/** Refresh one model. Synchronous: a single lookup is quicker than a job to poll. */
export const enrichModel = (sha: string, opts?: EnrichOptions) => {
  const qs = enrichParams(opts, new URLSearchParams()).toString()
  return request<{ detail: ModelDetail; result: EnrichResult }>(
    `/api/models/${sha}/enrich${qs ? `?${qs}` : ''}`,
    { method: 'POST' },
  )
}

/**
 * Start a background sweep.
 *
 * With filters, the sweep covers every model matching them — not just the page
 * on screen — because the server re-runs the same query rather than taking a
 * list of hashes from the client.
 */
export const startEnrich = (filters?: Filters, opts?: EnrichOptions) => {
  const params = enrichParams(opts, filterParams(filters))
  params.set('scope', filters ? 'search' : 'all')
  return request<EnrichJob>(`/api/enrich?${params}`, { method: 'POST' })
}

export const activeEnrich = () =>
  request<{ job: EnrichJob | null; available: boolean }>('/api/enrich')

export const cancelEnrich = (id: string) =>
  request<void>(`/api/enrich/${encodeURIComponent(id)}`, { method: 'DELETE' })

export const updateModel = (sha: string, patch: Record<string, unknown>) =>
  request<ModelRecord>(`/api/models/${sha}`, { method: 'PATCH', body: JSON.stringify(patch) })

export const setTags = (sha: string, tags: string[]) =>
  request<string[]>(`/api/models/${sha}/tags`, { method: 'PUT', body: JSON.stringify(tags) })

export const putTraining = (sha: string, record: Partial<TrainingRecord>) =>
  request<TrainingRecord>(`/api/models/${sha}/training`, {
    method: 'PUT',
    body: JSON.stringify(record),
  })

export const acceptSuggestion = (id: number) =>
  request<void>(`/api/suggestions/${id}/accept`, { method: 'POST' })

export const dismissSuggestion = (id: number) =>
  request<void>(`/api/suggestions/${id}/dismiss`, { method: 'POST' })

// The token goes in the query string here because an <img> tag cannot carry an
// Authorization header. It is no weaker: both are same-origin values this page
// already holds.
export function previewURL(imageSha: string): string {
  return config.token
    ? `/api/previews/${imageSha}?token=${encodeURIComponent(config.token)}`
    : `/api/previews/${imageSha}`
}

export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB']
  let value = n / 1024
  let i = 0
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024
    i++
  }
  return `${value.toFixed(value < 10 ? 2 : 1)} ${units[i]}`
}

// --- managed roots ------------------------------------------------------------
//
// A root is a directory the library indexes, and every root is also a legal
// download destination -- which is why adding one is a server-side operation
// with canonicalization and overlap checks, not a path the UI can invent.

export interface Root {
  id: number
  path: string
  label?: string
  tool?: string
  enabled: boolean
  added_at: string
  last_scanned_at?: string
  files: number
  bytes: number
}

export interface ScanJob {
  id: string
  roots: string[]
  state: 'running' | 'complete' | 'failed' | 'cancelled'
  started_at: string
  finished_at?: string
  files_total: number
  files_done: number
  files_hashed: number
  files_cached: number
  bytes_total: number
  bytes_done: number
  errors: number
  error?: string
}

export const listRoots = () => request<{ roots: Root[] }>('/api/roots').then((r) => r.roots)

export const addRoot = (path: string, label?: string, tool?: string) =>
  request<{ root: Root; scan?: ScanJob; scan_deferred?: string }>('/api/roots', {
    method: 'POST',
    body: JSON.stringify({ path, label, tool }),
  })

export const patchRoot = (id: number, patch: { enabled?: boolean; label?: string; tool?: string }) =>
  request<{ root: Root }>(`/api/roots/${id}`, { method: 'PATCH', body: JSON.stringify(patch) })

export const removeRoot = (id: number) =>
  request<{ status: string }>(`/api/roots/${id}`, { method: 'DELETE' })

export const startScan = (roots?: string[]) =>
  request<{ scan: ScanJob }>('/api/scans', {
    method: 'POST',
    body: JSON.stringify({ roots: roots ?? [] }),
  })

export const activeScan = () =>
  request<{ scan: ScanJob | null }>('/api/scans/active').then((r) => r.scan)

export const cancelScan = (id: string) =>
  request<{ status: string }>(`/api/scans/${encodeURIComponent(id)}`, { method: 'DELETE' })

export interface DetectedInstall {
  Tool: string
  Path: string
  ModelRoots: string[]
}

export const detectInstalls = () =>
  request<{ installs: DetectedInstall[]; model_roots: string[] }>('/api/detect')

// --- settings -----------------------------------------------------------------
//
// Server-side, not localStorage: the same daemon serves the desktop and the
// phone over the tailnet, and a view configured on one should be the view on
// the other.

export const SETTING_FILTERS = 'library.filters'
export const SETTING_DEFAULT_ROOT = 'downloads.default_root'
export const SETTING_FOLDER_MAP = 'downloads.folder_map'
export const SETTING_COMFY_OUTPUT = 'thumbnails.comfy_output_dir'
export const SETTING_COMFY_URL = 'thumbnails.comfy_url'
export const SETTING_COMFY_WORKFLOW = 'thumbnails.comfy_workflow'
export const SETTING_COMFY_CHECKPOINT = 'thumbnails.comfy_checkpoint'
export const SETTING_COMFY_WORKFLOW_DIR = 'thumbnails.comfy_workflow_dir'

// A (root path -> type -> subfolder) map. One type has three different folder
// names across the three tools, so the mapping can only be per (root, type).
export type FolderMap = Record<string, Record<string, string>>

export const getSettings = () =>
  request<{ settings: Record<string, unknown> }>('/api/settings').then((r) => r.settings)

export const putSetting = (key: string, value: unknown) =>
  request<{ key: string; value: unknown }>(`/api/settings/${encodeURIComponent(key)}`, {
    method: 'PUT',
    body: JSON.stringify(value),
  })

export const deleteSetting = (key: string) =>
  request<{ status: string }>(`/api/settings/${encodeURIComponent(key)}`, { method: 'DELETE' })

export interface FolderDefaults {
  types: string[]
  tools: string[]
  defaults: Record<string, Record<string, string>>
}

export const folderDefaults = () => request<FolderDefaults>('/api/downloads/folder-defaults')

export interface ResolvedDestination {
  root: string
  subdir: string
  dest_dir: string
  type: string
}

// Where a download will actually land. Resolved by the server, because the
// subfolder depends on which tool's vocabulary the root uses -- something the
// browser has no way to know.
export const resolveDestination = (root: string, type?: string) => {
  const params = new URLSearchParams()
  if (root) params.set('root', root)
  if (type) params.set('type', type)
  return request<ResolvedDestination>(`/api/downloads/destination?${params}`)
}

// --- previews -----------------------------------------------------------------

export async function uploadPreview(sha: string, file: File | Blob): Promise<PreviewImage> {
  // Raw body, not multipart: the server sniffs the bytes and ignores any
  // declared type, so a form envelope would only add a layer to unwrap.
  const h: Record<string, string> = {}
  if (config.token) h.Authorization = `Bearer ${config.token}`
  const res = await fetch(`/api/models/${sha}/previews`, { method: 'POST', headers: h, body: file })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.detail || body.error || res.statusText)
  return body.preview as PreviewImage
}

export const attachGeneratedPreview = (sha: string, rel: string) =>
  request<{ preview: PreviewImage }>(`/api/models/${sha}/previews/generated`, {
    method: 'POST',
    body: JSON.stringify({ rel }),
  }).then((r) => r.preview)

export const deletePreview = (sha: string, imageSha: string) =>
  request<void>(`/api/models/${sha}/previews/${imageSha}`, { method: 'DELETE' })

export const reorderPreviews = (sha: string, order: string[]) =>
  request<{ previews: PreviewImage[] }>(`/api/models/${sha}/previews/order`, {
    method: 'PUT',
    body: JSON.stringify({ order }),
  }).then((r) => r.previews)

export function workflowURL(sha: string, imageSha: string): string {
  const base = `/api/models/${sha}/previews/${imageSha}/workflow`
  return config.token ? `${base}?token=${encodeURIComponent(config.token)}` : base
}

export interface GeneratedImage {
  name: string
  rel: string
  bytes: number
  modified: string
}

export const listGenerated = (limit = 60) =>
  request<{ dir: string; images: GeneratedImage[] }>(`/api/generated?limit=${limit}`)

// --- rendering with ComfyUI ----------------------------------------------------
//
// The one feature that needs ComfyUI *running*. Everything else about ComfyUI
// here -- the folder names, the workflow inside a PNG, the output folder --
// works whether it is up or not, so the UI asks first and only offers the
// button when there is something to ask.

export interface ComfyStatus {
  configured: boolean
  reachable: boolean
  url?: string
  version?: string
  error?: string
  placeholders: string[]
  // The base-model families the settings UI offers a workflow slot for. One
  // graph cannot serve four architectures, so the UI has to know which exist.
  base_models: string[]
}

export interface RenderJob {
  id: string
  sha256: string
  state: 'queued' | 'running' | 'complete' | 'failed' | 'cancelled'
  prompt_id?: string
  started_at: string
  ended_at?: string
  image_sha256?: string
  error?: string
}

export const isRenderActive = (j: RenderJob) => j.state === 'queued' || j.state === 'running'

// The status probe is model-independent, but PreviewEditor asks it once per
// model opened, and the endpoint behind it does a live 5-second-timeout ping
// against ComfyUI. A short cache is what keeps browsing through a library from
// re-pinging ComfyUI (or, when it is down, re-waiting out the timeout) on
// every model opened.
const COMFY_STATUS_TTL_MS = 15_000
let comfyStatusCache: { at: number; value: Promise<ComfyStatus> } | null = null

export const comfyStatus = () => {
  const now = Date.now()
  if (comfyStatusCache && now - comfyStatusCache.at < COMFY_STATUS_TTL_MS) {
    return comfyStatusCache.value
  }
  const value = request<ComfyStatus>('/api/comfy')
  comfyStatusCache = { at: now, value }
  return value
}

export interface RenderRequest {
  prompt?: string
  negative?: string
  checkpoint?: string
  seed?: number
  // A graph to run instead of the configured template. Must be ComfyUI's API
  // format -- the editor format cannot be queued, and the server says so.
  workflow?: unknown
}

// A 409 means this model is already rendering, which is not a failure from the
// user's point of view: the job comes back so the UI can attach to it.
export async function renderPreview(sha: string, req: RenderRequest = {}): Promise<RenderJob> {
  const res = await fetch(`/api/models/${sha}/previews/render`, {
    method: 'POST',
    headers: headers(),
    body: JSON.stringify(req),
  })
  const body = await res.json().catch(() => ({}))
  if (res.status === 409 && body.render) return body.render as RenderJob
  if (!res.ok) throw new Error(body.detail || body.error || res.statusText)
  return body.render as RenderJob
}

export const listRenders = () =>
  request<{ renders: RenderJob[] }>('/api/renders').then((r) => r.renders)

export const cancelRender = (id: string) =>
  request<{ status: string }>(`/api/renders/${encodeURIComponent(id)}`, { method: 'DELETE' })

// --- workflows ------------------------------------------------------------------
//
// A family slot holds either a graph or the *name of a file* holding one.
// Naming a file is the better of the two: the workflow stays where ComfyUI saved
// it, stays editable there, and the next render picks the edit up.

export interface WorkflowFile {
  name: string
  rel: string
  // False for a graph saved from the canvas rather than with Save (API Format).
  // Shown but unusable, because "my workflow isn't in the list" is a worse
  // puzzle than "this one is the wrong format".
  api_format: boolean
  note?: string
  warnings?: ComfyWarning[]
}

export interface ComfyWarning {
  code: string
  message: string
}

export interface FamilyStatus {
  family: string
  source: 'file' | 'inline' | 'default' | 'inherited'
  file?: string
  checkpoint?: string
  ok: boolean
  error?: string
  warnings?: ComfyWarning[]
}

export const listWorkflows = () =>
  request<{ dir: string; workflows: WorkflowFile[]; error?: string }>('/api/comfy/workflows')

export const workflowStatus = () =>
  request<{ families: FamilyStatus[] }>('/api/comfy/status').then((r) => r.families)

// The render-plan endpoint (POST /api/models/{sha}/previews/render/plan) has
// no client here: it is only consumed by `mm comfy plan`. Add a typed wrapper
// alongside whatever UI first needs it, rather than carrying types nothing
// calls -- unused API surface type-checks fine right up until the response
// shape actually changes.

// A ComfyUI render carries the graph that made it. Adopting one is the shortest
// path from "I have a workflow that works" to "the app uses it".
export async function adoptWorkflow(file: File | Blob): Promise<{
  chunk: string
  workflow: unknown
  warnings: ComfyWarning[]
}> {
  const h: Record<string, string> = {}
  if (config.token) h.Authorization = `Bearer ${config.token}`
  const res = await fetch('/api/comfy/adopt', { method: 'POST', headers: h, body: file })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.detail || body.error || res.statusText)
  return body
}
