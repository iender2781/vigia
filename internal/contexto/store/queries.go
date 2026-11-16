package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// CIView es un CI tal como lo devuelve la API.
type CIView struct {
	ID             string            `json:"id"`
	Key            string            `json:"key"`
	Class          string            `json:"class"`
	Technology     string            `json:"technology"`
	Name           string            `json:"name"`
	Attrs          map[string]string `json:"attrs"`
	ManualAttrs    map[string]string `json:"manual_attrs,omitempty"`
	Sources        []string          `json:"sources"`
	Jobs           []string          `json:"jobs"`
	Sites          []string          `json:"sites"`
	SSII           string            `json:"ssii,omitempty"`
	Lifecycle      string            `json:"lifecycle"`
	LifecycleSince time.Time         `json:"lifecycle_since"`
	FirstSeen      time.Time         `json:"first_seen"`
	LastSeen       time.Time         `json:"last_seen"`
}

// CIBrief es la versión resumida de un CI (para listas y grafos).
type CIBrief struct {
	ID         string   `json:"id"`
	Class      string   `json:"class"`
	Technology string   `json:"technology"`
	Name       string   `json:"name"`
	Sites      []string `json:"sites"`
	SSII       string   `json:"ssii,omitempty"`
	Lifecycle  string   `json:"lifecycle"`
}

const ciCols = `id, natural_key, class, technology, name, attrs, manual_attrs, sources, jobs, sites,
	coalesce(ssii, ''), lifecycle, lifecycle_since, first_seen, last_seen`

func scanCI(sc interface{ Scan(...any) error }) (CIView, error) {
	var c CIView
	var attrs, manual []byte
	err := sc.Scan(&c.ID, &c.Key, &c.Class, &c.Technology, &c.Name, &attrs, &manual,
		pq.Array(&c.Sources), pq.Array(&c.Jobs), pq.Array(&c.Sites), &c.SSII, &c.Lifecycle,
		&c.LifecycleSince, &c.FirstSeen, &c.LastSeen)
	if err != nil {
		return c, err
	}
	_ = json.Unmarshal(attrs, &c.Attrs)
	_ = json.Unmarshal(manual, &c.ManualAttrs)
	for k, v := range c.ManualAttrs { // los atributos manuales prevalecen
		if c.Attrs == nil {
			c.Attrs = map[string]string{}
		}
		c.Attrs[k] = v
	}
	return c, nil
}

const briefCols = `id, class, technology, name, sites, coalesce(ssii, ''), lifecycle`

func scanBrief(sc interface{ Scan(...any) error }) (CIBrief, error) {
	var b CIBrief
	err := sc.Scan(&b.ID, &b.Class, &b.Technology, &b.Name, pq.Array(&b.Sites), &b.SSII, &b.Lifecycle)
	return b, err
}

// SearchFilter son los criterios de búsqueda de CI.
type SearchFilter struct {
	Q, Class, Technology, Site, Lifecycle string
	IncludeRetired                         bool
	Limit, Offset                          int
	SearchAttrs                            []string
}

