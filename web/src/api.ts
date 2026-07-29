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
      /* not JSON; the status text will have to do */
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
