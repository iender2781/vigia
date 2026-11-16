package ontology

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Lectura de JSON conservando el orden de las claves (el correlador Python
// depende del orden de "cis", "computed" y "map_regex").
// ---------------------------------------------------------------------------

type omap struct {
	keys []string
	vals map[string]any
}

func (m *omap) get(k string) any { return m.vals[k] }

func (m *omap) str(k string) string {
	s, _ := m.vals[k].(string)
	return s
}

func decodeOrdered(r io.Reader) (any, error) {
	dec := json.NewDecoder(r)
	dec.UseNumber()
	return decodeValue(dec)
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			m := &omap{vals: map[string]any{}}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k := kt.(string)
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				if _, dup := m.vals[k]; !dup {
					m.keys = append(m.keys, k)
				}
				m.vals[k] = v
			}
			_, err := dec.Token()
			return m, err
		case '[':
			var out []any
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			_, err := dec.Token()
			return out, err
		}
	}
	return tok, nil
}

func strList(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func strMap(v any) map[string]string {
	m, ok := v.(*omap)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, k := range m.keys {
		out[k] = fmt.Sprint(m.vals[k])
	}
	return out
}

func toPlain(v any) any {
	switch t := v.(type) {
	case *omap:
		out := map[string]any{}
		for _, k := range t.keys {
			out[k] = toPlain(t.vals[k])
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			out[i] = toPlain(x)
		}
		return out
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		f, _ := t.Float64()
		return f
	}
	return v
}

// ---------------------------------------------------------------------------
// Conversión de expresiones regulares de Python a RE2 (Go)
// ---------------------------------------------------------------------------

// lookaheadTail reconoce "PREFIJO(?=COLA$)" y lo traduce a "PREFIJO(COLA)$" con "$1"
// en el reemplazo: es la única forma de lookahead usada en las configuraciones conocidas.
var lookaheadTail = regexp.MustCompile(`^(.*)\(\?=(.*)\$\)$`)

func convertRewrite(pattern, repl string) (Rewrite, error) {
	if _, err := regexp.Compile(pattern); err == nil {
		return Rewrite{Pattern: pattern, Replace: pyReplToGo(repl)}, nil
	}
	if m := lookaheadTail.FindStringSubmatch(pattern); m != nil {
		p := m[1] + "(" + m[2] + ")$"
		if _, err := regexp.Compile(p); err == nil {
			return Rewrite{Pattern: p, Replace: pyReplToGo(repl) + "${1}"}, nil
		}
	}
	return Rewrite{}, fmt.Errorf("la expresión %q no es compatible con RE2 (Go) y no se puede traducir automáticamente", pattern)
}

func pyReplToGo(r string) string {
	return regexp.MustCompile(`\\(\d)`).ReplaceAllString(r, `$${$1}`)
}

// ---------------------------------------------------------------------------
// Importación
// ---------------------------------------------------------------------------

// ImportReport resume lo que hizo el importador.
type ImportReport struct {
	Rules    int
	Packs    map[string]int
	Classes  int
	Warnings []string
}

// ImportLegacy convierte un config_junta.json del correlador Python en una ontología.
// Conserva el orden de las reglas (campo order) para que el resultado sea idéntico.
func ImportLegacy(configPath string) (*Ontology, *ImportReport, error) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, nil, err
	}
	root, err := decodeOrdered(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", configPath, err)
	}
	cfg, ok := root.(*omap)
	if !ok {
		return nil, nil, fmt.Errorf("%s: se esperaba un objeto JSON", configPath)
	}
	rep := &ImportReport{Packs: map[string]int{}}
	model, _ := cfg.get("cmdb_model").(*omap)
	if model == nil {
		return nil, nil, fmt.Errorf("%s: no tiene cmdb_model", configPath)
	}
	o := &Ontology{Correlation: map[string]any{}}

	// --- Clases por tecnología ---
	classTech := map[string]string{}
	var classOrder []string
	if techs, ok := model.get("technologies").(*omap); ok {
		for _, t := range techs.keys {
			for _, c := range strList(techs.get(t)) {
				if _, seen := classTech[c]; !seen {
					classTech[c] = t
					classOrder = append(classOrder, c)
				}
			}
		}
	}
	devices := map[string]bool{}
	if dt, ok := model.get("device_types").(*omap); ok {
		for _, t := range dt.keys {
			for _, c := range strList(dt.get(t)) {
				devices[c] = true
			}
		}
	}
	siteFromName := map[string]bool{}
	for _, c := range strList(model.get("site_from_name_types")) {
		siteFromName[c] = true
	}
	navParent := strMap(model.get("nav_parent"))

	// --- Reglas ---
	rulesRaw, _ := model.get("rules").([]any)
	usedRels := []string{}
	relSeen := map[string]bool{}
	addRel := func(r string) {
		if !relSeen[r] {
			relSeen[r] = true
			usedRels = append(usedRels, r)
		}
	}
	packRules := map[string][]*Rule{}
	idCount := map[string]int{}
	for i, rv := range rulesRaw {
		rm, ok := rv.(*omap)
		if !ok {
			continue
		}
		r := &Rule{
			Order:       (i + 1) * 10,
			Metric:      rm.str("metric"),
			MetricRegex: rm.str("metric_regex"),
			Source:      rm.str("source"),
			Where:       strMap(rm.get("where")),
			Attach:      rm.str("attach"),
			Method:      rm.str("method"),
		}
		if comp, ok := rm.get("computed").(*omap); ok {
			for _, name := range comp.keys {
				c, err := convertComputed(name, comp.get(name))
				if err != nil {
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("regla %d: %v", i+1, err))
					continue
				}
				r.Computed = append(r.Computed, c)
			}
		}
		if cis, ok := rm.get("cis").(*omap); ok {
			for _, alias := range cis.keys {
				sm, _ := cis.get(alias).(*omap)
				if sm == nil {
					continue
				}
				spec := CISpec{
					Alias:     alias,
					Class:     sm.str("type"),
					Key:       strList(sm.get("key")),
					Name:      sm.str("name"),
					Attrs:     strMap(sm.get("attrs")),
					ValueAttr: sm.str("value_attr"),
					ValueMap:  strMap(sm.get("value_map")),
				}
				if b, ok := sm.get("set_name").(bool); ok {
					spec.SetName = b
				}
				if spec.Name == spec.Key[len(spec.Key)-1] {
					spec.Name = "" // es el valor por defecto
				}
				if _, ok := classTech[spec.Class]; !ok {
					classTech[spec.Class] = "Otros"
					classOrder = append(classOrder, spec.Class)
					rep.Warnings = append(rep.Warnings, fmt.Sprintf("la clase %q no está en technologies: se asigna a \"Otros\"", spec.Class))
				}
				r.CIs = append(r.CIs, spec)
			}
		}
		for _, relv := range asList(rm.get("rels")) {
			l, ok := relv.([]any)
			if !ok || len(l) < 3 {
				continue
			}
			rs := RelSpec{From: fmt.Sprint(l[0]), Rel: fmt.Sprint(l[1]), To: fmt.Sprint(l[2])}
			if len(l) > 3 {
				rs.Attrs = strMap(l[3])
			}
			addRel(rs.Rel)
			r.Rels = append(r.Rels, rs)
		}
		pack := packFor(r, classTech)
		base := ruleSlug(r)
		if r.Source == "alerts" {
			base = pack + "-" + base
		}
		idCount[base]++
		r.ID = base
		if idCount[base] > 1 {
			r.ID = fmt.Sprintf("%s-%d", base, idCount[base])
		}
		r.Pack = pack
		packRules[pack] = append(packRules[pack], r)
		rep.Rules++
	}

	for _, c := range classOrder {
		o.Classes = append(o.Classes, Class{
			Name: c, Technology: classTech[c], Device: devices[c],
			SiteFromName: siteFromName[c], NavParent: navParent[c],
		})
	}
	rep.Classes = len(o.Classes)

	packNames := make([]string, 0, len(packRules))
	for p := range packRules {
		packNames = append(packNames, p)
	}
	sort.Strings(packNames)
	for _, p := range packNames {
		o.Packs = append(o.Packs, Pack{
			File: filepath.Join("packs", p+".yaml"), Name: p, Technology: packTech[p],
			Description: "Importado de " + filepath.Base(configPath) + ". Revisar y documentar cada regla.",
			Rules:       packRules[p],
		})
		rep.Packs[p] = len(packRules[p])
	}

	// --- Relaciones ---
	informative := map[string]bool{}
	for _, r := range strList(model.get("informative_relations")) {
		informative[r] = true
	}
	rf, _ := cfg.get("relations_file").(*omap)
	if rf != nil && rf.str("relation") != "" {
		addRel(rf.str("relation"))
	}
	for _, name := range usedRels {
		o.Relations = append(o.Relations, RelationType{Name: name, Causal: !informative[name], Informative: informative[name]})
	}
	o.Relations = append(o.Relations, RelationType{
		Name: SameEquipment, Identity: true,
		Description: "El mismo equipo visto por exporters distintos (nombre normalizado).",
	})

	// --- Sedes ---
	o.Sites.Names = strMap(cfg.get("site_names"))
	if sp, ok := cfg.get("site_patterns").(*omap); ok {
		for _, s := range sp.keys {
			o.Sites.Patterns = append(o.Sites.Patterns, SitePattern{Site: s, Pattern: sp.str(s)})
		}
	}
	o.Sites.IgnoreAttrs = strList(model.get("site_ignore_attrs"))

	// --- Ajustes ---
	o.Settings = Settings{
		ExcludeJobs:        strList(cfg.get("exclude_jobs")),
		SameHostClasses:    strList(model.get("same_host_types")),
		DatastoreClass:     model.str("datastore_type"),
		LunClass:           model.str("lun_type"),
		SizeMatchTolerance: 0.01,
		SSIIAttr:           "COD_SSII",
		SearchAttrs:        []string{"COD_SSII", "IP", "WWN", "Serie"},
	}
	for _, hr := range asList(cfg.get("host_rewrite")) {
		l := strList(hr)
		if len(l) != 2 {
			continue
		}
		rw, err := convertRewrite(l[0], l[1])
		if err != nil {
			rep.Warnings = append(rep.Warnings, "host_rewrite: "+err.Error())
			continue
		}
		if rw.Pattern != l[0] {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("host_rewrite: %q traducida a RE2 como %q -> %q", l[0], rw.Pattern, rw.Replace))
		}
		o.Settings.HostRewrite = append(o.Settings.HostRewrite, rw)
	}
	if b, ok := cfg.get("datastore_lun_bridge").(*omap); ok {
		br := &Bridge{
			DatastoreMetric:     b.str("datastore_metric"),
			DatastoreRegex:      b.str("datastore_regex"),
			DatastoreSizeMetric: b.str("datastore_size_metric"),
		}
		for _, sv := range asList(b.get("lun_sources")) {
			s, _ := sv.(*omap)
			if s == nil {
				continue
			}
			br.LunSources = append(br.LunSources, BridgeSource{
				Metric: s.str("metric"), Label: s.str("label"), ArrayLabel: s.str("array_label"),
				CodeMetric: s.str("code_metric"), CodeFrom: s.str("code_from"),
				CodeRegex: s.str("code_regex"), SizeMetric: s.str("size_metric"),
			})
		}
		o.Settings.DatastoreLunBridge = br
	}

	// --- Correlación (se conserva para la fase 3) ---
	for _, k := range []string{"stop_labels", "relabel", "fanout_overrides"} {
		if v := cfg.get(k); v != nil {
			o.Correlation[k] = toPlain(v)
		}
	}
	if mv := model.get("member_view"); mv != nil {
		o.Correlation["member_view"] = toPlain(mv)
	}

	// --- Relaciones manuales ---
	if rf != nil {
		o.Manual.Relation = rf.str("relation")
		o.Manual.Prefixes = map[string]ManualPrefix{}
		if pf, ok := rf.get("prefixes").(*omap); ok {
			for _, k := range pf.keys {
				pm, _ := pf.get(k).(*omap)
				if pm == nil {
					continue
				}
				o.Manual.Prefixes[k] = ManualPrefix{
					Classes:      strList(pm.get("types")),
					NamePrefixes: strList(pm.get("name_prefixes")),
					Expand:       strList(pm.get("expand")),
				}
			}
		}
		for _, f := range strList(rf.get("files")) {
			o.Manual.Files = append(o.Manual.Files, filepath.Base(f))
		}
	}
	return o, rep, nil
}

