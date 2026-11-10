// Package discovery mantiene la CMDB al día: cada ciclo lee de Prometheus las series que
// declara la ontología, construye la instantánea y la reconcilia con PostgreSQL.
//
// Solo una réplica trabaja a la vez (bloqueo de asesoría de PostgreSQL); las demás esperan
// para tomar el relevo si la líder cae.
package discovery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"vigia/internal/contexto/config"
	"vigia/internal/contexto/engine"
	"vigia/internal/contexto/ontology"
	"vigia/internal/contexto/prom"
	"vigia/internal/contexto/store"
	"vigia/internal/platform/metrics"
)

var (
	mRuns     = metrics.Default.NewCounter("contexto_discovery_runs_total", "Ejecuciones del descubrimiento por resultado.", "status")
	mPhase    = metrics.Default.NewGauge("contexto_discovery_duration_seconds", "Duración de la última ejecución por fase.", "phase")
	mLastOK   = metrics.Default.NewGauge("contexto_discovery_last_success_timestamp_seconds", "Hora (Unix) de la última ejecución correcta o parcial.")
	mSeries   = metrics.Default.NewGauge("contexto_discovery_series", "Series leídas de Prometheus en la última ejecución.")
	mCIs      = metrics.Default.NewGauge("contexto_cmdb_snapshot_cis", "CI vistos en la última ejecución por tecnología.", "technology")
	mRels     = metrics.Default.NewGauge("contexto_cmdb_snapshot_relations", "Relaciones vistas en la última ejecución.")
	mChanges  = metrics.Default.NewCounter("contexto_discovery_changes_total", "Cambios aplicados a la CMDB por tipo.", "type")
	mLeader   = metrics.Default.NewGauge("contexto_discovery_leader", "1 si esta réplica es la líder del descubrimiento.")
	mPromErr  = metrics.Default.NewCounter("contexto_prometheus_errors_total", "Errores al consultar Prometheus o Alertmanager.", "api")
	mQuality  = metrics.Default.NewGauge("contexto_quality_issues", "Problemas de calidad de datos de la última ejecución.", "kind", "severity")
	mRules    = metrics.Default.NewGauge("contexto_ontology_rules", "Reglas cargadas en la ontología.")
	mMatchers = metrics.Default.NewGauge("contexto_discovery_matchers", "Selectores de series que se consultan en cada ciclo.")
)

// Discovery es el rol de descubrimiento.
type Discovery struct {
	Cfg      *config.Config
	Store    *store.Store
	Engine   *engine.Engine
	Prom     *prom.Client
	Log      *slog.Logger
	Instance string

	lastMaintenance time.Time
}

// New crea el rol.
func New(cfg *config.Config, st *store.Store, eng *engine.Engine, log *slog.Logger) *Discovery {
	host, _ := os.Hostname()
	mRules.Set(float64(len(eng.O.Rules)))
	return &Discovery{
		Cfg: cfg, Store: st, Engine: eng, Log: log, Instance: host,
		Prom: prom.New(prom.Options{
			URLs: cfg.PrometheusURLs, Timeout: cfg.PrometheusTimeout, Insecure: cfg.PrometheusInsecure,
			BearerToken: cfg.PrometheusBearer, BasicAuth: cfg.PrometheusBasic, Concurrency: cfg.PrometheusConcurrency,
		}),
	}
}

func promStr(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s)
}

// Matchers devuelve los selectores de series que necesita la ontología.
func Matchers(o *ontology.Ontology) []string {
	excl := ""
	if len(o.Settings.ExcludeJobs) > 0 {
		excl = `,job!~"` + promStr(strings.Join(o.Settings.ExcludeJobs, "|")) + `"`
	}
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, r := range o.Rules {
		switch {
		case r.Metric != "":
			add(`{__name__="` + promStr(r.Metric) + `"` + excl + `}`)
		case r.MetricRegex != "":
			add(`{__name__=~"` + promStr(r.MetricRegex) + `"` + excl + `}`)
		}
	}
	if b := o.Settings.DatastoreLunBridge; b != nil {
		add(`{__name__="` + promStr(b.DatastoreMetric) + `"` + excl + `}`)
		for _, s := range b.LunSources {
			add(`{__name__="` + promStr(s.Metric) + `"` + excl + `}`)
			if s.CodeMetric != "" {
				add(`{__name__="` + promStr(s.CodeMetric) + `"` + excl + `}`)
			}
		}
	}
	return out
}

