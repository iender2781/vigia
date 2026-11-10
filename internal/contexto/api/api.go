// Package api sirve la API REST (/api/v1), el canal SSE y la web.
package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"vigia/internal/contexto/config"
	"vigia/internal/contexto/ontology"
	"vigia/internal/contexto/store"
	"vigia/internal/platform/metrics"
)

var (
	mReq = metrics.Default.NewCounter("contexto_http_requests_total", "Peticiones HTTP por ruta y código.", "route", "code")
	mDur = metrics.Default.NewHistogram("contexto_http_request_duration_seconds", "Duración de las peticiones HTTP.", metrics.DefBuckets, "route")
)

// API es el servidor HTTP de la plataforma.
type API struct {
	Cfg     *config.Config
	Store   *store.Store
	O       *ontology.Ontology
	Log     *slog.Logger
	Web     http.Handler
	Version string
}

// Handler devuelve el enrutador completo.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	h := func(pattern string, fn func(http.ResponseWriter, *http.Request) error) {
		mux.Handle(pattern, a.instrument(pattern, a.auth(func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r); err != nil {
				a.fail(w, r, err)
			}
		})))
	}
	h("GET /api/v1/stats", a.stats)
	h("GET /api/v1/ontology", a.ontology)
	h("GET /api/v1/ci", a.search)
	h("GET /api/v1/ci/{id}", a.getCI)
	h("GET /api/v1/ci/{id}/relations", a.relations)
	h("GET /api/v1/ci/{id}/graph", a.graphHandler)
	h("GET /api/v1/ci/{id}/impact", a.impact)
	h("GET /api/v1/ci/{id}/history", a.history)
	h("GET /api/v1/ssii", a.ssiiList)
	h("GET /api/v1/ssii/{code}", a.ssiiDetail)
	h("GET /api/v1/quality", a.quality)
	h("GET /api/v1/discovery/runs", a.runs)
	h("GET /api/v1/events", a.events)
	h("GET /api/v1/stream", a.stream)
	h("GET /api/v1/whoami", a.whoami)
	mux.Handle("GET /api/", a.instrument("GET /api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "ruta no encontrada"})
	})))
	if a.Web != nil {
		mux.Handle("GET /", a.Web)
	}
	return securityHeaders(mux)
}

// OpsHandler sirve /healthz, /readyz y /metrics (puerto interno, sin autenticación).
func (a *API) OpsHandler(ready func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := ready(ctx); err != nil {
			http.Error(w, "no preparado: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", metrics.Default.Handler())
	return mux
}

// --- utilidades -------------------------------------------------------------

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) { s.code = c; s.ResponseWriter.WriteHeader(c) }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (a *API) instrument(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		mReq.Inc(route, strconv.Itoa(sw.code))
		if !strings.HasSuffix(route, "/stream") {
			mDur.Observe(time.Since(t0).Seconds(), route)
		}
	})
}

type ctxKey string

func (a *API) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := ""
		if tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); tok != "" && tok != r.Header.Get("Authorization") {
			for _, t := range a.Cfg.APITokens {
				if subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
					user = "token"
					break
				}
			}
		}
		if user == "" && r.URL.Query().Get("access_token") != "" && strings.HasSuffix(r.URL.Path, "/stream") {
			// EventSource no admite cabeceras: el canal SSE acepta el token como parámetro.
			tok := r.URL.Query().Get("access_token")
			for _, t := range a.Cfg.APITokens {
				if subtle.ConstantTimeCompare([]byte(tok), []byte(t)) == 1 {
					user = "token"
				}
			}
		}
		switch a.Cfg.AuthMode {
		case "none":
			if user == "" {
				user = "anónimo"
			}
		case "proxy":
			if user == "" {
				user = r.Header.Get(a.Cfg.AuthProxyHeader)
			}
		}
		if user == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no autenticado", "auth_mode": a.Cfg.AuthMode})
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey("user"), user)))
	})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return httpError{http.StatusBadRequest, fmt.Sprintf(format, a...)}
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var he httpError
	switch {
	case errors.As(err, &he):
		writeJSON(w, he.code, map[string]string{"error": he.msg})
	case errors.Is(err, sql.ErrNoRows):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no encontrado"})
	case errors.Is(err, context.Canceled):
	default:
		a.Log.Error("error en la API", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "error interno"})
	}
}

