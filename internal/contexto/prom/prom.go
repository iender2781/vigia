// Package prom lee series, valores, targets y alertas de Prometheus y Alertmanager,
// en vivo o desde capturas guardadas por el correlador Python (--save-raw).
package prom

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"vigia/internal/contexto/engine"
)

// Client habla con uno o varios Prometheus (en HA se deduplican las series).
type Client struct {
	URLs        []string
	HTTP        *http.Client
	BearerToken string
	BasicAuth   string // usuario:contraseña
	Concurrency int
}

// Options configura el cliente.
type Options struct {
	URLs        []string
	Timeout     time.Duration
	Insecure    bool
	BearerToken string
	BasicAuth   string
	Concurrency int
}

// New crea un cliente.
func New(o Options) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if o.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opción explícita del operador
	}
	if o.Timeout == 0 {
		o.Timeout = 60 * time.Second
	}
	if o.Concurrency <= 0 {
		o.Concurrency = 4
	}
	return &Client{URLs: o.URLs, HTTP: &http.Client{Timeout: o.Timeout, Transport: tr},
		BearerToken: o.BearerToken, BasicAuth: o.BasicAuth, Concurrency: o.Concurrency}
}

func (c *Client) do(ctx context.Context, base, path string, form url.Values) ([]byte, error) {
	u := strings.TrimRight(base, "/") + path
	var req *http.Request
	var err error
	if form != nil {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vigia-contexto")
	if c.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.BearerToken)
	} else if c.BasicAuth != "" {
		user, pass, _ := strings.Cut(c.BasicAuth, ":")
		req.SetBasicAuth(user, pass)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", req.Method, u, resp.StatusCode, truncate(string(body), 300))
	}
	return body, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// SeriesResult es el resultado de leer series de todos los Prometheus.
type SeriesResult struct {
	Series []engine.Labels
	Errors []error // errores parciales (un Prometheus caído no bloquea si responde otro)
	PerURL map[string]int
}

// Series lee /api/v1/series para cada selector (en lotes y en paralelo) y deduplica.
func (c *Client) Series(ctx context.Context, matchers []string, lookback time.Duration, batch int) SeriesResult {
	if batch <= 0 {
		batch = 20
	}
	end := time.Now()
	startT := end.Add(-lookback)
	type job struct {
		url string
		ms  []string
	}
	var jobs []job
	for _, u := range c.URLs {
		for i := 0; i < len(matchers); i += batch {
			j := i + batch
			if j > len(matchers) {
				j = len(matchers)
			}
			jobs = append(jobs, job{u, matchers[i:j]})
		}
	}
	type out struct {
		url    string
		idx    int
		series []engine.Labels
		err    error
	}
	results := make([]out, len(jobs))
	sem := make(chan struct{}, c.Concurrency)
	var wg sync.WaitGroup
	for i, jb := range jobs {
		wg.Add(1)
		go func(i int, jb job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			form := url.Values{}
			for _, m := range jb.ms {
				form.Add("match[]", m)
			}
			form.Set("start", strconv.FormatInt(startT.Unix(), 10))
			form.Set("end", strconv.FormatInt(end.Unix(), 10))
			body, err := c.do(ctx, jb.url, "/api/v1/series", form)
			if err != nil {
				results[i] = out{url: jb.url, idx: i, err: err}
				return
			}
			s, err := ParseSeries(body)
			results[i] = out{url: jb.url, idx: i, series: s, err: err}
		}(i, jb)
	}
	wg.Wait()
	res := SeriesResult{PerURL: map[string]int{}}
	seen := map[string]bool{}
	for _, r := range results {
		if r.err != nil {
			res.Errors = append(res.Errors, r.err)
			continue
		}
		for _, s := range r.series {
			k := labelKey(s)
			if !seen[k] {
				seen[k] = true
				res.Series = append(res.Series, s)
				res.PerURL[r.url]++
			}
		}
	}
	return res
}

// Values lee el valor actual de las métricas indicadas (consulta instantánea).
func (c *Client) Values(ctx context.Context, metrics []string, excludeJobs []string) ([]engine.Value, []error) {
	if len(metrics) == 0 {
		return nil, nil
	}
	q := `{__name__=~"` + strings.Join(metrics, "|") + `"`
	if len(excludeJobs) > 0 {
		q += `,job!~"` + strings.Join(excludeJobs, "|") + `"`
	}
	q += "}"
	var out []engine.Value
	var errs []error
	seen := map[string]bool{}
	for _, u := range c.URLs {
		body, err := c.do(ctx, u, "/api/v1/query", url.Values{"query": {q}})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		vals, err := ParseValues(body)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, v := range vals {
			k := labelKey(v.Labels)
			if !seen[k] {
				seen[k] = true
				out = append(out, v)
			}
		}
	}
	return out, errs
}

// TargetHealth es el estado de los targets por job.
type TargetHealth struct {
	Up, Down map[string]int
}