// ValueMetrics devuelve las métricas (o expresiones) cuyo valor actual hace falta.
func ValueMetrics(o *ontology.Ontology) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, r := range o.Rules {
		for _, c := range r.CIs {
			if c.ValueAttr != "" {
				if r.Metric != "" {
					add(r.Metric)
				} else {
					add("(" + r.MetricRegex + ")")
				}
			}
		}
	}
	if b := o.Settings.DatastoreLunBridge; b != nil {
		add(b.DatastoreSizeMetric)
		for _, s := range b.LunSources {
			add(s.SizeMetric)
		}
	}
	return out
}

// Report es el resultado de una ejecución.
type Report struct {
	RunID      int64            `json:"run_id"`
	Status     string           `json:"status"`
	Series     int              `json:"series"`
	Values     int              `json:"values"`
	Alerts     int              `json:"alerts"`
	CIs        int              `json:"cis"`
	Relations  int              `json:"relations"`
	JobsDown   []string         `json:"jobs_down,omitempty"`
	Errors     []string         `json:"errors,omitempty"`
	Apply      store.ApplyStats `json:"apply"`
	Build      engine.Stats     `json:"build"`
	ReadSecs   float64          `json:"read_seconds"`
	BuildSecs  float64          `json:"build_seconds"`
	ApplySecs  float64          `json:"apply_seconds"`
	TotalSecs  float64          `json:"total_seconds"`
	Matchers   int              `json:"matchers"`
	Technology map[string]int   `json:"by_technology"`
}

// RunOnce ejecuta un ciclo completo.
func (d *Discovery) RunOnce(ctx context.Context) (rep Report, err error) {
	t0 := time.Now()
	rep.Technology = map[string]int{}
	runID, err := d.Store.StartRun(ctx, d.Instance)
	if err != nil {
		return rep, fmt.Errorf("registrando la ejecución: %w", err)
	}
	rep.RunID = runID
	finish := func(status string, runErr error, issues []store.Issue) {
		rep.Status = status
		rep.TotalSecs = time.Since(t0).Seconds()
		msg := ""
		if runErr != nil {
			msg = runErr.Error()
		}
		// Se usa un contexto propio: aunque el ciclo haya agotado su tiempo, la ejecución se cierra.
		fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if ferr := d.Store.FinishRun(fctx, runID, status, rep, msg, issues); ferr != nil {
			d.Log.Error("no se pudo cerrar la ejecución", "run", runID, "err", ferr)
		}
		mRuns.Inc(status)
		mPhase.Set(rep.ReadSecs, "read")
		mPhase.Set(rep.BuildSecs, "build")
		mPhase.Set(rep.ApplySecs, "apply")
		mPhase.Set(rep.TotalSecs, "total")
		if status != "failed" {
			mLastOK.Set(float64(time.Now().Unix()))
		}
		mQuality.Reset()
		for _, is := range issues {
			mQuality.Set(float64(is.Count), is.Kind, is.Severity)
		}
	}

	o := d.Engine.O
	matchers := Matchers(o)
	rep.Matchers = len(matchers)
	mMatchers.Set(float64(len(matchers)))
	sr := d.Prom.Series(ctx, matchers, d.Cfg.SeriesLookback, d.Cfg.MatchersPerRequest)
	for _, e := range sr.Errors {
		rep.Errors = append(rep.Errors, e.Error())
		mPromErr.Inc("series")
	}
	if len(sr.Series) == 0 {
		err := errors.New("Prometheus no ha devuelto ninguna serie")
		if len(sr.Errors) > 0 {
			err = fmt.Errorf("no se pudo leer Prometheus: %w", errors.Join(sr.Errors...))
		}
		finish("failed", err, []store.Issue{{Kind: "prometheus_error", Severity: "error", Subject: "series", Count: 1,
			Detail: map[string]any{"errors": rep.Errors}}})
		return rep, err
	}
	in := engine.Input{Series: sr.Series}
	rep.Series = len(sr.Series)
	mSeries.Set(float64(len(sr.Series)))

	values, verrs := d.Prom.Values(ctx, ValueMetrics(o), o.Settings.ExcludeJobs)
	for _, e := range verrs {
		rep.Errors = append(rep.Errors, e.Error())
		mPromErr.Inc("query")
	}
	in.Values = values
	rep.Values = len(values)

	th, terrs := d.Prom.Targets(ctx)
	for _, e := range terrs {
		rep.Errors = append(rep.Errors, e.Error())
		mPromErr.Inc("targets")
	}
	jobsDown := map[string]bool{}
	for j, n := range th.Down {
		if n > 0 {
			jobsDown[j] = true
			rep.JobsDown = append(rep.JobsDown, j)
		}
	}
	sort.Strings(rep.JobsDown)

	if d.Cfg.AlertmanagerURL != "" {
		alerts, aerr := prom.Alerts(ctx, d.Prom, d.Cfg.AlertmanagerURL)
		if aerr != nil {
			rep.Errors = append(rep.Errors, aerr.Error())
			mPromErr.Inc("alertmanager")
		}
		excl := map[string]bool{}
		for _, j := range o.Settings.ExcludeJobs {
			excl[j] = true
		}
		for _, a := range alerts {
			if !excl[a.Labels["job"]] {
				in.Alerts = append(in.Alerts, a.Labels)
			}
		}
		rep.Alerts = len(in.Alerts)
	}
	rep.ReadSecs = time.Since(t0).Seconds()

	t1 := time.Now()
	res := d.Engine.Build(in)
	rep.BuildSecs = time.Since(t1).Seconds()
	rep.CIs, rep.Relations, rep.Build = len(res.CIs), len(res.Rels), res.Stats
	mCIs.Reset()
	for _, c := range res.CIs {
		rep.Technology[c.Tech]++
	}
	for t, n := range rep.Technology {
		mCIs.Set(float64(n), t)
	}
	mRels.Set(float64(len(res.Rels)))

	partial := len(rep.Errors) > 0
	t2 := time.Now()
	ap, err := d.Store.ApplySnapshot(ctx, res, store.ApplyOptions{
		Partial: partial, JobsDown: jobsDown, StaleAfter: d.Cfg.StaleAfter(), RetireAfter: d.Cfg.RetireAfter,
		SSIIAttr: o.Settings.SSIIAttr,
	})
	rep.ApplySecs = time.Since(t2).Seconds()
	rep.Apply = ap
	if err != nil {
		finish("failed", fmt.Errorf("guardando la CMDB: %w", err), nil)
		return rep, err
	}
	for k, v := range map[string]int{"created": ap.Created, "changed": ap.Changed, "revived": ap.Revived,
		"relation_added": ap.RelAdded, "relation_removed": ap.RelRemoved, "relation_updated": ap.RelUpdated} {
		mChanges.Add(float64(v), k)
	}
	for lc, n := range ap.Lifecycle {
		mChanges.Add(float64(n), lc)
	}
	status := "ok"
	if partial {
		status = "partial"
	}
	finish(status, nil, Quality(o, res, rep))
	return rep, nil
}