func qInt(r *http.Request, k string, def, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(k))
	if err != nil || v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func qTime(r *http.Request, k string) (*time.Time, error) {
	v := r.URL.Query().Get(k)
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, badRequest("%s debe ser una fecha RFC 3339 (p. ej. 2026-11-10T10:05:00+01:00)", k)
	}
	return &t, nil
}

// --- handlers ---------------------------------------------------------------

func (a *API) whoami(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, 200, map[string]any{"user": r.Context().Value(ctxKey("user")), "auth_mode": a.Cfg.AuthMode,
		"version": a.Version, "tenant": a.Cfg.TenantName})
	return nil
}

func (a *API) stats(w http.ResponseWriter, r *http.Request) error {
	st, err := a.Store.GetStats(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, 200, st)
	return nil
}

func (a *API) ontology(w http.ResponseWriter, _ *http.Request) error {
	type pack struct {
		Name, Technology string
		Rules            int
	}
	var packs []pack
	for _, p := range a.O.Packs {
		packs = append(packs, pack{p.Name, p.Technology, len(p.Rules)})
	}
	writeJSON(w, 200, map[string]any{
		"technologies": a.O.Technologies(), "classes": a.O.Classes, "relations": a.O.Relations,
		"packs": packs, "sites": a.O.Sites, "ssii_attr": a.O.Settings.SSIIAttr, "search_attrs": a.O.Settings.SearchAttrs,
	})
	return nil
}

func (a *API) search(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	items, total, err := a.Store.Search(r.Context(), store.SearchFilter{
		Q: q.Get("q"), Class: q.Get("class"), Technology: q.Get("technology"), Site: q.Get("site"),
		Lifecycle: q.Get("lifecycle"), IncludeRetired: q.Get("include_retired") == "true",
		Limit: qInt(r, "limit", 50, 500), Offset: qInt(r, "offset", 0, 1_000_000),
		SearchAttrs: a.O.Settings.SearchAttrs,
	})
	if err != nil {
		return err
	}
	if items == nil {
		items = []store.CIBrief{}
	}
	writeJSON(w, 200, map[string]any{"total": total, "items": items})
	return nil
}

func (a *API) getCI(w http.ResponseWriter, r *http.Request) error {
	c, err := a.Store.GetCI(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, c)
	return nil
}

// RelView es una relación con el CI del otro extremo.
type RelView struct {
	store.Edge
	Peer        store.CIBrief `json:"peer"`
	Causal      bool          `json:"causal"`
	Informative bool          `json:"informative"`
	Identity    bool          `json:"identity"`
}

func (a *API) relations(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	at, err := qTime(r, "at")
	if err != nil {
		return err
	}
	edges, err := a.Store.Edges(r.Context(), []string{id}, true, true, at)
	if err != nil {
		return err
	}
	peers := map[string]bool{}
	for _, e := range edges {
		peers[e.Src], peers[e.Dst] = true, true
	}
	ids := make([]string, 0, len(peers))
	for p := range peers {
		ids = append(ids, p)
	}
	briefs, err := a.Store.Briefs(r.Context(), ids)
	if err != nil {
		return err
	}
	out, in := []RelView{}, []RelView{}
	for _, e := range edges {
		v := RelView{Edge: e}
		if rt := a.O.Relation(e.Rel); rt != nil {
			v.Causal, v.Informative, v.Identity = rt.Causal, rt.Informative, rt.Identity
		}
		if e.Src == id {
			v.Peer = briefs[e.Dst]
			out = append(out, v)
		} else {
			v.Peer = briefs[e.Src]
			in = append(in, v)
		}
	}
	sortRel := func(l []RelView) {
		sort.Slice(l, func(i, j int) bool {
			if l[i].Rel != l[j].Rel {
				return l[i].Rel < l[j].Rel
			}
			return l[i].Peer.Name < l[j].Peer.Name
		})
	}
	sortRel(out)
	sortRel(in)
	writeJSON(w, 200, map[string]any{"depends_on": out, "dependents": in})
	return nil
}