// Targets lee /api/v1/targets y cuenta targets activos sanos y caídos por job.
func (c *Client) Targets(ctx context.Context) (TargetHealth, []error) {
	th := TargetHealth{Up: map[string]int{}, Down: map[string]int{}}
	var errs []error
	for _, u := range c.URLs {
		body, err := c.do(ctx, u, "/api/v1/targets?state=active", nil)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var r struct {
			Data struct {
				ActiveTargets []struct {
					Labels map[string]string `json:"labels"`
					Health string            `json:"health"`
				} `json:"activeTargets"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			errs = append(errs, err)
			continue
		}
		for _, t := range r.Data.ActiveTargets {
			if t.Health == "up" {
				th.Up[t.Labels["job"]]++
			} else {
				th.Down[t.Labels["job"]]++
			}
		}
	}
	return th, errs
}

// Alerts lee las alertas activas de Alertmanager (API v2).
func Alerts(ctx context.Context, c *Client, base string) ([]Alert, error) {
	body, err := c.do(ctx, base, "/api/v2/alerts?active=true&silenced=true&inhibited=true&unprocessed=true", nil)
	if err != nil {
		return nil, err
	}
	return ParseAlerts(body)
}

// Alert es una alerta normalizada.
type Alert struct {
	Fingerprint string            `json:"fingerprint"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	State       string            `json:"state"`
}

// ---------------------------------------------------------------------------
// Formatos de captura
// ---------------------------------------------------------------------------

// ParseSeries acepta la respuesta de /api/v1/series o una lista de etiquetas.
func ParseSeries(b []byte) ([]engine.Labels, error) {
	var raw json.RawMessage = b
	var wrap struct {
		Data json.RawMessage `json:"data"`
	}
	if len(b) > 0 && b[0] == '{' {
		if err := json.Unmarshal(b, &wrap); err != nil {
			return nil, err
		}
		raw = wrap.Data
	}
	var out []engine.Labels
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("series: %w", err)
	}
	return out, nil
}

// ParseValues acepta la respuesta de /api/v1/query (vector) o la lista values.json del
// correlador Python ({..etiquetas.., "__value__": n}).
func ParseValues(b []byte) ([]engine.Value, error) {
	var any_ any
	if err := json.Unmarshal(b, &any_); err != nil {
		return nil, err
	}
	if m, ok := any_.(map[string]any); ok {
		if d, ok := m["data"].(map[string]any); ok {
			any_ = d["result"]
		} else if d, ok := m["data"]; ok {
			any_ = d
		}
	}
	list, _ := any_.([]any)
	out := make([]engine.Value, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if metric, ok := m["metric"].(map[string]any); ok {
			v := math.NaN()
			if arr, ok := m["value"].([]any); ok && len(arr) == 2 {
				if s, ok := arr[1].(string); ok {
					if f, err := strconv.ParseFloat(s, 64); err == nil {
						v = f
					}
				}
			}
			out = append(out, engine.Value{Labels: toLabels(metric), Value: v})
			continue
		}
		v := math.NaN()
		switch t := m["__value__"].(type) {
		case float64:
			v = t
		case string:
			if f, err := strconv.ParseFloat(t, 64); err == nil {
				v = f
			}
		}
		delete(m, "__value__")
		out = append(out, engine.Value{Labels: toLabels(m), Value: v})
	}
	return out, nil
}

// ParseAlerts acepta la respuesta de Alertmanager v2 o la de Prometheus /api/v1/alerts.
func ParseAlerts(b []byte) ([]Alert, error) {
	var any_ any
	if err := json.Unmarshal(b, &any_); err != nil {
		return nil, err
	}
	if m, ok := any_.(map[string]any); ok {
		if d, ok := m["data"].(map[string]any); ok {
			any_ = d["alerts"]
		} else if d, ok := m["data"]; ok {
			any_ = d
		}
	}
	list, _ := any_.([]any)
	var out []Alert
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		a := Alert{Labels: map[string]string{}, Annotations: map[string]string{}}
		if l, ok := m["labels"].(map[string]any); ok {
			a.Labels = toLabels(l)
		}
		if l, ok := m["annotations"].(map[string]any); ok {
			a.Annotations = toLabels(l)
		}
		a.Fingerprint, _ = m["fingerprint"].(string)
		if st, ok := m["status"].(map[string]any); ok { // Alertmanager
			a.State, _ = st["state"].(string)
			if s, _ := m["startsAt"].(string); s != "" {
				a.StartsAt, _ = time.Parse(time.RFC3339Nano, s)
			}
		} else { // Prometheus
			a.State, _ = m["state"].(string)
			if a.State == "inactive" {
				continue
			}
			if s, _ := m["activeAt"].(string); s != "" {
				a.StartsAt, _ = time.Parse(time.RFC3339Nano, s)
			}
		}
		out = append(out, a)
	}
	return out, nil
}

func toLabels(m map[string]any) engine.Labels {
	l := engine.Labels{}
	for k, v := range m {
		switch t := v.(type) {
		case string:
			l[k] = t
		case nil:
		default:
			l[k] = fmt.Sprint(t)
		}
	}
	return l
}

func labelKey(l engine.Labels) string {
	b, _ := json.Marshal(l) // json ordena las claves de los mapas
	return string(b)
}

// ReadFile lee un fichero de captura.
func ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }
