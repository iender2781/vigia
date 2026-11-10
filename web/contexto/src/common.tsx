import { createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { AuthError, lifecycleLabel, type CIBrief } from './api'
import { onLinkClick } from './router'

// Se incrementa cada vez que termina un ciclo de descubrimiento (canal SSE): las vistas
// que lo usan vuelven a pedir sus datos.
export const RefreshContext = createContext(0)
export const AuthContext = createContext<() => void>(() => {})

export function useApi<T>(fn: () => Promise<T>, deps: unknown[]) {
  const tick = useContext(RefreshContext)
  const onAuth = useContext(AuthContext)
  const [state, setState] = useState<{ data?: T; error?: string; loading: boolean }>({ loading: true })
  useEffect(() => {
    let live = true
    setState((s) => ({ ...s, loading: true }))
    fn()
      .then((data) => live && setState({ data, loading: false }))
      .catch((e) => {
        if (!live) return
        if (e instanceof AuthError) onAuth()
        setState({ error: String(e.message || e), loading: false })
      })
    return () => {
      live = false
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, tick])
  return state
}

export function Link(props: { to: string; children: ReactNode; className?: string; title?: string }) {
  return (
    <a href={props.to} onClick={onLinkClick} className={props.className} title={props.title}>
      {props.children}
    </a>
  )
}

export function LifecycleBadge({ lc }: { lc: string }) {
  return <span className={'badge lc-' + lc}>{lifecycleLabel[lc] || lc}</span>
}

export function CILink({ ci, showClass = true }: { ci: CIBrief; showClass?: boolean }) {
  return (
    <Link to={'/ci/' + ci.id} className="ci-link" title={ci.class + ' · ' + ci.name}>
      {showClass && <span className="cls">{ci.class}</span>}
      <span className="nm">{ci.name}</span>
      {ci.lifecycle !== 'active' && <LifecycleBadge lc={ci.lifecycle} />}
    </Link>
  )
}

export function Sites({ sites }: { sites?: string[] | null }) {
  if (!sites || sites.length === 0) return <span className="muted">—</span>
  return (
    <>
      {sites.map((s) => (
        <span key={s} className="chip">
          {s}
        </span>
      ))}
    </>
  )
}

export function Loading({ state }: { state: { loading: boolean; error?: string } }) {
  if (state.error) return <div className="error">No se pudo cargar: {state.error}</div>
  if (state.loading) return <div className="muted pad">Cargando…</div>
  return null
}

export function Card({ title, children, actions }: { title?: ReactNode; children: ReactNode; actions?: ReactNode }) {
  return (
    <section className="card">
      {(title || actions) && (
        <header className="card-h">
          <h2>{title}</h2>
          <div>{actions}</div>
        </header>
      )}
      {children}
    </section>
  )
}

export function Bars({ items, max, linkFor }: { items: { key: string; n: number }[]; max?: number; linkFor?: (k: string) => string }) {
  const top = max ?? Math.max(1, ...items.map((i) => i.n))
  return (
    <ul className="bars">
      {items.map((i) => (
        <li key={i.key}>
          <span className="bk">{linkFor ? <Link to={linkFor(i.key)}>{i.key || '—'}</Link> : i.key || '—'}</span>
          <span className="bb">
            <span style={{ width: (100 * i.n) / top + '%' }} />
          </span>
          <span className="bn">{i.n.toLocaleString('es-ES')}</span>
        </li>
      ))}
    </ul>
  )
}