// Node es un nodo del grafo con su distancia al origen.
type Node struct {
	store.CIBrief
	Depth int    `json:"depth"`
	Side  string `json:"side,omitempty"`
}

// Graph es un subgrafo.
type Graph struct {
	Nodes     []Node       `json:"nodes"`
	Edges     []store.Edge `json:"edges"`
	Truncated bool         `json:"truncated"`
}

// walk recorre el grafo desde roots. dir "up" sigue las dependencias (lo que necesita),
// "down" a los dependientes (a quién afecta). La identidad ("mismo equipo") se recorre en
// ambos sentidos; las relaciones informativas no se recorren.
func (a *API) walk(ctx context.Context, roots []string, dir string, depth, limit int, at *time.Time) (map[string]int, []store.Edge, bool, error) {
	seen := map[string]int{}
	for _, r := range roots {
		seen[r] = 0
	}
	frontier := append([]string(nil), roots...)
	var edges []store.Edge
	edgeSeen := map[int64]bool{}
	truncated := false
	for d := 1; d <= depth && len(frontier) > 0; d++ {
		es, err := a.Store.Edges(ctx, frontier, true, true, at)
		if err != nil {
			return nil, nil, false, err
		}
		inFrontier := map[string]bool{}
		for _, f := range frontier {
			inFrontier[f] = true
		}
		var next []string
		for _, e := range es {
			rt := a.O.Relation(e.Rel)
			informative := rt != nil && rt.Informative
			identity := rt != nil && rt.Identity
			var to string
			switch {
			case informative:
				continue
			case identity && inFrontier[e.Src]:
				to = e.Dst
			case identity && inFrontier[e.Dst]:
				to = e.Src
			case dir == "up" && inFrontier[e.Src]:
				to = e.Dst
			case dir == "down" && inFrontier[e.Dst]:
				to = e.Src
			default:
				continue
			}
			if !edgeSeen[e.ID] {
				edgeSeen[e.ID] = true
				edges = append(edges, e)
			}
			if _, ok := seen[to]; ok {
				continue
			}
			if len(seen) >= limit {
				truncated = true
				continue
			}
			seen[to] = d
			next = append(next, to)
		}
		frontier = next
	}
	// Solo se devuelven aristas entre nodos incluidos.
	var kept []store.Edge
	for _, e := range edges {
		if _, ok := seen[e.Src]; ok {
			if _, ok := seen[e.Dst]; ok {
				kept = append(kept, e)
			}
		}
	}
	return seen, kept, truncated, nil
}

func (a *API) toGraph(ctx context.Context, seen map[string]int, edges []store.Edge, side map[string]string, truncated bool) (Graph, error) {
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	briefs, err := a.Store.Briefs(ctx, ids)
	if err != nil {
		return Graph{}, err
	}
	g := Graph{Nodes: []Node{}, Edges: edges, Truncated: truncated}
	if g.Edges == nil {
		g.Edges = []store.Edge{}
	}
	for id, d := range seen {
		if b, ok := briefs[id]; ok {
			g.Nodes = append(g.Nodes, Node{CIBrief: b, Depth: d, Side: side[id]})
		}
	}
	sort.Slice(g.Nodes, func(i, j int) bool {
		if g.Nodes[i].Depth != g.Nodes[j].Depth {
			return g.Nodes[i].Depth < g.Nodes[j].Depth
		}
		if g.Nodes[i].Class != g.Nodes[j].Class {
			return g.Nodes[i].Class < g.Nodes[j].Class
		}
		return g.Nodes[i].Name < g.Nodes[j].Name
	})
	return g, nil
}

