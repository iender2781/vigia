import { useState } from 'react'
import { api, type CIBrief, type Graph, type SSIISummary } from '../api'
import { Card, CILink, Link, Loading, Sites, useApi } from '../common'
import GraphView from '../GraphView'

export function SSIIList() {
  const [q, setQ] = useState('')
  const st = useApi(() => api<{ attr: string; items: SSIISummary[] }>('/ssii'), [])
  const items = (st.data?.items || []).filter((x) => x.code.toLowerCase().includes(q.toLowerCase()))
  return (
    <>
      <h1>Sistemas de Información</h1>
      <p className="muted">Agrupados por el atributo {st.data?.attr || 'COD_SSII'} de cada elemento.</p>
      <input className="filter" placeholder="Filtrar por código…" value={q} onChange={(e) => setQ(e.target.value)} />
      <Loading state={st} />
      {st.data && (
        <Card title={items.length + ' sistemas'}>
          <table className="tbl">
            <thead>
              <tr>
                <th>Código</th>
                <th>Elementos</th>
                <th>Encendidas</th>
                <th>Apagadas</th>
                <th>No activos</th>
                <th>Sedes</th>
              </tr>
            </thead>
            <tbody>
              {items.map((x) => (
                <tr key={x.code}>
                  <td>
                    <Link to={'/ssii/' + encodeURIComponent(x.code)}>{x.code}</Link>
                  </td>
                  <td>{x.cis}</td>
                  <td>{x.on || ''}</td>
                  <td className={x.off ? 'warn' : ''}>{x.off || ''}</td>
                  <td className={x.not_fresh ? 'warn' : ''}>{x.not_fresh || ''}</td>
                  <td>
                    <Sites sites={x.sites} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      )}
    </>
  )
}

export function SSIIDetail({ code }: { code: string }) {
  const st = useApi(() => api<{ code: string; cis: CIBrief[]; graph: Graph; by_class: Record<string, number> }>('/ssii/' + encodeURIComponent(code)), [code])
  const d = st.data
  return (
    <>
      <div className="title">
        <span className="cls big">Sistema de Información</span>
        <h1>{code}</h1>
      </div>
      <Loading state={st} />
      {d && (
        <>
          <p>
            {d.cis.length} elementos con este código · {d.graph.nodes.length} elementos relacionados{' '}
            {Object.entries(d.by_class)
              .sort((a, b) => b[1] - a[1])
              .map(([k, n]) => (
                <span key={k} className="chip">
                  {k}: {n}
                </span>
              ))}
          </p>
          <Card title="Dependencias">
            <GraphView graph={d.graph} rootIds={d.cis.map((c) => c.id)} height={560} />
            <p className="muted small">
              Columna central: elementos del sistema. A la derecha, de qué dependen (hasta 5 niveles); a la izquierda, el mismo
              equipo en otras tecnologías y lo que lo publica (hasta 3 niveles).
            </p>
          </Card>
          <Card title="Elementos relacionados por clase">
            {Object.keys(d.by_class)
              .sort()
              .map((cls) => (
                <details key={cls}>
                  <summary>
                    {cls} <span className="muted">({d.by_class[cls]})</span>
                  </summary>
                  <ul className="plain cols">
                    {d.graph.nodes
                      .filter((n) => n.class === cls)
                      .map((n) => (
                        <li key={n.id}>
                          <CILink ci={n} showClass={false} />
                        </li>
                      ))}
                  </ul>
                </details>
              ))}
          </Card>
        </>
      )}
    </>
  )
}
