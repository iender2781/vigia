// Package engine aplica la ontología a series de Prometheus (y, opcionalmente, a alertas)
// y produce una instantánea de la CMDB: CI, relaciones y atributos.
//
// Es un port fiel de build_cmdb del correlador Python, con dos diferencias deliberadas:
//   - value_attr busca el valor con las etiquetas originales de la serie (en Python fallaba
//     cuando la regla tenía etiquetas calculadas).
//   - las expresiones regulares son RE2 (Go); el importador traduce las que no lo son.
package engine

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"vigia/internal/contexto/ontology"
)

// Labels son las etiquetas de una serie o alerta (incluye __name__).
type Labels map[string]string

// Value es el valor instantáneo de una serie.
type Value struct {
	Labels Labels
	Value  float64
}

// Input son los datos de entrada de una construcción.
type Input struct {
	Series []Labels
	Values []Value
	Alerts []Labels
}

// CI es un elemento de configuración de la instantánea.
type CI struct {
	ID        string            `json:"id"`
	Key       string            `json:"key"`
	Class     string            `json:"class"`
	Name      string            `json:"name"`
	Attrs     map[string]string `json:"attrs"`
	AttrOrder []string          `json:"-"`
	Sources   []string          `json:"sources"`
	Jobs      []string          `json:"jobs,omitempty"`
	Sites     []string          `json:"sites,omitempty"`
	Tech      string            `json:"technology,omitempty"`

	sources map[string]bool
	jobs    map[string]bool
}

// Relation es una relación From -> To ("From depende de To").
type Relation struct {
	From   string            `json:"from"`
	Rel    string            `json:"rel"`
	To     string            `json:"to"`
	Method string            `json:"method"`
	Attrs  map[string]string `json:"attrs,omitempty"`
}

// Result es la instantánea de la CMDB.
type Result struct {
	CIs     map[string]*CI // por clave natural
	Order   []string       // claves naturales en orden de creación
	Rels    []*Relation
	AlertCI map[int][]string // índice de alerta -> claves de CI a los que se cuelga
	Stats   Stats

	relIdx map[[3]string]*Relation
}

// Stats resume la construcción (sirve para el informe de calidad).
type Stats struct {
	SeriesIn      int            `json:"series_in"`
	RuleRows      map[string]int `json:"rule_rows"`
	Bridge        map[string]int `json:"bridge"`
	ManualApplied int            `json:"manual_applied"`
	ManualRels    int            `json:"manual_rels"`
	ManualErrors  []string       `json:"manual_errors,omitempty"`
}

// Engine es la ontología compilada, lista para aplicarse muchas veces.
type Engine struct {
	O     *ontology.Ontology
	rules []*crule
	// ManualLines son las líneas de relaciones manuales (ficheros heredados + entradas YAML).
	ManualLines []ManualLine
}

type crule struct {
	*ontology.Rule
	mrx   *regexp.Regexp
	where map[string]*regexp.Regexp
	comp  []ccomp
}

type ccomp struct {
	ontology.Computed
	rx       *regexp.Regexp
	patterns []*regexp.Regexp
}

func full(p string) *regexp.Regexp  { return regexp.MustCompile(`^(?:` + p + `)$`) }
func start(p string) *regexp.Regexp { return regexp.MustCompile(`^(?:` + p + `)`) }

// New compila la ontología (las expresiones ya están validadas por ontology.Load).
func New(o *ontology.Ontology) (*Engine, error) {
	e := &Engine{O: o}
	for _, r := range o.Rules {
		cr := &crule{Rule: r, where: map[string]*regexp.Regexp{}}
		if r.MetricRegex != "" {
			cr.mrx = full(r.MetricRegex)
		}
		for k, v := range r.Where {
			cr.where[k] = full(v)
		}
		for _, c := range r.Computed {
			cc := ccomp{Computed: c}
			switch {
			case c.Regex != nil:
				cc.rx = start(c.Regex.Pattern)
			case len(c.First) > 0 && c.Pattern != "":
				cc.rx = start(c.Pattern)
			case c.Map != nil:
				for _, pv := range c.Map.Patterns {
					cc.patterns = append(cc.patterns, regexp.MustCompile(pv.Pattern))
				}
			}
			cr.comp = append(cr.comp, cc)
		}
		e.rules = append(e.rules, cr)
	}
	lines, err := LoadManualLines(o)
	if err != nil {
		return nil, err
	}
	e.ManualLines = lines
	return e, nil
}

// labelKey es la clave canónica de un conjunto de etiquetas (sin __value__).
func labelKey(l Labels) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		if k != "__value__" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(l[k])
		b.WriteByte(1)
	}
	return b.String()
}

func formatValue(v float64) string {
	if v == math.Trunc(v) && !math.IsInf(v, 0) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
}

var braceRx = regexp.MustCompile(`\{([^{}]*)\}`)