// SameEquipment es la relación de identidad entre CI de distintas tecnologías.
const SameEquipment = "es el mismo equipo que"

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func convertComputed(name string, v any) (Computed, error) {
	c := Computed{Name: name}
	switch t := v.(type) {
	case []any:
		l := strList(t)
		if len(l) != 2 {
			return c, fmt.Errorf("computed %q: se esperaba [etiqueta, regex]", name)
		}
		c.Regex = &RegexFrom{From: l[0], Pattern: l[1]}
	case *omap:
		switch {
		case t.get("sorted") != nil:
			c.Sorted = strList(t.get("sorted"))
			c.Sep = t.str("sep")
			if c.Sep == "" {
				c.Sep = "/"
			}
		case t.get("first") != nil:
			c.First = strList(t.get("first"))
			c.Pattern = t.str("regex")
		case t.get("map") != nil || t.get("map_regex") != nil:
			m := &MapFrom{From: t.str("from"), Values: strMap(t.get("map")), Default: t.str("default")}
			if mr, ok := t.get("map_regex").(*omap); ok {
				for _, p := range mr.keys {
					m.Patterns = append(m.Patterns, PatternValue{Pattern: p, Value: mr.str(p)})
				}
			}
			c.Map = m
		default:
			f := t.str("format")
			c.Format = &f
		}
	default:
		return c, fmt.Errorf("computed %q: forma desconocida", name)
	}
	return c, nil
}

