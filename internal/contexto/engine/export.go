package engine

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WriteCSV escribe cmdb_ci.csv y cmdb_relaciones.csv con el mismo formato que
// --cmdb-export del correlador Python, para poder compararlos.
func (r *Result) WriteCSV(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(dir, "cmdb_ci.csv"))
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Comma, w.UseCRLF = ';', true
	_ = w.Write([]string{"id", "tipo", "nombre", "atributos", "metricas"})
	keys := append([]string(nil), r.Order...)
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := r.CIs[keys[i]], r.CIs[keys[j]]
		if a.Class != b.Class {
			return a.Class < b.Class
		}
		return a.Name < b.Name
	})
	for _, k := range keys {
		c := r.CIs[k]
		attrs := make([]string, 0, len(c.AttrOrder))
		for _, a := range c.AttrOrder {
			attrs = append(attrs, a+"="+c.Attrs[a])
		}
		_ = w.Write([]string{c.Key, c.Class, c.Name, strings.Join(attrs, " | "), strings.Join(c.Sources, ",")})
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	f.Close()

	f, err = os.Create(filepath.Join(dir, "cmdb_relaciones.csv"))
	if err != nil {
		return err
	}
	defer f.Close()
	w = csv.NewWriter(f)
	w.Comma, w.UseCRLF = ';', true
	_ = w.Write([]string{"origen_tipo", "origen", "relacion", "destino_tipo", "destino", "metodo", "atributos"})
	for _, rel := range r.Rels {
		a, b := r.CIs[rel.From], r.CIs[rel.To]
		var attrs []string
		ks := make([]string, 0, len(rel.Attrs))
		for k := range rel.Attrs {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			attrs = append(attrs, k+"="+rel.Attrs[k])
		}
		_ = w.Write([]string{a.Class, a.Name, rel.Rel, b.Class, b.Name, rel.Method, strings.Join(attrs, " | ")})
	}
	w.Flush()
	return w.Error()
}

// WriteJSON escribe la instantánea completa en JSON.
func (r *Result) WriteJSON(path string) error {
	out := struct {
		CIs   []*CI       `json:"cis"`
		Rels  []*Relation `json:"relations"`
		Stats Stats       `json:"stats"`
	}{Rels: r.Rels, Stats: r.Stats}
	for _, k := range r.Order {
		out.CIs = append(out.CIs, r.CIs[k])
	}
	b, err := json.MarshalIndent(out, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}
