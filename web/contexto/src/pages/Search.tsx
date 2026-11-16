import { useEffect, useState } from 'react'
import { api, type CIBrief } from '../api'
import { Card, CILink, LifecycleBadge, Link, Loading, Sites, useApi } from '../common'
import { navigate } from '../router'

export default function Search({ url }: { url: URL }) {
  const p = url.searchParams
  const [q, setQ] = useState(p.get('q') || '')
  useEffect(() => setQ(p.get('q') || ''), [url.search])
  const params = {
    q: p.get('q') || '',
    class: p.get('class') || '',
    technology: p.get('technology') || '',
    site: p.get('site') || '',
    lifecycle: p.get('lifecycle') || '',
    limit: 100,
    offset: Number(p.get('offset') || 0),
  }
  const st = useApi(() => api<{ total: number; items: CIBrief[] }>('/ci', params), [url.search])
  const set = (k: string, v: string) => {
    const n = new URLSearchParams(url.search)
    if (v) n.set(k, v)
    else n.delete(k)
    n.delete('offset')
    navigate('/ci?' + n)
  }
  const filters = (['class', 'technology', 'site', 'lifecycle'] as const).filter((k) => params[k])
  return (
    <>
      <h1>Buscar elementos</h1>
      <form
        className="searchbar"
        onSubmit={(e) => {
          e.preventDefault()
          set('q', q.trim())
        }}
      >
        <input value={q} onChange={(e) => setQ(e.target.value)} placeholder="Nombre, COD_SSII, IP, WWN, serie…" autoFocus />
        <button>Buscar</button>
      </form>
      {filters.length > 0 && (
        <p className="filters">
          {filters.map((k) => (
            <span key={k} className="chip">
              {k}: {params[k]}{' '}
              <a href="#" onClick={(e) => (e.preventDefault(), set(k, ''))}>
                ×
              </a>
            </span>
          ))}
        </p>
      )}
      <Loading state={st} />
      {st.data && (
        <Card title={st.data.total.toLocaleString('es-ES') + ' resultados'}>
          <table className="tbl">
            <thead>
              <tr>
                <th>Clase</th>
                <th>Nombre</th>
                <th>Sede</th>
                <th>Sistema de Información</th>
                <th>Estado</th>
              </tr>
            </thead>
            <tbody>
              {st.data.items.map((c) => (
                <tr key={c.id}>
                  <td>
                    <a href="#" onClick={(e) => (e.preventDefault(), set('class', c.class))} className="muted">
                      {c.class}
                    </a>
                  </td>
                  <td>
                    <CILink ci={c} showClass={false} />
                  </td>
                  <td>
                    <Sites sites={c.sites} />
                  </td>
                  <td>{c.ssii ? <Link to={'/ssii/' + encodeURIComponent(c.ssii)}>{c.ssii}</Link> : <span className="muted">—</span>}</td>
                  <td>
                    <LifecycleBadge lc={c.lifecycle} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="pager">
            {params.offset > 0 && <Link to={'/ci?' + withOffset(url, params.offset - 100)}>← Anteriores</Link>}
            {params.offset + 100 < st.data.total && <Link to={'/ci?' + withOffset(url, params.offset + 100)}>Siguientes →</Link>}
          </div>
        </Card>
      )}
    </>
  )
}

function withOffset(url: URL, off: number) {
  const n = new URLSearchParams(url.search)
  n.set('offset', String(Math.max(0, off)))
  return n.toString()
}
