package engine_test

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"vigia/internal/contexto/engine"
	"vigia/internal/contexto/ontology"
	"vigia/internal/contexto/prom"
)

// Las salidas de referencia (testdata/golden/python) las genera el correlador Python con
// tools/reference/py_cmdb.py sobre la captura sintética de testdata/fixture.

func loadFixture(t *testing.T) engine.Input {
	t.Helper()
	var in engine.Input
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", "fixture", name))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	var err error
	if in.Series, err = prom.ParseSeries(read("series.json")); err != nil {
		t.Fatal(err)
	}
	if in.Values, err = prom.ParseValues(read("values.json")); err != nil {
		t.Fatal(err)
	}
	alerts, err := prom.ParseAlerts(read("alerts.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range alerts {
		in.Alerts = append(in.Alerts, a.Labels)
	}
	return in
}

func legacyEngine(t *testing.T) *engine.Engine {
	t.Helper()
	o, _, err := ontology.ImportLegacy("../../../reference/config_junta.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := o.WriteDir(dir); err != nil {
		t.Fatal(err)
	}
	rel, _ := os.ReadFile(filepath.Join("testdata", "fixture", "relaciones.txt"))
	if err := os.WriteFile(filepath.Join(dir, "business", "relaciones.txt"), rel, 0o644); err != nil {
		t.Fatal(err)
	}
	o, err = ontology.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(o)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func readCSV(t *testing.T, path string) [][]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = ';'
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows[1:]
}

func normAttrs(class, s string, ignore map[string]bool) string {
	var parts []string
	for _, a := range strings.Split(s, " | ") {
		if a == "" || ignore[class+":"+strings.SplitN(a, "=", 2)[0]] {
			continue
		}
		parts = append(parts, a)
	}
	sort.Strings(parts)
	return strings.Join(parts, " | ")
}

// TestEquivalenceWithPython comprueba que el motor produce la misma CMDB que el correlador
// Python, salvo las dos mejoras documentadas.
func TestEquivalenceWithPython(t *testing.T) {
	e := legacyEngine(t)
	res := e.Build(loadFixture(t))
	out := t.TempDir()
	if err := res.WriteCSV(out); err != nil {
		t.Fatal(err)
	}
	ignore := map[string]bool{"Servicio Veeam:Estado": true}
	ciKey := func(rows [][]string) map[string]string {
		m := map[string]string{}
		for _, r := range rows {
			m[r[0]] = strings.Join([]string{r[1], r[2], normAttrs(r[1], r[3], ignore), r[4]}, " ;; ")
		}
		return m
	}
	want := ciKey(readCSV(t, filepath.Join("testdata", "golden", "python", "cmdb_ci.csv")))
	got := ciKey(readCSV(t, filepath.Join(out, "cmdb_ci.csv")))
	for k, w := range want {
		if got[k] != w {
			t.Errorf("CI %s\n  python: %s\n  go:     %s", k, w, got[k])
		}
	}
	for k := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("CI %s solo en Go", k)
		}
	}
	relSet := func(rows [][]string) map[string]bool {
		m := map[string]bool{}
		for _, r := range rows {
			r[6] = normAttrs("", r[6], nil)
			m[strings.Join(r, ";")] = true
		}
		return m
	}
	wantR := relSet(readCSV(t, filepath.Join("testdata", "golden", "python", "cmdb_relaciones.csv")))
	gotR := relSet(readCSV(t, filepath.Join(out, "cmdb_relaciones.csv")))
	for k := range wantR {
		if !gotR[k] {
			t.Errorf("relación solo en Python: %s", k)
		}
	}
	extra := 0
	for k := range gotR {
		if !wantR[k] {
			if strings.Contains(k, ";nombre del datastore;") {
				extra++ // mejora: desempate por capacidad en el puente datastore -> LUN
				continue
			}
			t.Errorf("relación solo en Go: %s", k)
		}
	}
	if extra != 1 {
		t.Errorf("se esperaba 1 enlace datastore -> LUN extra por capacidad, hay %d", extra)
	}
}

func TestRepositoryOntologyLoadsAndExtracts(t *testing.T) {
	o, err := ontology.Load("../../../ontology")
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(o)
	if err != nil {
		t.Fatal(err)
	}
	in := engine.Input{
		Series: []engine.Labels{{"__name__": "vmware_vm_power_state", "job": "vmware_vcnppu01",
			"instance": "vcnppu01.junta.es:443", "vm_name": "ccobuz38", "host_name": "s00e-hses-48",
			"cluster_name": "CL-PROD", "ds_name": "DS1", "dc_name": "DC", "tags": "backup, CJDA-CCOR-001, linux"}},
		Values: []engine.Value{{Labels: engine.Labels{"__name__": "vmware_vm_power_state", "job": "vmware_vcnppu01",
			"instance": "vcnppu01.junta.es:443", "vm_name": "ccobuz38", "host_name": "s00e-hses-48",
			"cluster_name": "CL-PROD", "ds_name": "DS1", "dc_name": "DC", "tags": "backup, CJDA-CCOR-001, linux"}, Value: 1}},
	}
	res := e.Build(in)
	vm := res.CIs["VM|vcnppu01|ccobuz38"]
	if vm == nil {
		t.Fatalf("no se creó la VM: %v", res.Order)
	}
	if vm.Attrs["COD_SSII"] != "CJDA-CCOR-001" || vm.Attrs["Estado"] != "encendida" {
		t.Errorf("atributos inesperados: %v", vm.Attrs)
	}
	if vm.Tech != "Virtualización" {
		t.Errorf("tecnología: %q", vm.Tech)
	}
}

func TestNormValue(t *testing.T) {
	cases := map[string]string{
		"naa.6008C0E1AABBCCDD0011223344556677": "6008c0e1aabbccdd0011223344556677",
		"10.47.208.11:9100":                    "10.47.208.11",
		"https://vcnppu01.junta.es:443/sdk":    "vcnppu01.junta.es",
		"Host-01:9100":                         "host-01",
		"[fe80::1]:9100":                       "fe80::1",
	}
	for in, want := range cases {
		if got := engine.NormValue(in); got != want {
			t.Errorf("NormValue(%q) = %q, se esperaba %q", in, got, want)
		}
	}
	if engine.StripDomain("s00e-hses-48.junta.es") != "s00e-hses-48" || engine.StripDomain("10.1.1.1") != "10.1.1.1" {
		t.Error("StripDomain")
	}
}
