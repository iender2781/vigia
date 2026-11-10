// Cliente de la API /api/v1 con tipos.

export interface CIBrief {
  id: string
  class: string
  technology: string
  name: string
  sites: string[] | null
  ssii?: string
  lifecycle: Lifecycle
}
export type Lifecycle = 'active' | 'nodata' | 'stale' | 'retired'

export interface CIView extends CIBrief {
  key: string
  attrs: Record<string, string> | null
  manual_attrs?: Record<string, string>
  sources: string[] | null
  jobs: string[] | null
  lifecycle_since: string
  first_seen: string
  last_seen: string
}

export interface Edge {
  id: number
  src: string
  rel: string
  dst: string
  method: string
  origin: string
  attrs?: Record<string, string>
  valid_from: string
}
export interface RelView extends Edge {
  peer: CIBrief
  causal: boolean
  informative: boolean
  identity: boolean
}
export interface GraphNode extends CIBrief {
  depth: number
  side?: string
}
export interface Graph {
  nodes: GraphNode[]
  edges: Edge[]
  truncated: boolean
}
export interface Count {
  key: string
  n: number
}
export interface Run {
  id: number
  started_at: string
  finished_at?: string
  status: string
  instance: string
  stats: any
  error?: string
}
export interface Stats {
  cis: number
  relations: number
  ssii: number
  by_class: Count[] | null
  by_technology: Count[] | null
  by_lifecycle: Count[] | null
  by_site: Count[] | null
  last_run?: Run
}
export interface Issue {
  kind: string
  severity: 'info' | 'warning' | 'error'
  subject: string
  count: number
  detail?: Record<string, any>
}
export interface SSIISummary {
  code: string
  cis: number
  on: number
  off: number
  not_fresh: number
  sites: string[] | null
}
export interface EventItem {
  id: number
  ts: string
  type: string
  ci_id?: string
  data: any
}
export interface AttrChange {
  at: string
  attr: string
  old: string | null
  new: string | null
  source: string
}

const TOKEN_KEY = 'contexto.token'

export function getToken(): string {
  try {
    return localStorage.getItem(TOKEN_KEY) || ''
  } catch {
    return ''
  }
}
export function setToken(t: string) {
  try {
    if (t) localStorage.setItem(TOKEN_KEY, t)
    else localStorage.removeItem(TOKEN_KEY)
  } catch {
    /* almacenamiento no disponible */
  }
}

export class AuthError extends Error {}

export async function api<T>(path: string, params?: Record<string, string | number | undefined>): Promise<T> {
  const q = new URLSearchParams()
  for (const [k, v] of Object.entries(params || {})) if (v !== undefined && v !== '') q.set(k, String(v))
  const url = '/api/v1' + path + (q.toString() ? '?' + q : '')
  const headers: Record<string, string> = { Accept: 'application/json' }
  const tok = getToken()
  if (tok) headers.Authorization = 'Bearer ' + tok
  const r = await fetch(url, { headers })
  if (r.status === 401) throw new AuthError('no autenticado')
  if (!r.ok) {
    let msg = r.statusText
    try {
      msg = (await r.json()).error || msg
    } catch {
      /* sin cuerpo JSON */
    }
    throw new Error(msg)
  }
  return r.json()
}

export const lifecycleLabel: Record<string, string> = {
  active: 'Activo',
  nodata: 'Sin datos',
  stale: 'Obsoleto',
  retired: 'Retirado',
}

export function fmtDate(s?: string | null): string {
  if (!s) return '—'
  const d = new Date(s)
  return d.toLocaleString('es-ES', { dateStyle: 'short', timeStyle: 'short' })
}

export function ago(s?: string | null): string {
  if (!s) return ''
  const secs = Math.max(0, (Date.now() - new Date(s).getTime()) / 1000)
  if (secs < 90) return 'hace ' + Math.round(secs) + ' s'
  if (secs < 5400) return 'hace ' + Math.round(secs / 60) + ' min'
  if (secs < 172800) return 'hace ' + Math.round(secs / 3600) + ' h'
  return 'hace ' + Math.round(secs / 86400) + ' días'
}