// Search busca CI por nombre, código de SSII o atributos de búsqueda.
func (s *Store) Search(ctx context.Context, f SearchFilter) ([]CIBrief, int, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	var where []string
	args := []any{}
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	order := "class, name"
	if q := strings.TrimSpace(f.Q); q != "" {
		lq := strings.ToLower(q)
		pat := arg("%" + escapeLike(lq) + "%")
		conds := []string{"lower(name) LIKE " + pat, "lower(coalesce(ssii, '')) LIKE " + pat}
		for _, a := range f.SearchAttrs {
			conds = append(conds, "lower(attrs->>"+arg(a)+") LIKE "+pat)
		}
		where = append(where, "("+strings.Join(conds, " OR ")+")")
	}
	if f.Class != "" {
		where = append(where, "class = "+arg(f.Class))
	}
	if f.Technology != "" {
		where = append(where, "technology = "+arg(f.Technology))
	}
	if f.Site != "" {
		where = append(where, arg(f.Site)+" = ANY(sites)")
	}
	if f.Lifecycle != "" {
		where = append(where, "lifecycle = "+arg(f.Lifecycle))
	} else if !f.IncludeRetired {
		where = append(where, "lifecycle <> 'retired'")
	}
	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}
	countArgs := append([]any(nil), args...)
	if q := strings.TrimSpace(f.Q); q != "" { // el parámetro de orden va al final: no lo usa el recuento
		exact := arg(strings.ToLower(q))
		order = fmt.Sprintf("(lower(name) = %s OR lower(coalesce(ssii,'')) = %s) DESC, similarity(lower(name), %s) DESC, class, name", exact, exact, exact)
	}
	var out []CIBrief
	total := 0
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM ci "+w, countArgs...).Scan(&total); err != nil {
			return err
		}
		q := fmt.Sprintf("SELECT %s FROM ci %s ORDER BY %s LIMIT %d OFFSET %d", briefCols, w, order, f.Limit, f.Offset)
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBrief(rows)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, total, err
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// GetCI devuelve un CI por id (sql.ErrNoRows si no existe).
func (s *Store) GetCI(ctx context.Context, id string) (CIView, error) {
	var c CIView
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		var err error
		c, err = scanCI(tx.QueryRowContext(ctx, "SELECT "+ciCols+" FROM ci WHERE id = $1", id))
		return err
	})
	return c, err
}

// Briefs devuelve el resumen de varios CI.
func (s *Store) Briefs(ctx context.Context, ids []string) (map[string]CIBrief, error) {
	out := map[string]CIBrief{}
	if len(ids) == 0 {
		return out, nil
	}
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+briefCols+" FROM ci WHERE id = ANY($1)", pq.Array(ids))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBrief(rows)
			if err != nil {
				return err
			}
			out[b.ID] = b
		}
		return rows.Err()
	})
	return out, err
}

// Edge es una relación vigente.
type Edge struct {
	ID        int64             `json:"id"`
	Src       string            `json:"src"`
	Rel       string            `json:"rel"`
	Dst       string            `json:"dst"`
	Method    string            `json:"method"`
	Origin    string            `json:"origin"`
	Attrs     map[string]string `json:"attrs,omitempty"`
	ValidFrom time.Time         `json:"valid_from"`
	ValidTo   *time.Time        `json:"valid_to,omitempty"`
}

