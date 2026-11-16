import { ago, api, fmtDate, type Issue, type Run } from '../api'
import { Card, Loading, useApi } from '../common'

const kindLabel: Record<string, string> = {
  prometheus_error: 'Error al leer Prometheus',
  targets_down: 'Job con targets caídos',
  rule_without_data: 'Regla sin datos',
  manual_relation: 'Relación manual no aplicada',
  datastore_lun_bridge: 'Datastore → LUN',
  missing_ssii: 'Sin Sistema de Información',
  orphan_ci: 'Elementos aislados',
}

export function Quality() {
  const st = useApi(() => api<{ run?: Run; issues: Issue[] }>('/quality'), [])
  const groups = new Map<string, Issue[]>()
  for (const i of st.data?.issues || []) groups.set(i.kind, [...(groups.get(i.kind) || []), i])
  return (
    <>
      <h1>Calidad de datos</h1>
      <p className="muted">
        Lo que la ontología no consigue modelar en el último descubrimiento
        {st.data?.run && <> ({fmtDate(st.data.run.finished_at)})</>}. Es la lista de trabajo para mejorar exporters y ontología.
      </p>
      <Loading state={st} />
      {st.data && st.data.issues.length === 0 && <p>Sin problemas.</p>}
      {[...groups].map(([kind, is]) => (
        <Card key={kind} title={(kindLabel[kind] || kind) + ' (' + is.length + ')'}>
          <table className="tbl">
            <tbody>
              {is.map((i, n) => (
                <tr key={n}>
                  <td>
                    <span className={'sev sev-' + i.severity}>{i.severity}</span>
                  </td>
                  <td>{i.subject}</td>
                  <td className="num">{i.count}</td>
                  <td className="muted small">
                    {i.detail && Object.keys(i.detail).length > 0 && (
                      <details>
                        <summary>detalle</summary>
                        <pre>{JSON.stringify(i.detail, null, 1)}</pre>
                      </details>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </Card>
      ))}
    </>
  )
}

export function Runs() {
  const st = useApi(() => api<{ items: Run[] }>('/discovery/runs', { limit: 50 }), [])
  return (
    <>
      <h1>Descubrimiento</h1>
      <Loading state={st} />
      {st.data && (
        <Card title="Últimas ejecuciones">
          <table className="tbl">
            <thead>
              <tr>
                <th>#</th>
                <th>Inicio</th>
                <th>Estado</th>
                <th>Series</th>
                <th>CI</th>
                <th>Relaciones</th>
                <th>Cambios</th>
                <th>Duración</th>
                <th>Réplica</th>
              </tr>
            </thead>
            <tbody>
              {st.data.items.map((r) => {
                const a = r.stats?.apply || {}
                const lc = Object.entries(a.lifecycle_changes || {})
                  .map(([k, v]) => k + ' ' + v)
                  .join(', ')
                return (
                  <tr key={r.id}>
                    <td>{r.id}</td>
                    <td title={fmtDate(r.started_at)}>{ago(r.started_at)}</td>
                    <td>
                      <span className={'st-' + r.status}>{r.status}</span>
                      {r.error && <div className="error small">{r.error}</div>}
                    </td>
                    <td className="num">{r.stats?.series ?? ''}</td>
                    <td className="num">{r.stats?.cis ?? ''}</td>
                    <td className="num">{r.stats?.relations ?? ''}</td>
                    <td className="small">
                      {a.initial_load
                        ? 'carga inicial'
                        : [a.created && '+' + a.created, a.changed && '~' + a.changed, a.relations_added && '+' + a.relations_added + ' rel', a.relations_removed && '−' + a.relations_removed + ' rel', lc]
                            .filter(Boolean)
                            .join(' · ') || 'sin cambios'}
                    </td>
                    <td className="num">{r.stats?.total_seconds ? r.stats.total_seconds.toFixed(1) + ' s' : ''}</td>
                    <td className="muted small">{r.instance}</td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        </Card>
      )}
    </>
  )
}
