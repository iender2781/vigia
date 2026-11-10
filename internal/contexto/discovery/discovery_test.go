package discovery_test

import (
	"strings"
	"testing"

	"vigia/internal/contexto/discovery"
	"vigia/internal/contexto/ontology"
)

func TestMatchersAndValues(t *testing.T) {
	o, err := ontology.Load("../../../ontology")
	if err != nil {
		t.Fatal(err)
	}
	ms := discovery.Matchers(o)
	if len(ms) == 0 {
		t.Fatal("sin selectores")
	}
	for _, m := range ms {
		if !strings.Contains(m, `job!~"acosv2"`) {
			t.Errorf("el selector no excluye acosv2: %s", m)
		}
	}
	joined := strings.Join(ms, "\n")
	for _, want := range []string{`{__name__="vmware_vm_power_state"`, `{__name__=~"f5os_(memory|cpu_utilization)_.*"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("falta el selector %s", want)
		}
	}
	vals := strings.Join(discovery.ValueMetrics(o), ",")
	for _, want := range []string{"vmware_vm_power_state", "veeam_service_state", "vsphere_datastore_capacity_bytes"} {
		if !strings.Contains(vals, want) {
			t.Errorf("faltan valores de %s: %s", want, vals)
		}
	}
}
