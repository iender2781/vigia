package ontology

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/goccy/go-yaml"
)

// Load lee la ontología de un directorio y la valida. Devuelve error si hay
// cualquier problema bloqueante (fichero mal formado, regla incoherente, regex inválida).
func Load(dir string) (*Ontology, error) {
	o := &Ontology{Dir: dir}
	core := filepath.Join(dir, "core")
	if err := readYAML(filepath.Join(core, "settings.yaml"), &o.Settings, true); err != nil {
		return nil, err
	}
	var classes struct {
		Classes []Class `yaml:"classes"`
	}
	if err := readYAML(filepath.Join(core, "classes.yaml"), &classes, true); err != nil {
		return nil, err
	}
	o.Classes = classes.Classes
	var rels struct {
		Relations []RelationType `yaml:"relations"`
	}
	if err := readYAML(filepath.Join(core, "relations.yaml"), &rels, true); err != nil {
		return nil, err
	}
	o.Relations = rels.Relations
	if err := readYAML(filepath.Join(core, "sites.yaml"), &o.Sites, true); err != nil {
		return nil, err
	}
	o.Correlation = map[string]any{}
	if err := readYAML(filepath.Join(core, "correlation.yaml"), &o.Correlation, false); err != nil {
		return nil, err
	}

	packFiles, _ := filepath.Glob(filepath.Join(dir, "packs", "*.yaml"))
	sort.Strings(packFiles)
	for _, f := range packFiles {
		var p Pack
		if err := readYAML(f, &p, true); err != nil {
			return nil, err
		}
		p.File = f
		for _, r := range p.Rules {
			r.Pack = p.Name
		}
		o.Packs = append(o.Packs, p)
	}

	mr := filepath.Join(dir, "business", "manual_relations.yaml")
	if _, err := os.Stat(mr); err == nil {
		if err := readYAML(mr, &o.Manual, true); err != nil {
			return nil, err
		}
		o.Manual.File = mr
	}

	if errs := o.Validate(); len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = "  - " + e.Error()
		}
		return nil, fmt.Errorf("ontología no válida (%s):\n%s", dir, strings.Join(msgs, "\n"))
	}
	o.Index()
	return o, nil
}

func readYAML(path string, v any, required bool) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if !required && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("leyendo %s: %w", path, err)
	}
	if err := yaml.UnmarshalWithOptions(data, v, yaml.Strict()); err != nil {
		return fmt.Errorf("%s: %s", path, yaml.FormatError(err, false, true))
	}
	return nil
}

// Index ordena las reglas y prepara los índices de clases y relaciones.
func (o *Ontology) Index() {
	o.Rules = o.Rules[:0]
	for i := range o.Packs {
		o.Rules = append(o.Rules, o.Packs[i].Rules...)
	}
	// Orden estable: primero las que tienen Order (ascendente), luego el resto en orden de fichero.
	sort.SliceStable(o.Rules, func(i, j int) bool {
		a, b := o.Rules[i].Order, o.Rules[j].Order
		if a == 0 || b == 0 {
			return a != 0 && b == 0
		}
		return a < b
	})
	o.classByName = map[string]*Class{}
	for i := range o.Classes {
		o.classByName[o.Classes[i].Name] = &o.Classes[i]
	}
	o.relByName = map[string]*RelationType{}
	for i := range o.Relations {
		o.relByName[o.Relations[i].Name] = &o.Relations[i]
	}
}

// Class devuelve la clase por nombre (nil si no existe).
func (o *Ontology) Class(name string) *Class { return o.classByName[name] }

// Relation devuelve el tipo de relación por nombre (nil si no existe).
func (o *Ontology) Relation(name string) *RelationType { return o.relByName[name] }

// TechnologyOf devuelve la tecnología de una clase ("" si no está declarada).
func (o *Ontology) TechnologyOf(class string) string {
	if c := o.classByName[class]; c != nil {
		return c.Technology
	}
	return ""
}

// Technologies devuelve las tecnologías en orden de declaración.
func (o *Ontology) Technologies() []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range o.Classes {
		if !seen[c.Technology] {
			seen[c.Technology] = true
			out = append(out, c.Technology)
		}
	}
	return out
}

