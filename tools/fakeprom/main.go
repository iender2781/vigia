// Command fakeprom sirve una captura guardada (series.json, values.json, alerts.json y,
// opcionalmente, targets.json) con las APIs de Prometheus y Alertmanager que usa la
// plataforma. Sirve para pruebas locales y de integración sin tocar producción.
//
//	go run ./tools/fakeprom -dir CAPTURA -addr :9090
//
// Admite /api/v1/series (match[] con =, !=, =~, !~), /api/v1/query ({__name__=~"..."}),
// /api/v1/targets y /api/v2/alerts. Con SIGHUP vuelve a leer los ficheros.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"

	"vigia/internal/contexto/engine"
	"vigia/internal/contexto/prom"
)

type matcher struct {
	name, op, value string
	rx              *regexp.Regexp
}

var matcherRx = regexp.MustCompile(`\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*(=~|!~|!=|=)\s*"((?:[^"\\]|\\.)*)"\s*,?`)

func parseSelector(s string) ([]matcher, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return nil, fmt.Errorf("selector no soportado: %s", s)
	}
	body := s[1 : len(s)-1]
	var out []matcher
	for _, m := range matcherRx.FindAllStringSubmatch(body, -1) {
		v := strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(m[3])
		mt := matcher{name: m[1], op: m[2], value: v}
		if mt.op == "=~" || mt.op == "!~" {
			rx, err := regexp.Compile("^(?:" + v + ")$")
			if err != nil {
				return nil, err
			}
			mt.rx = rx
		}
		out = append(out, mt)
	}
	return out, nil
}

func (m matcher) ok(l engine.Labels) bool {
	v := l[m.name]
	switch m.op {
	case "=":
		return v == m.value
	case "!=":
		return v != m.value
	case "=~":
		return m.rx.MatchString(v)
	default:
		return !m.rx.MatchString(v)
	}
}

type data struct {
	mu      sync.RWMutex
	dir     string
	series  []engine.Labels
	values  []engine.Value
	alerts  json.RawMessage
	targets json.RawMessage
}

func (d *data) load() error {
	b, err := os.ReadFile(filepath.Join(d.dir, "series.json"))
	if err != nil {
		return err
	}
	s, err := prom.ParseSeries(b)
	if err != nil {
		return err
	}
	var v []engine.Value
	if b, err := os.ReadFile(filepath.Join(d.dir, "values.json")); err == nil {
		if v, err = prom.ParseValues(b); err != nil {
			return err
		}
	}
	alerts := json.RawMessage("[]")
	if b, err := os.ReadFile(filepath.Join(d.dir, "alerts.json")); err == nil {
		alerts = b
	}
	var targets json.RawMessage
	if b, err := os.ReadFile(filepath.Join(d.dir, "targets.json")); err == nil {
		targets = b
	} else { // todos los jobs de la captura, sanos
		jobs := map[string]bool{}
		for _, x := range s {
			jobs[x["job"]] = true
		}
		var at []map[string]any
		for j := range jobs {
			at = append(at, map[string]any{"labels": map[string]string{"job": j}, "health": "up"})
		}
		targets, _ = json.Marshal(map[string]any{"status": "success", "data": map[string]any{"activeTargets": at}})
	}
	d.mu.Lock()
	d.series, d.values, d.alerts, d.targets = s, v, alerts, targets
	d.mu.Unlock()
	log.Printf("captura cargada: %d series, %d valores", len(s), len(v))
	return nil
}

func main() {
	dir := flag.String("dir", ".", "directorio de la captura")
	addr := flag.String("addr", ":9090", "dirección de escucha")
	flag.Parse()
	d := &data{dir: *dir}
	if err := d.load(); err != nil {
		log.Fatal(err)
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			if err := d.load(); err != nil {
				log.Print("recarga fallida: ", err)
			}
		}
	}()
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	http.HandleFunc("/api/v1/series", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var sels [][]matcher
		for _, m := range r.Form["match[]"] {
			ms, err := parseSelector(m)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			sels = append(sels, ms)
		}
		d.mu.RLock()
		defer d.mu.RUnlock()
		out := []engine.Labels{}
		for _, s := range d.series {
			for _, ms := range sels {
				all := true
				for _, m := range ms {
					if !m.ok(s) {
						all = false
						break
					}
				}
				if all {
					out = append(out, s)
					break
				}
			}
		}
		reply(w, map[string]any{"status": "success", "data": out})
	})
	http.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		ms, err := parseSelector(r.Form.Get("query"))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		d.mu.RLock()
		defer d.mu.RUnlock()
		res := []map[string]any{}
		for _, v := range d.values {
			all := true
			for _, m := range ms {
				if !m.ok(v.Labels) {
					all = false
					break
				}
			}
			if all {
				res = append(res, map[string]any{"metric": v.Labels, "value": []any{0, fmt.Sprint(v.Value)}})
			}
		}
		reply(w, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": res}})
	})
	http.HandleFunc("/api/v1/targets", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.RLock()
		defer d.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(d.targets)
	})
	http.HandleFunc("/api/v2/alerts", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.RLock()
		defer d.mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write(d.alerts)
	})
	log.Printf("fakeprom escuchando en %s (captura %s)", *addr, *dir)
	log.Fatal(http.ListenAndServe(*addr, nil))
}