// format sustituye {etiqueta}; si falta alguna, devuelve ok=false (como KeyError en Python).
func format(tpl string, x Labels) (string, bool) {
	ok := true
	out := braceRx.ReplaceAllStringFunc(tpl, func(m string) string {
		k := m[1 : len(m)-1]
		v, found := x[k]
		if !found {
			ok = false
		}
		return v
	})
	return out, ok
}

func (cr *crule) applyComputed(orig Labels) Labels {
	x := make(Labels, len(orig)+len(cr.comp))
	for k, v := range orig {
		x[k] = v
	}
	for _, c := range cr.comp {
		switch {
		case len(c.Sorted) > 0:
			vals := make([]string, 0, len(c.Sorted))
			all := true
			for _, k := range c.Sorted {
				if x[k] == "" {
					all = false
					break
				}
				vals = append(vals, x[k])
			}
			if all {
				sort.Strings(vals)
				x[c.Name] = strings.Join(vals, c.Sep)
			}
		case len(c.First) > 0:
			for _, k := range c.First {
				v := x[k]
				if v == "" {
					continue
				}
				if c.rx != nil {
					if m := c.rx.FindStringSubmatch(v); m != nil && len(m) > 1 {
						v = m[1]
					}
				}
				x[c.Name] = v
				break
			}
		case c.Map != nil:
			v := x[c.Map.From]
			val := c.Map.Values[v]
			if val == "" && v != "" {
				for i, p := range c.patterns {
					if p.MatchString(v) {
						val = c.Map.Patterns[i].Value
						break
					}
				}
			}
			if val == "" {
				val = c.Map.Default
			}
			if val != "" {
				x[c.Name] = val
			}
		case c.Format != nil:
			if v, ok := format(*c.Format, x); ok {
				x[c.Name] = v
			}
		case c.Regex != nil:
			if m := c.rx.FindStringSubmatch(x[c.Regex.From]); len(m) > 1 && m[1] != "" {
				x[c.Name] = m[1]
			}
		}
	}
	return x
}

func (r *Result) rel(a, rel, b, method string) *Relation {
	if a == "" || b == "" || a == b {
		return nil
	}
	k := [3]string{a, rel, b}
	if ex := r.relIdx[k]; ex != nil {
		return ex
	}
	x := &Relation{From: a, Rel: rel, To: b, Method: method}
	r.relIdx[k] = x
	r.Rels = append(r.Rels, x)
	return x
}

// Build aplica la ontología a los datos de entrada.
func (e *Engine) Build(in Input) *Result {
	res := &Result{
		CIs: map[string]*CI{}, AlertCI: map[int][]string{}, relIdx: map[[3]string]*Relation{},
		Stats: Stats{SeriesIn: len(in.Series), RuleRows: map[string]int{}},
	}
	vidx := make(map[string]float64, len(in.Values))
	for _, v := range in.Values {
		vidx[labelKey(v.Labels)] = v.Value
	}

	// Series agrupadas por nombre, en orden de primera aparición.
	byName := map[string][]Labels{}
	var names []string
	for _, x := range in.Series {
		n := x["__name__"]
		if _, ok := byName[n]; !ok {
			names = append(names, n)
		}
		byName[n] = append(byName[n], x)
	}
	alertRows := make([]Labels, len(in.Alerts))
	for i, a := range in.Alerts {
		x := Labels{}
		for k, v := range a {
			x[k] = v
		}
		x["__name__"] = "ALERTS"
		alertRows[i] = x
	}

	ciFn := func(spec ontology.CISpec, x, orig Labels) string {
		parts := make([]string, 0, len(spec.Key)+1)
		parts = append(parts, spec.Class)
		for _, k := range spec.Key {
			if x[k] == "" {
				return ""
			}
			parts = append(parts, x[k])
		}
		key := strings.Join(parts, "|")
		c := res.CIs[key]
		name := x[spec.NameLabel()]
		if c == nil {
			c = &CI{Key: key, ID: HashID(key), Class: spec.Class, Name: name,
				Attrs: map[string]string{}, sources: map[string]bool{}, jobs: map[string]bool{}}
			res.CIs[key] = c
			res.Order = append(res.Order, key)
		} else if spec.SetName && name != "" {
			c.Name = name
		}
		for disp, lab := range spec.Attrs {
			if v := x[lab]; v != "" {
				c.setAttr(disp, v)
			}
		}
		if spec.ValueAttr != "" && len(vidx) > 0 {
			if val, ok := vidx[labelKey(orig)]; ok && !math.IsNaN(val) {
				s := formatValue(val)
				if m, ok := spec.ValueMap[s]; ok {
					s = m
				}
				c.setAttr(spec.ValueAttr, s)
			}
		}
		c.sources[x["__name__"]] = true
		if j := x["job"]; j != "" {
			c.jobs[j] = true
		}
		return key
	}

	for _, r := range e.rules {
		var groups [][]Labels
		if r.Source == "alerts" {
			groups = [][]Labels{alertRows}
		} else if r.Metric != "" {
			groups = [][]Labels{byName[r.Metric]}
		} else {
			for _, n := range names {
				if r.mrx.MatchString(n) {
					groups = append(groups, byName[n])
				}
			}
		}
		for _, rows := range groups {
			for ri, orig := range rows {
				match := true
				for k, rx := range r.where {
					v, ok := orig[k]
					if !ok || !rx.MatchString(v) {
						match = false
						break
					}
				}
				if !match {
					continue
				}
				res.Stats.RuleRows[r.ID]++
				x := orig
				if len(r.comp) > 0 {
					x = r.applyComputed(orig)
				}
				ids := make(map[string]string, len(r.CIs))
				for _, spec := range r.CIs {
					ids[spec.Alias] = ciFn(spec, x, orig)
				}
				if r.Source == "alerts" {
					att := r.Attach
					if att == "" {
						att = r.CIs[0].Alias
					}
					if t := ids[att]; t != "" && !contains(res.AlertCI[ri], t) {
						res.AlertCI[ri] = append(res.AlertCI[ri], t)
					}
				}
				method := r.Method
				if method == "" {
					method = "métricas"
				}
				for _, rs := range r.Rels {
					a, b := ids[rs.From], ids[rs.To]
					rel := res.rel(a, rs.Rel, b, method)
					if len(rs.Attrs) > 0 && a != "" && b != "" && rel != nil {
						for disp, lab := range rs.Attrs {
							if v := x[lab]; v != "" {
								if rel.Attrs == nil {
									rel.Attrs = map[string]string{}
								}
								rel.Attrs[disp] = v
							}
						}
					}
				}
			}
		}
	}

	e.applyBridge(res, in)
	e.applySameEquipment(res)
	for _, c := range res.CIs {
		c.finish()
	}
	// Las relaciones manuales unen tecnologías distintas, así que no cambian las sedes:
	// se calculan una vez y sirven para filtrar las manuales y para el resultado.
	sites := e.Sites(res)
	e.applyManual(res, sites)
	for key, st := range sites {
		res.CIs[key].Sites = st
	}
	for _, c := range res.CIs {
		c.Tech = e.O.TechnologyOf(c.Class)
	}
	return res
}

func (c *CI) setAttr(k, v string) {
	if _, ok := c.Attrs[k]; !ok {
		c.AttrOrder = append(c.AttrOrder, k)
	}
	c.Attrs[k] = v
}

func (c *CI) finish() {
	c.Sources = c.Sources[:0]
	for s := range c.sources {
		if s == "ALERTS" {
			s = "alertas"
		}
		c.Sources = append(c.Sources, s)
	}
	sort.Strings(c.Sources)
	c.Jobs = c.Jobs[:0]
	for j := range c.jobs {
		c.Jobs = append(c.Jobs, j)
	}
	sort.Strings(c.Jobs)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// applySameEquipment enlaza CI de clases distintas con el mismo nombre normalizado.
func (e *Engine) applySameEquipment(res *Result) {
	same := map[string]bool{}
	for _, c := range e.O.Settings.SameHostClasses {
		same[c] = true
	}
	type rw struct {
		rx   *regexp.Regexp
		repl string
	}
	var rws []rw
	for _, r := range e.O.Settings.HostRewrite {
		rws = append(rws, rw{regexp.MustCompile(`(?i)` + r.Pattern), r.Replace})
	}
	hkey := func(name string) string {
		v := StripDomain(NormValue(name))
		for _, r := range rws {
			v = r.rx.ReplaceAllString(v, r.repl)
		}
		if ipv4Rx.MatchString(v) {
			return v
		}
		return compactRx.ReplaceAllString(v, "")
	}
	groups := map[string][]string{}
	var order []string
	for _, key := range res.Order {
		c := res.CIs[key]
		if !same[c.Class] {
			continue
		}
		h := hkey(c.Name)
		if _, ok := groups[h]; !ok {
			order = append(order, h)
		}
		groups[h] = append(groups[h], key)
	}
	for _, h := range order {
		ids := groups[h]
		classes := map[string]bool{}
		for _, i := range ids {
			classes[res.CIs[i].Class] = true
		}
		if len(classes) <= 1 {
			continue
		}
		sorted := append([]string(nil), ids...)
		sort.Strings(sorted)
		for _, i := range sorted[1:] {
			res.rel(i, ontology.SameEquipment, sorted[0], "nombre normalizado")
		}
	}
}

// Summary devuelve el número de CI por clase, ordenado de mayor a menor.
func (r *Result) Summary() []ClassCount {
	n := map[string]int{}
	for _, c := range r.CIs {
		n[c.Class]++
	}
	out := make([]ClassCount, 0, len(n))
	for k, v := range n {
		out = append(out, ClassCount{k, v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].N != out[j].N {
			return out[i].N > out[j].N
		}
		return out[i].Class < out[j].Class
	})
	return out
}

// ClassCount es el número de CI de una clase.
type ClassCount struct {
	Class string `json:"class"`
	N     int    `json:"n"`
}

func (cc ClassCount) String() string { return fmt.Sprintf("%s: %d", cc.Class, cc.N) }