// Quality calcula el informe de calidad de datos de una ejecución.
func Quality(o *ontology.Ontology, res *engine.Result, rep Report) []store.Issue {
	var out []store.Issue
	for i, e := range rep.Errors {
		if i >= 10 {
			break
		}
		out = append(out, store.Issue{Kind: "prometheus_error", Severity: "error", Subject: truncate(e, 200), Count: 1})
	}
	for _, j := range rep.JobsDown {
		out = append(out, store.Issue{Kind: "targets_down", Severity: "warning", Subject: j, Count: 1,
			Detail: map[string]any{"efecto": "los CI de este job que no aparezcan pasan a 'sin datos'"}})
	}
	for _, r := range o.Rules {
		if r.Source == "" && res.Stats.RuleRows[r.ID] == 0 {
			out = append(out, store.Issue{Kind: "rule_without_data", Severity: "info", Subject: r.ID, Count: 1,
				Detail: map[string]any{"pack": r.Pack, "metric": r.Metric + r.MetricRegex}})
		}
	}
	for _, m := range res.Stats.ManualErrors {
		out = append(out, store.Issue{Kind: "manual_relation", Severity: "warning", Subject: truncate(m, 200), Count: 1})
	}
	for k, n := range res.Stats.Bridge {
		sev := "warning"
		if strings.HasPrefix(k, "enlazados por") {
			sev = "info"
		}
		out = append(out, store.Issue{Kind: "datastore_lun_bridge", Severity: sev, Subject: k, Count: n})
	}
	// CI sin código de Sistema de Información, en las clases donde otros CI sí lo tienen.
	if attr := o.Settings.SSIIAttr; attr != "" {
		with, without := map[string]int{}, map[string][]string{}
		for _, c := range res.CIs {
			if c.Attrs[attr] != "" {
				with[c.Class]++
			} else {
				without[c.Class] = append(without[c.Class], c.Name)
			}
		}
		for class, names := range without {
			if with[class] == 0 {
				continue
			}
			sort.Strings(names)
			sample := names
			if len(sample) > 50 {
				sample = sample[:50]
			}
			out = append(out, store.Issue{Kind: "missing_ssii", Severity: "warning", Subject: class, Count: len(names),
				Detail: map[string]any{"attr": attr, "with": with[class], "sample": sample}})
		}
	}
	// CI aislados (sin ninguna relación), por clase.
	linked := map[string]bool{}
	for _, r := range res.Rels {
		linked[r.From], linked[r.To] = true, true
	}
	orphans := map[string]int{}
	for k, c := range res.CIs {
		if !linked[k] {
			orphans[c.Class]++
		}
	}
	for class, n := range orphans {
		out = append(out, store.Issue{Kind: "orphan_ci", Severity: "info", Subject: class, Count: n})
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// Loop ejecuta el descubrimiento mientras esta réplica sea la líder.
func (d *Discovery) Loop(ctx context.Context) error {
	h := fnv.New32a()
	h.Write([]byte("contexto-discovery-" + d.Store.TenantID))
	lockKey := int64(h.Sum32())
	var conn *sql.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
		mLeader.Set(0)
	}()
	for {
		if conn == nil {
			c, err := d.Store.DB.Conn(ctx)
			if err == nil {
				var ok bool
				if err = c.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&ok); err == nil && ok {
					conn = c
					mLeader.Set(1)
					d.Log.Info("esta réplica es la líder del descubrimiento")
				} else {
					c.Close()
				}
			}
			if conn == nil {
				mLeader.Set(0)
				if err != nil && ctx.Err() == nil {
					d.Log.Warn("no se pudo intentar el liderazgo", "err", err)
				}
				if !sleep(ctx, 15*time.Second) {
					return nil
				}
				continue
			}
		}
		// Comprueba que la conexión (y con ella el bloqueo) sigue viva.
		if err := conn.PingContext(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			d.Log.Warn("perdido el liderazgo del descubrimiento", "err", err)
			conn.Close()
			conn = nil
			mLeader.Set(0)
			continue
		}
		d.maintain(ctx)
		cctx, cancel := context.WithTimeout(ctx, d.Cfg.DiscoveryInterval*2)
		rep, err := d.RunOnce(cctx)
		cancel()
		if err != nil {
			d.Log.Error("descubrimiento fallido", "run", rep.RunID, "err", err)
		} else {
			d.Log.Info("descubrimiento terminado", "run", rep.RunID, "status", rep.Status, "series", rep.Series,
				"cis", rep.CIs, "relations", rep.Relations, "created", rep.Apply.Created, "changed", rep.Apply.Changed,
				"relations_added", rep.Apply.RelAdded, "relations_removed", rep.Apply.RelRemoved,
				"lifecycle", rep.Apply.Lifecycle, "seconds", fmt.Sprintf("%.1f", rep.TotalSecs))
		}
		if !sleep(ctx, d.Cfg.DiscoveryInterval) {
			return nil
		}
	}
}

func (d *Discovery) maintain(ctx context.Context) {
	if time.Since(d.lastMaintenance) < 24*time.Hour {
		return
	}
	created, dropped, err := d.Store.MaintainPartitions(ctx, d.Cfg.EventRetentionMonths)
	if err != nil {
		d.Log.Error("mantenimiento de particiones", "err", err)
		return
	}
	d.lastMaintenance = time.Now()
	d.Log.Info("particiones de eventos revisadas", "creadas", created, "borradas", dropped)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
