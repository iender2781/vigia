package ontology

import (
	"strings"
	"testing"
)

func TestValidateDetectsErrors(t *testing.T) {
	o := &Ontology{
		Classes:   []Class{{Name: "VM", Technology: "Virtualización"}},
		Relations: []RelationType{{Name: "usa", Causal: true}},
		Packs: []Pack{{File: "x.yaml", Name: "x", Rules: []*Rule{
			{ID: "a", Metric: "m", MetricRegex: "n", CIs: []CISpec{{Alias: "vm", Class: "VM", Key: []string{"k"}}}},
			{ID: "b", Metric: "m", Where: map[string]string{"job": "(sin cerrar"},
				CIs:  []CISpec{{Alias: "vm", Class: "Desconocida", Key: []string{"k"}}},
				Rels: []RelSpec{{From: "vm", Rel: "no-existe", To: "otro"}}},
			{ID: "b", Metric: "m", CIs: []CISpec{{Alias: "vm", Class: "VM", Key: []string{"k"}}}},
		}}},
	}
	var msgs []string
	for _, e := range o.Validate() {
		msgs = append(msgs, e.Error())
	}
	all := strings.Join(msgs, "\n")
	for _, want := range []string{"exactamente uno de metric", "expresión regular no válida", "no está declarada",
		"usa un alias que no existe", "no está en core/relations.yaml", "id repetido"} {
		if !strings.Contains(all, want) {
			t.Errorf("no se detectó %q en:\n%s", want, all)
		}
	}
}

func TestConvertRewrite(t *testing.T) {
	rw, err := convertRewrite(`^[a-z]{2,6}_(?=[a-z0-9-]+$)`, "")
	if err != nil {
		t.Fatal(err)
	}
	if rw.Pattern != `^[a-z]{2,6}_([a-z0-9-]+)$` || rw.Replace != "${1}" {
		t.Errorf("traducción inesperada: %+v", rw)
	}
}
