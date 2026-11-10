package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"

	"vigia/internal/contexto/engine"
)

// ApplyOptions controla cómo se reconcilia una instantánea con la base de datos.
type ApplyOptions struct {
	Now time.Time
	// Partial: la lectura de Prometheus tuvo errores. No se degrada el ciclo de vida ni se
	// cierran relaciones, para no perder elementos por un fallo transitorio.
	Partial bool
	// JobsDown: jobs con targets caídos. Un CI que no aparece y cuyo job está caído pasa a
	// "nodata" (sin datos), no a "stale".
	JobsDown map[string]bool
	// StaleAfter: tiempo sin ver un CI (con su exporter sano) para marcarlo obsoleto.
	StaleAfter time.Duration
	// RetireAfter: tiempo sin ver un CI para retirarlo.
	RetireAfter time.Duration
	// SSIIAttr: atributo que identifica el Sistema de Información.
	SSIIAttr string
}

// ApplyStats resume los cambios aplicados.
type ApplyStats struct {
	InitialLoad    bool           `json:"initial_load"`
	CIs            int            `json:"cis"`
	Created        int            `json:"created"`
	Changed        int            `json:"changed"`
	Revived        int            `json:"revived"`
	Lifecycle      map[string]int `json:"lifecycle_changes"`
	Relations      int            `json:"relations"`
	RelAdded       int            `json:"relations_added"`
	RelRemoved     int            `json:"relations_removed"`
	RelUpdated     int            `json:"relations_updated"`
	AttrHistory    int            `json:"attr_history"`
	DurationMillis int64          `json:"duration_ms"`
}

func origin(method string) string {
	if strings.HasPrefix(method, "fichero ") {
		return "manual"
	}
	if method == "nombre normalizado" || method == "nombre del datastore" {
		return "inferred"
	}
	return "discovered"
}

func downList(m map[string]bool) []string {
	out := []string{}
	for j, d := range m {
		if d {
			out = append(out, j)
		}
	}
	sort.Strings(out)
	return out
}

