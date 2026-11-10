import { ago, api, lifecycleLabel, type Stats } from '../api'
import { Bars, Card, Link, Loading, useApi } from '../common'

export default function Overview() {
  const st = useApi(() => api<Stats>('/stats'), [])
  const d = st.data
  return (
    <>
      <h1>Resumen</h1>
      <Loading state={st} />
      {d && (
        <>
          <div className="kpis">
            <div className="kpi">
              <span>Elementos (CI)</span>
              <b>{d.cis.toLocaleString('es-ES')}</b>
            </div>
            <div className="kpi">
              <span>Relaciones vigentes</span>
              <b>{d.relations.toLocaleString('es-ES')}</b>
            </div>
            <div className="kpi">
              <span>Sistemas de Información</span>
              <b>
                <Link to="/ssii">{d.ssii}</Link>
              </b>
            </div>
            <div className="kpi">
              <span>Último descubrimiento</span>
              <b className={'st-' + (d.last_run?.status || 'none')}>
                {d.last_run ? (
                  <Link to="/descubrimiento">
                    {d.last_run.status === 'ok' ? 'correcto' : d.last_run.status} · {ago(d.last_run.finished_at || d.last_run.started_at)}
                  </Link>
                ) : (
                  'nunca'
                )}
              </b>
            </div>
          </div>
          <div className="grid2">
            <Card title="Por tecnología">
              <Bars items={d.by_technology || []} linkFor={(k) => '/ci?technology=' + encodeURIComponent(k)} />
            </Card>
            <Card title="Por sede">
              <Bars items={d.by_site || []} linkFor={(k) => (k.startsWith('(') ? '/ci' : '/ci?site=' + encodeURIComponent(k))} />
            </Card>
            <Card title="Ciclo de vida">
              <Bars
                items={(d.by_lifecycle || []).map((c) => ({ key: lifecycleLabel[c.key] || c.key, n: c.n }))}
              />
              <p className="muted small">
                <b>Sin datos</b>: su exporter está caído. <b>Obsoleto</b>: no se ve desde hace varios ciclos con el exporter
                sano. <b>Retirado</b>: obsoleto durante más del plazo configurado.
              </p>
            </Card>
            <Card title="Por clase">
              <Bars items={(d.by_class || []).slice(0, 20)} linkFor={(k) => '/ci?class=' + encodeURIComponent(k)} />
            </Card>
          </div>
        </>
      )}
    </>
  )
}