// Edges devuelve las relaciones vigentes (o vigentes en "at") que salen de (out) o entran
// en (in) los CI indicados.
func (s *Store) Edges(ctx context.Context, ids []string, out, in bool, at *time.Time) ([]Edge, error) {
	var conds []string
	if out {
		conds = append(conds, "src = ANY($1)")
	}
	if in {
		conds = append(conds, "dst = ANY($1)")
	}
	if len(conds) == 0 || len(ids) == 0 {
		return nil, nil
	}
	validity := "valid_to IS NULL"
	args := []any{pq.Array(ids)}
	if at != nil {
		validity = "valid_from <= $2 AND (valid_to IS NULL OR valid_to > $2)"
		args = append(args, *at)
	}
	var edges []Edge
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, src, rel, dst, method, origin, attrs, valid_from, valid_to
			FROM relation WHERE `+validity+` AND (`+strings.Join(conds, " OR ")+`)`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Edge
			var attrs []byte
			if err := rows.Scan(&e.ID, &e.Src, &e.Rel, &e.Dst, &e.Method, &e.Origin, &attrs, &e.ValidFrom, &e.ValidTo); err != nil {
				return err
			}
			_ = json.Unmarshal(attrs, &e.Attrs)
			edges = append(edges, e)
		}
		return rows.Err()
	})
	return edges, err
}

// AttrChange es un cambio de atributo.
type AttrChange struct {
	At       time.Time `json:"at"`
	Attr     string    `json:"attr"`
	OldValue *string   `json:"old"`
	NewValue *string   `json:"new"`
	Source   string    `json:"source"`
}

// Event es un evento guardado.
type Event struct {
	ID   int64           `json:"id"`
	TS   time.Time       `json:"ts"`
	Type string          `json:"type"`
	CI   string          `json:"ci_id,omitempty"`
	Data json.RawMessage `json:"data"`
}

// History devuelve los cambios de atributos de un CI, los más recientes primero.
func (s *Store) History(ctx context.Context, id string, limit int) ([]AttrChange, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var out []AttrChange
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT changed_at, attr, old_value, new_value, source
			FROM ci_attr_history WHERE ci_id = $1 ORDER BY changed_at DESC LIMIT $2`, id, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c AttrChange
			if err := rows.Scan(&c.At, &c.Attr, &c.OldValue, &c.NewValue, &c.Source); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// EventFilter son los criterios de consulta de eventos.
type EventFilter struct {
	CI, Type string
	From, To *time.Time
	Limit    int
}

// Events consulta eventos, los más recientes primero.
func (s *Store) Events(ctx context.Context, f EventFilter) ([]Event, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 200
	}
	var where []string
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	if f.CI != "" {
		where = append(where, "ci_id = "+arg(f.CI))
	}
	if f.Type != "" {
		if strings.HasSuffix(f.Type, "*") {
			where = append(where, "type LIKE "+arg(strings.TrimSuffix(f.Type, "*")+"%"))
		} else {
			where = append(where, "type = "+arg(f.Type))
		}
	}
	if f.From != nil {
		where = append(where, "ts >= "+arg(*f.From))
	}
	if f.To != nil {
		where = append(where, "ts < "+arg(*f.To))
	}
	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}
	var out []Event
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT id, ts, type, coalesce(ci_id, ''), data FROM event %s
			ORDER BY ts DESC, id DESC LIMIT %d`, w, f.Limit), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Event
			var data []byte
			if err := rows.Scan(&e.ID, &e.TS, &e.Type, &e.CI, &data); err != nil {
				return err
			}
			e.Data = data
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// Count es un recuento por clave.
type Count struct {
	Key string `json:"key"`
	N   int    `json:"n"`
}

// Stats resume el contenido de la CMDB.
type Stats struct {
	CIs         int       `json:"cis"`
	Relations   int       `json:"relations"`
	ByClass     []Count   `json:"by_class"`
	ByTech      []Count   `json:"by_technology"`
	ByLifecycle []Count   `json:"by_lifecycle"`
	BySite      []Count   `json:"by_site"`
	SSII        int       `json:"ssii"`
	LastRun     *RunView  `json:"last_run,omitempty"`
	GeneratedAt time.Time `json:"generated_at"`
}

func counts(ctx context.Context, tx *sql.Tx, q string) ([]Count, error) {
	rows, err := tx.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Count
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Key, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetStats devuelve el resumen de la CMDB.
func (s *Store) GetStats(ctx context.Context) (Stats, error) {
	st := Stats{GeneratedAt: time.Now()}
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		var err error
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM ci WHERE lifecycle <> 'retired'`).Scan(&st.CIs); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM relation WHERE valid_to IS NULL`).Scan(&st.Relations); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(DISTINCT ssii) FROM ci WHERE ssii IS NOT NULL AND lifecycle <> 'retired'`).Scan(&st.SSII); err != nil {
			return err
		}
		if st.ByClass, err = counts(ctx, tx, `SELECT class, count(*) FROM ci WHERE lifecycle <> 'retired' GROUP BY 1 ORDER BY 2 DESC, 1`); err != nil {
			return err
		}
		if st.ByTech, err = counts(ctx, tx, `SELECT technology, count(*) FROM ci WHERE lifecycle <> 'retired' GROUP BY 1 ORDER BY 2 DESC, 1`); err != nil {
			return err
		}
		if st.ByLifecycle, err = counts(ctx, tx, `SELECT lifecycle, count(*) FROM ci GROUP BY 1 ORDER BY 1`); err != nil {
			return err
		}
		st.BySite, err = counts(ctx, tx, `SELECT s, count(*) FROM ci, unnest(CASE WHEN cardinality(sites) = 0 THEN ARRAY['(sin sede)'] ELSE sites END) s
			WHERE lifecycle <> 'retired' GROUP BY 1 ORDER BY 2 DESC`)
		return err
	})
	if err != nil {
		return st, err
	}
	runs, err := s.Runs(ctx, 1)
	if err == nil && len(runs) > 0 {
		st.LastRun = &runs[0]
	}
	return st, err
}