// nz evita que una lista vacía se guarde como NULL.
func nz(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ApplySnapshot reconcilia una instantánea de la CMDB con lo guardado.
func (s *Store) ApplySnapshot(ctx context.Context, snap *engine.Result, o ApplyOptions) (ApplyStats, error) {
	t0 := time.Now()
	st := ApplyStats{Lifecycle: map[string]int{}, CIs: len(snap.CIs), Relations: len(snap.Rels)}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	now := o.Now.UTC()
	err := s.Tx(ctx, false, func(tx *sql.Tx) error {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM ci`).Scan(&existing); err != nil {
			return err
		}
		st.InitialLoad = existing == 0

		// --- CI ---
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE snap_ci (
			id text PRIMARY KEY, natural_key text, class text, technology text, name text,
			attrs jsonb, sources text[], jobs text[], sites text[], ssii text, h text NOT NULL DEFAULT '') ON COMMIT DROP`); err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, pq.CopyIn("snap_ci", "id", "natural_key", "class", "technology",
			"name", "attrs", "sources", "jobs", "sites", "ssii"))
		if err != nil {
			return err
		}
		for _, key := range snap.Order {
			c := snap.CIs[key]
			attrs, _ := json.Marshal(c.Attrs)
			if _, err := stmt.ExecContext(ctx, c.ID, c.Key, c.Class, c.Tech, c.Name, string(attrs),
				pq.Array(nz(c.Sources)), pq.Array(nz(c.Jobs)), pq.Array(nz(c.Sites)), nullable(c.Attrs[o.SSIIAttr])); err != nil {
				stmt.Close()
				return err
			}
		}
		if _, err := stmt.ExecContext(ctx); err != nil {
			stmt.Close()
			return err
		}
		stmt.Close()
		// Mientras un job está caído se conservan el nombre, los atributos y los jobs que aportaba: el CI puede
		// seguir viéndose por otra fuente (otro exporter, sus alertas) sin perder información.
		if _, err := tx.ExecContext(ctx, `
			UPDATE snap_ci s SET attrs = c.attrs || s.attrs, name = c.name,
			       jobs = ARRAY(SELECT DISTINCT j FROM unnest(c.jobs || s.jobs) j ORDER BY j)
			FROM ci c WHERE c.tenant_id = $1 AND c.id = s.id AND c.jobs && $2::text[]`,
			s.TenantID, pq.Array(downList(o.JobsDown))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE snap_ci SET h = md5(class || '|' || name || '|' ||
			array_to_string(sites, ',') || '|' || attrs::text)`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ANALYZE snap_ci`); err != nil {
			return err
		}

		// Historial de atributos y de nombre de los CI que han cambiado.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO ci_attr_history (tenant_id, ci_id, changed_at, attr, old_value, new_value)
			SELECT $1::uuid, s.id, $2::timestamptz, k.key, c.attrs->>k.key, s.attrs->>k.key
			FROM snap_ci s
			JOIN ci c ON c.tenant_id = $1 AND c.id = s.id
			CROSS JOIN LATERAL (SELECT jsonb_object_keys(c.attrs) UNION SELECT jsonb_object_keys(s.attrs)) AS k(key)
			WHERE c.content_hash <> s.h AND (c.attrs->>k.key) IS DISTINCT FROM (s.attrs->>k.key)
			UNION ALL
			SELECT $1::uuid, s.id, $2::timestamptz, '_name', c.name, s.name
			FROM snap_ci s JOIN ci c ON c.tenant_id = $1 AND c.id = s.id
			WHERE c.name <> s.name`, s.TenantID, now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		st.AttrHistory = int(n)

		res, err = tx.ExecContext(ctx, `
			INSERT INTO event (tenant_id, ts, type, ci_id, data)
			SELECT $1::uuid, $2::timestamptz, 'ci.changed', s.id, jsonb_build_object('class', s.class, 'name', s.name,
			       'attrs', (SELECT coalesce(jsonb_agg(k.key), '[]'::jsonb) FROM
			                 (SELECT jsonb_object_keys(c.attrs) UNION SELECT jsonb_object_keys(s.attrs)) AS k(key)
			                 WHERE (c.attrs->>k.key) IS DISTINCT FROM (s.attrs->>k.key)))
			FROM snap_ci s JOIN ci c ON c.tenant_id = $1 AND c.id = s.id
			WHERE c.content_hash <> s.h`, s.TenantID, now)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		st.Changed = int(n)

		res, err = tx.ExecContext(ctx, `
			INSERT INTO event (tenant_id, ts, type, ci_id, data)
			SELECT $1::uuid, $2::timestamptz, 'ci.lifecycle', s.id, jsonb_build_object('class', s.class, 'name', s.name, 'from', c.lifecycle, 'to', 'active')
			FROM snap_ci s JOIN ci c ON c.tenant_id = $1 AND c.id = s.id
			WHERE c.lifecycle <> 'active'`, s.TenantID, now)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		st.Revived = int(n)

		if _, err := tx.ExecContext(ctx, `
			UPDATE ci SET name = s.name, class = s.class, technology = s.technology, attrs = s.attrs,
			       sources = s.sources, jobs = s.jobs, sites = s.sites, ssii = s.ssii, content_hash = s.h,
			       last_seen = $2,
			       lifecycle_since = CASE WHEN ci.lifecycle <> 'active' THEN $2 ELSE ci.lifecycle_since END,
			       lifecycle = 'active'
			FROM snap_ci s
			WHERE ci.tenant_id = $1 AND ci.id = s.id`, s.TenantID, now); err != nil {
			return err
		}

		if !st.InitialLoad {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event (tenant_id, ts, type, ci_id, data)
				SELECT $1::uuid, $2::timestamptz, 'ci.created', s.id, jsonb_build_object('class', s.class, 'name', s.name)
				FROM snap_ci s WHERE NOT EXISTS (SELECT 1 FROM ci c WHERE c.tenant_id = $1 AND c.id = s.id)`,
				s.TenantID, now); err != nil {
				return err
			}
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO ci (tenant_id, id, natural_key, class, technology, name, attrs, sources, jobs, sites,
			                ssii, lifecycle, lifecycle_since, first_seen, last_seen, content_hash)
			SELECT $1, s.id, s.natural_key, s.class, s.technology, s.name, s.attrs, s.sources, s.jobs, s.sites,
			       s.ssii, 'active', $2::timestamptz, $2::timestamptz, $2::timestamptz, s.h
			FROM snap_ci s WHERE NOT EXISTS (SELECT 1 FROM ci c WHERE c.tenant_id = $1 AND c.id = s.id)`,
			s.TenantID, now)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		st.Created = int(n)

		// --- Ciclo de vida de los CI que no aparecen ---
		if !o.Partial {
			if err := s.demoteMissing(ctx, tx, now, o, &st); err != nil {
				return err
			}
		}

		// --- Relaciones ---
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE snap_rel (
			src text, rel text, dst text, method text, origin text, attrs jsonb,
			PRIMARY KEY (src, rel, dst)) ON COMMIT DROP`); err != nil {
			return err
		}
		stmt, err = tx.PrepareContext(ctx, pq.CopyIn("snap_rel", "src", "rel", "dst", "method", "origin", "attrs"))
		if err != nil {
			return err
		}
		for _, r := range snap.Rels {
			a, b := snap.CIs[r.From], snap.CIs[r.To]
			attrs := []byte("{}")
			if len(r.Attrs) > 0 {
				attrs, _ = json.Marshal(r.Attrs)
			}
			if _, err := stmt.ExecContext(ctx, a.ID, r.Rel, b.ID, r.Method, origin(r.Method), string(attrs)); err != nil {
				stmt.Close()
				return err
			}
		}
		if _, err := stmt.ExecContext(ctx); err != nil {
			stmt.Close()
			return err
		}
		stmt.Close()
		if _, err := tx.ExecContext(ctx, `ANALYZE snap_rel`); err != nil {
			return err
		}

		res, err = tx.ExecContext(ctx, `
			UPDATE relation r SET attrs = s.attrs, method = s.method, origin = s.origin
			FROM snap_rel s
			WHERE r.tenant_id = $1 AND r.valid_to IS NULL AND r.src = s.src AND r.rel = s.rel AND r.dst = s.dst
			  AND (r.attrs <> s.attrs OR r.method <> s.method)`, s.TenantID)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		st.RelUpdated = int(n)

		if !st.InitialLoad {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO event (tenant_id, ts, type, ci_id, data)
				SELECT $1::uuid, $2::timestamptz, 'relation.added', s.src, jsonb_build_object('rel', s.rel, 'dst', s.dst, 'method', s.method)
				FROM snap_rel s WHERE NOT EXISTS (SELECT 1 FROM relation r WHERE r.tenant_id = $1
				      AND r.valid_to IS NULL AND r.src = s.src AND r.rel = s.rel AND r.dst = s.dst)`,
				s.TenantID, now); err != nil {
				return err
			}
		}
		res, err = tx.ExecContext(ctx, `
			INSERT INTO relation (tenant_id, src, rel, dst, method, origin, attrs, valid_from)
			SELECT $1::uuid, s.src, s.rel, s.dst, s.method, s.origin, s.attrs, $2::timestamptz
			FROM snap_rel s WHERE NOT EXISTS (SELECT 1 FROM relation r WHERE r.tenant_id = $1
			      AND r.valid_to IS NULL AND r.src = s.src AND r.rel = s.rel AND r.dst = s.dst)`,
			s.TenantID, now)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		st.RelAdded = int(n)

		if !o.Partial {
			// Se cierran las relaciones que ya no aparecen, salvo si alguno de sus extremos está
			// sin datos, obsoleto o tiene un job caído (no sabemos si la relación sigue existiendo).
			res, err = tx.ExecContext(ctx, `
				WITH closed AS (
					UPDATE relation r SET valid_to = $2
					WHERE r.tenant_id = $1 AND r.valid_to IS NULL
					  AND NOT EXISTS (SELECT 1 FROM snap_rel s WHERE s.src = r.src AND s.rel = r.rel AND s.dst = r.dst)
					  AND NOT EXISTS (SELECT 1 FROM ci c WHERE c.tenant_id = $1 AND c.id IN (r.src, r.dst)
					                  AND (c.lifecycle IN ('nodata', 'stale') OR c.jobs && $3::text[]))
					RETURNING r.src, r.rel, r.dst)
				INSERT INTO event (tenant_id, ts, type, ci_id, data)
				SELECT $1::uuid, $2::timestamptz, 'relation.removed', src, jsonb_build_object('rel', rel, 'dst', dst) FROM closed`,
				s.TenantID, now, pq.Array(downList(o.JobsDown)))
			if err != nil {
				return err
			}
			n, _ = res.RowsAffected()
			st.RelRemoved = int(n)
		}

		if st.InitialLoad {
			data, _ := json.Marshal(map[string]int{"cis": st.Created, "relations": st.RelAdded})
			if _, err := tx.ExecContext(ctx, `INSERT INTO event (tenant_id, ts, type, data) VALUES ($1, $2, 'cmdb.initial_load', $3)`,
				s.TenantID, now, string(data)); err != nil {
				return err
			}
		}
		return nil
	})
	st.DurationMillis = time.Since(t0).Milliseconds()
	return st, err
}

func (s *Store) demoteMissing(ctx context.Context, tx *sql.Tx, now time.Time, o ApplyOptions, st *ApplyStats) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.class, c.name, c.jobs, c.lifecycle, c.last_seen FROM ci c
		WHERE c.tenant_id = $1 AND c.lifecycle <> 'retired'
		  AND NOT EXISTS (SELECT 1 FROM snap_ci s WHERE s.id = c.id)`, s.TenantID)
	if err != nil {
		return err
	}
	type change struct{ id, class, name, from, to string }
	var changes []change
	for rows.Next() {
		var id, class, name, lc string
		var jobs []string
		var last time.Time
		if err := rows.Scan(&id, &class, &name, pq.Array(&jobs), &lc, &last); err != nil {
			rows.Close()
			return err
		}
		down := false
		for _, j := range jobs {
			if o.JobsDown[j] {
				down = true
				break
			}
		}
		age := now.Sub(last)
		next := lc
		switch {
		case down:
			next = "nodata"
		case lc == "stale" && o.RetireAfter > 0 && age >= o.RetireAfter:
			next = "retired"
		case (lc == "active" || lc == "nodata") && age >= o.StaleAfter:
			next = "stale"
			if o.RetireAfter > 0 && age >= o.RetireAfter {
				next = "retired"
			}
		}
		if next != lc {
			changes = append(changes, change{id, class, name, lc, next})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	byTo := map[string][]string{}
	for _, c := range changes {
		byTo[c.to] = append(byTo[c.to], c.id)
		st.Lifecycle[c.to]++
	}
	for to, ids := range byTo {
		if _, err := tx.ExecContext(ctx, `UPDATE ci SET lifecycle = $2, lifecycle_since = $3
			WHERE tenant_id = $1 AND id = ANY($4)`, s.TenantID, to, now, pq.Array(ids)); err != nil {
			return err
		}
	}
	if len(changes) > 0 {
		ids, classes, names, froms, tos := make([]string, len(changes)), make([]string, len(changes)),
			make([]string, len(changes)), make([]string, len(changes)), make([]string, len(changes))
		for i, c := range changes {
			ids[i], classes[i], names[i], froms[i], tos[i] = c.id, c.class, c.name, c.from, c.to
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO event (tenant_id, ts, type, ci_id, data)
			SELECT $1::uuid, $2::timestamptz, 'ci.lifecycle', u.id, jsonb_build_object('class', u.class, 'name', u.name, 'from', u.f, 'to', u.t)
			FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[]) AS u(id, class, name, f, t)`,
			s.TenantID, now, pq.Array(ids), pq.Array(classes), pq.Array(names), pq.Array(froms), pq.Array(tos)); err != nil {
			return err
		}
	}
	// Las relaciones de los CI retirados se cierran.
	if ids := byTo["retired"]; len(ids) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE relation SET valid_to = $2
			WHERE tenant_id = $1 AND valid_to IS NULL AND (src = ANY($3) OR dst = ANY($3))`,
			s.TenantID, now, pq.Array(ids)); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ejecuciones e informe de calidad
// ---------------------------------------------------------------------------

// Issue es un problema de calidad de datos detectado en una ejecución.
type Issue struct {
	Kind     string         `json:"kind"`
	Severity string         `json:"severity"`
	Subject  string         `json:"subject"`
	Count    int            `json:"count"`
	Detail   map[string]any `json:"detail,omitempty"`
}

// StartRun registra el inicio de una ejecución del descubrimiento.
func (s *Store) StartRun(ctx context.Context, instance string) (int64, error) {
	var id int64
	err := s.Tx(ctx, false, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `INSERT INTO discovery_run (tenant_id, started_at, status, instance)
			VALUES ($1, now(), 'running', $2) RETURNING id`, s.TenantID, instance).Scan(&id)
	})
	return id, err
}

