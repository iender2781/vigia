import { useState } from 'react'
import { ago, api, fmtDate, type AttrChange, type CIBrief, type CIView, type EventItem, type Graph, type RelView } from '../api'
import { Card, CILink, LifecycleBadge, Link, Loading, Sites, useApi } from '../common'
import GraphView from '../GraphView'

interface Impact {
  total: number
  by_class: { key: string; n: number }[]
  ssii: { key: string; n: number }[]
  direct: CIBrief[]
  truncated: boolean
}

export default function CIDetail({ id }: { id: string }) {
  const ci = useApi(() => api<CIView>('/ci/' + id), [id])
  const rels = useApi(() => api<{ depends_on: RelView[]; dependents: RelView[] }>('/ci/' + id + '/relations'), [id])
  const impact = useApi(() => api<Impact>('/ci/' + id + '/impact'), [id])
  const [dir, setDir] = useState<'up' | 'down' | 'both'>('both')
  const [depth, setDepth] = useState(2)
  const graph = useApi(() => api<Graph>('/ci/' + id + '/graph', { direction: dir, depth }), [id, dir, depth])
  const hist = useApi(() => api<{ attributes: AttrChange[]; events: EventItem[] }>('/ci/' + id + '/history'), [id])
  const c = ci.data
  return (
    <>
      <Loading state={ci} />
      {c && (
        <>
          <div className="title">
            <span className="cls big">{c.class}</span>
            <h1>{c.name}</h1>
            <LifecycleBadge lc={c.lifecycle} />
          </div>
          <p className="muted small">
            {c.technology} · sede: <Sites sites={c.sites} /> · visto por última vez {ago(c.last_seen)} · descubierto el{' '}
            {fmtDate(c.first_seen)}
            {c.lifecycle !== 'active' && <> · en este estado desde {fmtDate(c.lifecycle_since)}</>}
          </p>
          <div className="grid2">
            <Card title="Atributos">
              <table className="kv">
                <tbody>
                  {c.ssii && (
                    <tr>
                      <th>Sistema de Información</th>
                      <td>
                        <Link to={'/ssii/' + encodeURIComponent(c.ssii)}>{c.ssii}</Link>
                      </td>
                    </tr>
                  )}
                  {Object.entries(c.attrs || {})
                    .sort()
                    .map(([k, v]) => (
                      <tr key={k}>
                        <th>{k}</th>
                        <td>
                          {v}
                          {c.manual_attrs && k in c.manual_attrs && <span className="chip">manual</span>}
                        </td>
                      </tr>
                    ))}
                  <tr>
                    <th>Métricas de origen</th>
                    <td className="muted small">{(c.sources || []).join(', ')}</td>
                  </tr>
                  <tr>
                    <th>Jobs</th>
                    <td className="muted small">{(c.jobs || []).join(', ') || '—'}</td>
                  </tr>
                  <tr>
                    <th>Clave natural</th>
                    <td className="muted small mono">{c.key}</td>
                  </tr>
                </tbody>
              </table>
            </Card>
            <Card title="Impacto si falla">
              <Loading state={impact} />
              {impact.data && (
                <>
                  {impact.data.total === 0 ? (
                    <p className="muted">Ningún otro elemento depende de este.</p>
                  ) : (
                    <>
                      <p>
                        <b>{impact.data.total.toLocaleString('es-ES')}</b> elementos dependen de él, directa o indirectamente
                        {impact.data.truncated && ' (recorrido truncado)'}.
                      </p>
                      <p>
                        {impact.data.by_class.map((x) => (
                          <span key={x.key} className="chip">
                            {x.key}: {x.n}
                          </span>
                        ))}
                      </p>
                    </>
                  )}
                  {impact.data.ssii.length > 0 && (
                    <>
                      <h3>Sistemas de Información afectados</h3>
                      <p>
                        {impact.data.ssii.map((x) => (
                          <Link key={x.key} to={'/ssii/' + encodeURIComponent(x.key)} className="chip strong">
                            {x.key} ({x.n})
                          </Link>
                        ))}
                      </p>
                    </>
                  )}
                </>
              )}
            </Card>
          </div>
          <Card
            title="Relaciones"
            actions={
              <span className="seg">
                {(['up', 'both', 'down'] as const).map((d) => (
                  <button key={d} className={dir === d ? 'on' : ''} onClick={() => setDir(d)}>
                    {d === 'up' ? 'De qué depende' : d === 'down' ? 'Qué depende de él' : 'Ambos'}
                  </button>
                ))}
                <select value={depth} onChange={(e) => setDepth(Number(e.target.value))}>
                  {[1, 2, 3, 4, 5].map((n) => (
                    <option key={n} value={n}>
                      {n} {n === 1 ? 'nivel' : 'niveles'}
                    </option>
                  ))}
                </select>
              </span>
            }
          >
            <Loading state={graph} />
            {graph.data && <GraphView graph={graph.data} rootIds={[id]} />}
            {graph.data?.truncated && <p className="muted small">El grafo se ha recortado: reduce los niveles para verlo completo.</p>}
            <p className="muted small">A la derecha, lo que necesita; a la izquierda, lo que depende de él. Clic en un elemento para abrir su ficha.</p>
          </Card>
          <div className="grid2">
            <RelList title="Depende de" items={rels.data?.depends_on} />
            <RelList title="Dependen de él directamente" items={rels.data?.dependents} />
          </div>
          <Card title="Historial">
            <Loading state={hist} />
            {hist.data && hist.data.attributes.length === 0 && hist.data.events.length === 0 && <p className="muted">Sin cambios registrados.</p>}
            {hist.data && hist.data.attributes.length > 0 && (
              <table className="tbl">
                <thead>
                  <tr>
                    <th>Fecha</th>
                    <th>Atributo</th>
                    <th>Antes</th>
                    <th>Después</th>
                  </tr>
                </thead>
                <tbody>
                  {hist.data.attributes.map((h, i) => (
                    <tr key={i}>
                      <td>{fmtDate(h.at)}</td>
                      <td>{h.attr === '_name' ? 'Nombre' : h.attr}</td>
                      <td className="muted">{h.old ?? '—'}</td>
                      <td>{h.new ?? '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {hist.data && hist.data.events.length > 0 && (
              <ul className="events">
                {hist.data.events.map((e) => (
                  <li key={e.id}>
                    <span className="muted">{fmtDate(e.ts)}</span> <b>{eventLabel(e)}</b>
                  </li>
                ))}
              </ul>
            )}
          </Card>
        </>
      )}
    </>
  )
}

function eventLabel(e: EventItem): string {
  switch (e.type) {
    case 'ci.created':
      return 'Descubierto'
    case 'ci.changed':
      return 'Cambian: ' + (e.data.attrs || []).join(', ')
    case 'ci.lifecycle':
      return 'Estado: ' + e.data.from + ' → ' + e.data.to
    case 'relation.added':
      return 'Nueva relación «' + e.data.rel + '»'
    case 'relation.removed':
      return 'Desaparece la relación «' + e.data.rel + '»'
  }
  return e.type
}

function RelList({ title, items }: { title: string; items?: RelView[] }) {
  const groups = new Map<string, RelView[]>()
  for (const r of items || []) {
    const k = r.rel + ' · ' + r.peer.class
    groups.set(k, [...(groups.get(k) || []), r])
  }
  return (
    <Card title={title + (items ? ' (' + items.length + ')' : '')}>
      {items && items.length === 0 && <p className="muted">Nada.</p>}
      {[...groups].map(([k, rs]) => (
        <details key={k} open={rs.length <= 8}>
          <summary>
            {k} <span className="muted">({rs.length})</span>
            {rs[0].informative && <span className="chip">informativa</span>}
          </summary>
          <ul className="plain">
            {rs.slice(0, 300).map((r) => (
              <li key={r.id}>
                <CILink ci={r.peer} showClass={false} />
                {r.method !== 'métricas' && <span className="muted small"> ({r.method})</span>}
                {r.attrs &&
                  Object.entries(r.attrs).map(([a, v]) => (
                    <span key={a} className="chip">
                      {a}: {v}
                    </span>
                  ))}
              </li>
            ))}
          </ul>
        </details>
      ))}
    </Card>
  )
}