// Validate comprueba la coherencia de la ontología y devuelve todos los errores encontrados.
func (o *Ontology) Validate() []error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	checkRx := func(where, rx string) {
		if _, err := regexp.Compile(rx); err != nil {
			add("%s: expresión regular no válida %q: %v", where, rx, err)
		}
	}

	classes := map[string]bool{}
	for _, c := range o.Classes {
		if c.Name == "" || c.Technology == "" {
			add("core/classes.yaml: cada clase necesita name y technology (%+v)", c)
		}
		if classes[c.Name] {
			add("core/classes.yaml: clase duplicada %q", c.Name)
		}
		classes[c.Name] = true
	}
	for _, c := range o.Classes {
		if c.NavParent != "" && !classes[c.NavParent] {
			add("core/classes.yaml: %q tiene nav_parent %q, que no es una clase", c.Name, c.NavParent)
		}
	}
	relTypes := map[string]bool{}
	for _, r := range o.Relations {
		if relTypes[r.Name] {
			add("core/relations.yaml: relación duplicada %q", r.Name)
		}
		relTypes[r.Name] = true
		if r.Causal && r.Informative {
			add("core/relations.yaml: %q no puede ser causal e informativa a la vez", r.Name)
		}
	}
	for _, sp := range o.Sites.Patterns {
		checkRx("core/sites.yaml "+sp.Site, sp.Pattern)
	}
	for _, rw := range o.Settings.HostRewrite {
		checkRx("core/settings.yaml host_rewrite", rw.Pattern)
	}
	for _, c := range o.Settings.SameHostClasses {
		if !classes[c] {
			add("core/settings.yaml same_host_classes: %q no es una clase", c)
		}
	}
	if b := o.Settings.DatastoreLunBridge; b != nil {
		checkRx("core/settings.yaml datastore_lun_bridge", b.DatastoreRegex)
		for _, s := range b.LunSources {
			if s.CodeRegex != "" {
				checkRx("core/settings.yaml datastore_lun_bridge.lun_sources", s.CodeRegex)
			}
		}
		if !classes[o.Settings.DatastoreClass] || !classes[o.Settings.LunClass] {
			add("core/settings.yaml: datastore_class y lun_class deben ser clases declaradas")
		}
	}

	ids := map[string]string{}
	for _, p := range o.Packs {
		file := filepath.Base(p.File)
		if p.Name == "" {
			add("%s: falta el campo pack", file)
		}
		for i, r := range p.Rules {
			where := fmt.Sprintf("%s regla %d", file, i+1)
			if r.ID != "" {
				where = fmt.Sprintf("%s regla %q", file, r.ID)
				if prev, ok := ids[r.ID]; ok {
					add("%s: id repetido (también en %s)", where, prev)
				}
				ids[r.ID] = file
			} else {
				add("%s: falta id", where)
			}
			n := 0
			if r.Metric != "" {
				n++
			}
			if r.MetricRegex != "" {
				n++
				checkRx(where, r.MetricRegex)
			}
			if r.Source != "" {
				n++
				if r.Source != "alerts" {
					add("%s: source solo admite \"alerts\"", where)
				}
			}
			if n != 1 {
				add("%s: indica exactamente uno de metric, metric_regex o source", where)
			}
			for lab, rx := range r.Where {
				checkRx(where+" where."+lab, rx)
			}
			for _, c := range r.Computed {
				forms := 0
				if c.Regex != nil {
					forms++
					checkRx(where+" computed."+c.Name, c.Regex.Pattern)
				}
				if c.Format != nil {
					forms++
				}
				if len(c.First) > 0 {
					forms++
					if c.Pattern != "" {
						checkRx(where+" computed."+c.Name, c.Pattern)
					}
				}
				if c.Map != nil {
					forms++
					for _, pv := range c.Map.Patterns {
						checkRx(where+" computed."+c.Name, pv.Pattern)
					}
				}
				if len(c.Sorted) > 0 {
					forms++
				}
				if c.Name == "" || forms != 1 {
					add("%s: computed %q necesita nombre y exactamente una forma (regex, format, first, map, sorted)", where, c.Name)
				}
			}
			aliases := map[string]bool{}
			if len(r.CIs) == 0 {
				add("%s: la regla no define ningún CI", where)
			}
			for _, c := range r.CIs {
				if c.Alias == "" || len(c.Key) == 0 {
					add("%s: cada CI necesita alias y key", where)
					continue
				}
				if aliases[c.Alias] {
					add("%s: alias repetido %q", where, c.Alias)
				}
				aliases[c.Alias] = true
				if !classes[c.Class] {
					add("%s: la clase %q no está declarada en core/classes.yaml", where, c.Class)
				}
			}
			for _, rs := range r.Rels {
				if !aliases[rs.From] || !aliases[rs.To] {
					add("%s: la relación %s -[%s]-> %s usa un alias que no existe", where, rs.From, rs.Rel, rs.To)
				}
				if !relTypes[rs.Rel] {
					add("%s: el tipo de relación %q no está en core/relations.yaml", where, rs.Rel)
				}
			}
			if r.Attach != "" && !aliases[r.Attach] {
				add("%s: attach %q no es un alias de la regla", where, r.Attach)
			}
		}
	}
	if o.Manual.Relation != "" {
		if !relTypes[o.Manual.Relation] {
			add("business/manual_relations.yaml: la relación %q no está en core/relations.yaml", o.Manual.Relation)
		}
		for name, p := range o.Manual.Prefixes {
			for _, c := range p.Classes {
				if !classes[c] {
					add("business/manual_relations.yaml: el prefijo %s usa la clase %q, que no existe", name, c)
				}
			}
		}
	}
	return errs
}

// Marshal serializa un valor a YAML con el estilo de la ontología.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf, yaml.Indent(2), yaml.IndentSequence(true), yaml.UseLiteralStyleIfMultiline(true))
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ManualFiles devuelve las rutas de los ficheros heredados de relaciones manuales
// (relativas a business/).
func (o *Ontology) ManualFiles() []string {
	var out []string
	for _, f := range o.Manual.Files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(o.Dir, "business", f)
		}
		out = append(out, f)
	}
	return out
}

// ManualEntries devuelve el nombre del fichero YAML y sus entradas de relaciones manuales.
func (o *Ontology) ManualEntries() (string, []string) {
	return "manual_relations.yaml", o.Manual.Entries
}