func (a *API) graphHandler(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	dir := r.URL.Query().Get("direction")
	if dir == "" {
		dir = "both"
	}
	if dir != "up" && dir != "down" && dir != "both" {
		return badRequest("direction debe ser up, down o both")
	}
	at, err := qTime(r, "at")
	if err != nil {
		return err
	}
	depth, limit := qInt(r, "depth", 3, 8), qInt(r, "limit", 300, 2000)
	seen := map[string]int{id: 0}
	side := map[string]string{id: "self"}
	var edges []store.Edge
	truncated := false
	for _, d := range []string{"up", "down"} {
		if dir != "both" && dir != d {
			continue
		}
		s, e, t, err := a.walk(r.Context(), []string{id}, d, depth, limit, at)
		if err != nil {
			return err
		}
		for k, v := range s {
			if _, ok := seen[k]; !ok {
				seen[k] = v
				side[k] = d
			}
		}
		edges = append(edges, e...)
		truncated = truncated || t
	}
	g, err := a.toGraph(r.Context(), seen, dedupEdges(edges), side, truncated)
	if err != nil {
		return err
	}
	writeJSON(w, 200, g)
	return nil
}

func dedupEdges(es []store.Edge) []store.Edge {
	seen := map[int64]bool{}
	var out []store.Edge
	for _, e := range es {
		if !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out
}

func (a *API) impact(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	seen, _, truncated, err := a.walk(r.Context(), []string{id}, "down", 12, 20000, nil)
	if err != nil {
		return err
	}
	delete(seen, id)
	ids := make([]string, 0, len(seen))
	for k := range seen {
		ids = append(ids, k)
	}
	briefs, err := a.Store.Briefs(r.Context(), ids)
	if err != nil {
		return err
	}
	byClass := map[string]int{}
	ssii := map[string]int{}
	direct := []store.CIBrief{}
	for k, b := range briefs {
		byClass[b.Class]++
		if b.SSII != "" {
			ssii[b.SSII]++
		}
		if seen[k] == 1 {
			direct = append(direct, b)
		}
	}
	if self, err := a.Store.GetCI(r.Context(), id); err == nil && self.SSII != "" {
		ssii[self.SSII]++
	}
	type kv struct {
		Key string `json:"key"`
		N   int    `json:"n"`
	}
	toList := func(m map[string]int) []kv {
		out := make([]kv, 0, len(m))
		for k, v := range m {
			out = append(out, kv{k, v})
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].N != out[j].N {
				return out[i].N > out[j].N
			}
			return out[i].Key < out[j].Key
		})
		return out
	}
	sort.Slice(direct, func(i, j int) bool { return direct[i].Class+direct[i].Name < direct[j].Class+direct[j].Name })
	writeJSON(w, 200, map[string]any{"total": len(briefs), "by_class": toList(byClass), "ssii": toList(ssii),
		"direct": direct, "truncated": truncated})
	return nil
}

func (a *API) history(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	hist, err := a.Store.History(r.Context(), id, qInt(r, "limit", 200, 1000))
	if err != nil {
		return err
	}
	evs, err := a.Store.Events(r.Context(), store.EventFilter{CI: id, Limit: qInt(r, "limit", 200, 1000)})
	if err != nil {
		return err
	}
	if hist == nil {
		hist = []store.AttrChange{}
	}
	if evs == nil {
		evs = []store.Event{}
	}
	writeJSON(w, 200, map[string]any{"attributes": hist, "events": evs})
	return nil
}

func (a *API) ssiiList(w http.ResponseWriter, r *http.Request) error {
	l, err := a.Store.SSIIList(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		return err
	}
	if l == nil {
		l = []store.SSIISummary{}
	}
	writeJSON(w, 200, map[string]any{"attr": a.O.Settings.SSIIAttr, "items": l})
	return nil
}