// FinishRun cierra la ejecución con su estado, estadísticas y problemas de calidad.
func (s *Store) FinishRun(ctx context.Context, id int64, status string, stats any, runErr string, issues []Issue) error {
	b, _ := json.Marshal(stats)
	return s.Tx(ctx, false, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE discovery_run SET finished_at = now(), status = $2, stats = $3, error = $4
			WHERE id = $1`, id, status, string(b), nullable(runErr)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event (tenant_id, type, data) VALUES ($1, 'discovery.run', $2)`,
			s.TenantID, `{"run_id":`+jsonInt(id)+`,"status":"`+status+`"}`); err != nil {
			return err
		}
		for _, is := range issues {
			d, _ := json.Marshal(is.Detail)
			if is.Detail == nil {
				d = []byte("{}")
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO quality_issue (tenant_id, run_id, kind, severity, subject, count, detail)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, s.TenantID, id, is.Kind, is.Severity, is.Subject, is.Count, string(d)); err != nil {
				return err
			}
		}
		// Se conservan las últimas 2000 ejecuciones (una semana con ciclos de 5 min).
		_, err := tx.ExecContext(ctx, `DELETE FROM discovery_run WHERE tenant_id = $1 AND id < $2 - 2000`, s.TenantID, id)
		return err
	})
}

func jsonInt(i int64) string {
	b, _ := json.Marshal(i)
	return string(b)
}