var packTech = map[string]string{
	"vmware": "Virtualización", "f5": "Balanceadores", "panos": "Firewalls", "veeam": "Backups",
	"flashsystem": "Cabinas", "unity": "Cabinas", "oceanstor": "Cabinas", "otros": "Otros",
}

func packFor(r *Rule, classTech map[string]string) string {
	tech := ""
	if len(r.CIs) > 0 {
		tech = classTech[r.CIs[0].Class]
	}
	switch tech {
	case "Virtualización":
		return "vmware"
	case "Balanceadores":
		return "f5"
	case "Firewalls":
		return "panos"
	case "Backups":
		return "veeam"
	case "Cabinas":
		hint := strings.ToLower(r.Metric + " " + r.MetricRegex)
		for k, v := range r.Where {
			hint += " " + strings.ToLower(k+"="+v)
		}
		switch {
		case strings.Contains(hint, "flashsystem"):
			return "flashsystem"
		case strings.Contains(hint, "unity"):
			return "unity"
		case strings.Contains(hint, "oceanstor"):
			return "oceanstor"
		}
	}
	return "otros"
}

var slugRx = regexp.MustCompile(`[^a-z0-9]+`)

func ruleSlug(r *Rule) string {
	s := r.Metric
	if s == "" {
		s = r.MetricRegex
	}
	if s == "" {
		s = "alertas"
		keys := make([]string, 0, len(r.Where))
		for k := range r.Where {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s += "-" + k
		}
	}
	s = slugRx.ReplaceAllString(strings.ToLower(s), "-")
	return strings.Trim(s, "-")
}

