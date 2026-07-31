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
  total: number
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

export function searchModels(filters: Filters, offset = 0, limit = 60): Promise<SearchResults> {
  const params = new URLSearchParams()
  if (filters.q) params.set('q', filters.q)
  for (const t of filters.type) params.append('type', t)
  for (const b of filters.base_model) params.append('base_model', b)
  for (const t of filters.tag) params.append('tag', t)
  if (filters.present !== undefined) params.set('present', String(filters.present))
  if (filters.needs_attention) params.set('needs_attention', 'true')
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
  subdir?: string
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
export const getFacets = () => request<Facets>('/api/facets')
export const getSuggestions = () => request<Suggestion[]>('/api/suggestions')

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