// SSIISummary resume un Sistema de Información.
type SSIISummary struct {
	Code     string   `json:"code"`
	CIs      int      `json:"cis"`
	On       int      `json:"on"`
	Off      int      `json:"off"`
	NotFresh int      `json:"not_fresh"`
	Sites    []string `json:"sites"`
}

// SSIIList devuelve los Sistemas de Información (código en el atributo ssii_attr).
func (s *Store) SSIIList(ctx context.Context, q string) ([]SSIISummary, error) {
	var out []SSIISummary
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT ssii, count(*),
			       count(*) FILTER (WHERE attrs->>'Estado' = 'encendida'),
			       count(*) FILTER (WHERE attrs->>'Estado' = 'apagada'),
			       count(*) FILTER (WHERE lifecycle <> 'active'),
			       coalesce((SELECT array_agg(DISTINCT x ORDER BY x) FROM ci c2, unnest(c2.sites) x
			                 WHERE c2.ssii = ci.ssii AND c2.lifecycle <> 'retired'), '{}')
			FROM ci WHERE ssii IS NOT NULL AND lifecycle <> 'retired'
			  AND ($1 = '' OR lower(ssii) LIKE '%' || lower($1) || '%')
			GROUP BY ssii ORDER BY ssii`, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x SSIISummary
			if err := rows.Scan(&x.Code, &x.CIs, &x.On, &x.Off, &x.NotFresh, pq.Array(&x.Sites)); err != nil {
				return err
			}
			out = append(out, x)
		}
		return rows.Err()
	})
	return out, err
}

// CIsBySSII devuelve los CI con un código de Sistema de Información.
func (s *Store) CIsBySSII(ctx context.Context, code string) ([]CIBrief, error) {
	var out []CIBrief
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT "+briefCols+` FROM ci WHERE ssii = $1 AND lifecycle <> 'retired' ORDER BY class, name`, code)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			b, err := scanBrief(rows)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return out, err
}

// RunView es una ejecución del descubrimiento.
type RunView struct {
	ID         int64           `json:"id"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
	Status     string          `json:"status"`
	Instance   string          `json:"instance"`
	Stats      json.RawMessage `json:"stats"`
	Error      *string         `json:"error,omitempty"`
}

// Runs devuelve las últimas ejecuciones.
func (s *Store) Runs(ctx context.Context, limit int) ([]RunView, error) {
	if limit <= 0 || limit > 500 {
		limit = 20
	}
	var out []RunView
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, started_at, finished_at, status, instance, stats, error
			FROM discovery_run ORDER BY id DESC LIMIT $1`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r RunView
			var stats []byte
			if err := rows.Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Instance, &stats, &r.Error); err != nil {
				return err
			}
			r.Stats = stats
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// Quality devuelve los problemas de calidad de la última ejecución terminada.
func (s *Store) Quality(ctx context.Context) (*RunView, []Issue, error) {
	var run *RunView
	var issues []Issue
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		var r RunView
		var stats []byte
		err := tx.QueryRowContext(ctx, `SELECT id, started_at, finished_at, status, instance, stats, error
			FROM discovery_run WHERE status <> 'running' ORDER BY id DESC LIMIT 1`).
			Scan(&r.ID, &r.StartedAt, &r.FinishedAt, &r.Status, &r.Instance, &stats, &r.Error)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		r.Stats = stats
		run = &r
		rows, err := tx.QueryContext(ctx, `SELECT kind, severity, subject, count, detail FROM quality_issue
			WHERE run_id = $1 ORDER BY CASE severity WHEN 'error' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END, kind, count DESC`, r.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var is Issue
			var d []byte
			if err := rows.Scan(&is.Kind, &is.Severity, &is.Subject, &is.Count, &d); err != nil {
				return err
			}
			_ = json.Unmarshal(d, &is.Detail)
			issues = append(issues, is)
		}
		return rows.Err()
	})
	return run, issues, err
}

// LatestRunID devuelve el id de la última ejecución terminada (0 si no hay).
func (s *Store) LatestRunID(ctx context.Context) (int64, error) {
	var id int64
	err := s.Tx(ctx, true, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT coalesce(max(id), 0) FROM discovery_run WHERE status <> 'running'`).Scan(&id)
	})
	return id, err
}