// ssiiDetail devuelve los CI de un Sistema de Información, de qué dependen (hasta 5 niveles)
// y lo que los publica en otras tecnologías (el mismo equipo y sus dependientes, 3 niveles).
func (a *API) ssiiDetail(w http.ResponseWriter, r *http.Request) error {
	code := r.PathValue("code")
	own, err := a.Store.CIsBySSII(r.Context(), code)
	if err != nil {
		return err
	}
	if len(own) == 0 {
		return sql.ErrNoRows
	}
	roots := make([]string, len(own))
	side := map[string]string{}
	for i, c := range own {
		roots[i] = c.ID
		side[c.ID] = "self"
	}
	limit := qInt(r, "limit", 1500, 5000)
	up, upEdges, t1, err := a.walk(r.Context(), roots, "up", 5, limit, nil)
	if err != nil {
		return err
	}
	down, downEdges, t2, err := a.walk(r.Context(), roots, "down", 3, limit, nil)
	if err != nil {
		return err
	}
	seen := map[string]int{}
	for k, v := range up {
		seen[k] = v
		if side[k] == "" {
			side[k] = "depends"
		}
	}
	for k, v := range down {
		if _, ok := seen[k]; !ok {
			seen[k] = v
			side[k] = "publishes"
		}
	}
	g, err := a.toGraph(r.Context(), seen, dedupEdges(append(upEdges, downEdges...)), side, t1 || t2)
	if err != nil {
		return err
	}
	byClass := map[string]int{}
	for _, n := range g.Nodes {
		byClass[n.Class]++
	}
	writeJSON(w, 200, map[string]any{"code": code, "cis": own, "graph": g, "by_class": byClass})
	return nil
}

func (a *API) quality(w http.ResponseWriter, r *http.Request) error {
	run, issues, err := a.Store.Quality(r.Context())
	if err != nil {
		return err
	}
	if issues == nil {
		issues = []store.Issue{}
	}
	writeJSON(w, 200, map[string]any{"run": run, "issues": issues})
	return nil
}

func (a *API) runs(w http.ResponseWriter, r *http.Request) error {
	l, err := a.Store.Runs(r.Context(), qInt(r, "limit", 20, 500))
	if err != nil {
		return err
	}
	if l == nil {
		l = []store.RunView{}
	}
	writeJSON(w, 200, map[string]any{"items": l})
	return nil
}

func (a *API) events(w http.ResponseWriter, r *http.Request) error {
	from, err := qTime(r, "from")
	if err != nil {
		return err
	}
	to, err := qTime(r, "to")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	l, err := a.Store.Events(r.Context(), store.EventFilter{CI: q.Get("ci"), Type: q.Get("type"), From: from, To: to,
		Limit: qInt(r, "limit", 200, 1000)})
	if err != nil {
		return err
	}
	if l == nil {
		l = []store.Event{}
	}
	writeJSON(w, 200, map[string]any{"items": l})
	return nil
}

// stream es un canal SSE que avisa cuando termina una ejecución del descubrimiento.
// (En la fase 2 se alimentará de NATS con los cambios de estado e incidencias.)
func (a *API) stream(w http.ResponseWriter, r *http.Request) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming no soportado")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	last, _ := a.Store.LatestRunID(r.Context())
	fmt.Fprintf(w, "event: hello\ndata: {\"run_id\":%d}\n\n", last)
	fl.Flush()
	poll := time.NewTicker(10 * time.Second)
	beat := time.NewTicker(25 * time.Second)
	defer poll.Stop()
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-beat.C:
			fmt.Fprint(w, ": latido\n\n")
			fl.Flush()
		case <-poll.C:
			id, err := a.Store.LatestRunID(r.Context())
			if err == nil && id != last {
				last = id
				fmt.Fprintf(w, "event: discovery\ndata: {\"run_id\":%d}\n\n", id)
				fl.Flush()
			}
		}
	}
}
