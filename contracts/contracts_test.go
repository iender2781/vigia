package contracts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Qué esquema y qué tipo Go corresponden a cada tipo de evento.
var registry = map[string]struct {
	schema string
	value  func() any
}{
	TypeAlertFired:         {"alert.v1.schema.json", func() any { return &Alert{} }},
	TypeAlertResolved:      {"alert.v1.schema.json", func() any { return &Alert{} }},
	TypeAlertEnriched:      {"ctx.alert.enriched.v1.schema.json", func() any { return &AlertEnriched{} }},
	TypeIncidentOpened:     {"ctx.incident.v1.schema.json", func() any { return &Incident{} }},
	TypeIncidentUpdated:    {"ctx.incident.v1.schema.json", func() any { return &Incident{} }},
	TypeIncidentResolved:   {"ctx.incident.v1.schema.json", func() any { return &Incident{} }},
	TypeSSIIState:          {"ctx.ssii.state.v1.schema.json", func() any { return &SSIIState{} }},
	TypeAlertAcked:         {"feedback.alert.v1.schema.json", func() any { return &AlertFeedback{} }},
	TypeAlertClosed:        {"feedback.alert.v1.schema.json", func() any { return &AlertFeedback{} }},
	TypeRootCause:          {"feedback.root_cause.v1.schema.json", func() any { return &RootCause{} }},
	TypeMaintenanceStarted: {"feedback.maintenance.v1.schema.json", func() any { return &Maintenance{} }},
	TypeMaintenanceEnded:   {"feedback.maintenance.v1.schema.json", func() any { return &Maintenance{} }},
}

type schema struct {
	Required   []string                   `json:"required"`
	Properties map[string]json.RawMessage `json:"properties"`
}

func loadSchema(t *testing.T, name string) schema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("schemas", name))
	if err != nil {
		t.Fatal(err)
	}
	var s schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s
}

func jsonFields(v any) []string {
	tp := reflect.TypeOf(v).Elem()
	var out []string
	for i := 0; i < tp.NumField(); i++ {
		name := strings.Split(tp.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Los esquemas JSON y los tipos Go describen los mismos campos.
func TestSchemasMatchGoTypes(t *testing.T) {
	for typ, reg := range registry {
		s := loadSchema(t, reg.schema)
		var props []string
		for p := range s.Properties {
			props = append(props, p)
		}
		sort.Strings(props)
		if got := jsonFields(reg.value()); !reflect.DeepEqual(props, got) {
			t.Errorf("%s: el esquema %s tiene %v y el tipo Go %v", typ, reg.schema, props, got)
		}
		if _, err := Subject(typ, "t"); err != nil {
			t.Error(err)
		}
	}
	env := loadSchema(t, "cloudevent.schema.json")
	var props []string
	for p := range env.Properties {
		props = append(props, p)
	}
	sort.Strings(props)
	if got := jsonFields(&Event{}); !reflect.DeepEqual(props, got) {
		t.Errorf("sobre: esquema %v, Go %v", props, got)
	}
}

// Los ejemplos se leen sin campos desconocidos y traen todos los obligatorios.
func TestExamples(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join("examples", "*.json"))
	if len(files) == 0 {
		t.Fatal("no hay ejemplos")
	}
	env := loadSchema(t, "cloudevent.schema.json")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		var ev Event
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&ev); err != nil {
			t.Errorf("%s: sobre: %v", f, err)
			continue
		}
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(b, &raw)
		for _, r := range env.Required {
			if _, ok := raw[r]; !ok {
				t.Errorf("%s: falta %s en el sobre", f, r)
			}
		}
		reg, ok := registry[ev.Type]
		if !ok {
			t.Errorf("%s: tipo desconocido %s", f, ev.Type)
			continue
		}
		v := reg.value()
		dec = json.NewDecoder(bytes.NewReader(ev.Data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(v); err != nil {
			t.Errorf("%s: data: %v", f, err)
		}
		var data map[string]json.RawMessage
		_ = json.Unmarshal(ev.Data, &data)
		for _, r := range loadSchema(t, reg.schema).Required {
			if _, ok := data[r]; !ok {
				t.Errorf("%s: falta el campo obligatorio %s", f, r)
			}
		}
	}
}