// WriteDir escribe la ontología en un directorio con la estructura estándar.
func (o *Ontology) WriteDir(dir string) error {
	write := func(rel string, header string, v any) error {
		b, err := Marshal(v)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		return os.WriteFile(p, append([]byte(header), b...), 0o644)
	}
	if err := write("core/settings.yaml", "# Ajustes de descubrimiento. Ver ontology/README.md.\n", o.Settings); err != nil {
		return err
	}
	if err := write("core/classes.yaml", "# Clases de CI por tecnología.\n", map[string]any{"classes": o.Classes}); err != nil {
		return err
	}
	if err := write("core/relations.yaml", "# Tipos de relación. causal: propaga impacto del destino al origen.\n", map[string]any{"relations": o.Relations}); err != nil {
		return err
	}
	if err := write("core/sites.yaml", "# Sedes y cómo deducirlas de nombres y atributos.\n", o.Sites); err != nil {
		return err
	}
	if err := write("core/correlation.yaml", "# Parámetros de correlación heredados del correlador Python (se usan desde la fase 3).\n", o.Correlation); err != nil {
		return err
	}
	for _, p := range o.Packs {
		if err := write(filepath.Join("packs", p.Name+".yaml"), "# Reglas de "+p.Technology+".\n", p); err != nil {
			return err
		}
	}
	if o.Manual.Relation != "" {
		if err := write("business/manual_relations.yaml", "# Relaciones que Prometheus no ve. Formato de cada entrada: 'Prefijo:Nombre => Prefijo:Nombre'.\n", o.Manual); err != nil {
			return err
		}
	}
	return nil
}
