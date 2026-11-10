// Command helmcheck renderiza el chart con un subconjunto de las funciones de Helm y
// comprueba que cada documento es YAML válido con apiVersion, kind y metadata.name.
// Es una comprobación rápida para entornos sin Helm; el CI usa además "helm lint" y
// "helm template" reales.
//
//	go run ./tools/helmcheck deploy/helm/vigia-contexto [valores-extra.yaml ...]
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/goccy/go-yaml"
)

func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

func readYAML(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		fail("%v", err)
	}
	m := map[string]any{}
	if err := yaml.Unmarshal(b, &m); err != nil {
		fail("%s: %v", path, err)
	}
	return m
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "ERROR: "+f+"\n", a...)
	os.Exit(1)
}

func toInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case uint64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

func main() {
	if len(os.Args) < 2 {
		fail("uso: helmcheck DIR_CHART [valores.yaml ...]")
	}
	dir := os.Args[1]
	chart := readYAML(filepath.Join(dir, "Chart.yaml"))
	values := readYAML(filepath.Join(dir, "values.yaml"))
	for _, extra := range os.Args[2:] {
		merge(values, readYAML(extra))
	}
	var tpl *template.Template
	funcs := template.FuncMap{
		"include": func(name string, data any) (string, error) {
			var b bytes.Buffer
			err := tpl.ExecuteTemplate(&b, name, data)
			return b.String(), err
		},
		"toYaml": func(v any) string {
			b, _ := yaml.Marshal(v)
			return strings.TrimSuffix(string(b), "\n")
		},
		"nindent": func(n int, s string) string {
			pad := strings.Repeat(" ", n)
			return "\n" + pad + strings.ReplaceAll(s, "\n", "\n"+pad)
		},
		"quote":     func(v any) string { return fmt.Sprintf("%q", fmt.Sprint(v)) },
		"default":   func(d, v any) any { if v == nil || v == "" || v == false { return d }; return v },
		"join":      func(sep string, v any) string { var s []string; if l, ok := v.([]any); ok { for _, x := range l { s = append(s, fmt.Sprint(x)) } }; return strings.Join(s, sep) },
		"sha256sum": func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) },
		"list":      func(v ...any) []any { return v },
		"int":       toInt,
	}
	tpl = template.New("chart").Funcs(funcs).Option("missingkey=zero")
	files, _ := filepath.Glob(filepath.Join(dir, "templates", "*"))
	sort.Strings(files)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := tpl.New("templates/" + filepath.Base(f)).Parse(string(b)); err != nil {
			fail("%s: %v", f, err)
		}
	}
	ctx := map[string]any{
		"Values":   values,
		"Chart":    map[string]any{"Name": chart["name"], "Version": chart["version"], "AppVersion": chart["appVersion"]},
		"Release":  map[string]any{"Name": "vigia-contexto", "Namespace": "vigia-app", "Service": "Helm", "Revision": 1},
		"Template": map[string]any{"BasePath": "templates"},
	}
	docs := 0
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasPrefix(base, "_") || strings.HasSuffix(base, ".txt") {
			continue
		}
		var out bytes.Buffer
		if err := tpl.ExecuteTemplate(&out, "templates/"+base, ctx); err != nil {
			fail("%s: %v", base, err)
		}
		for i, doc := range strings.Split(out.String(), "\n---") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			m := map[string]any{}
			if err := yaml.Unmarshal([]byte(doc), &m); err != nil {
				fail("%s (documento %d): YAML no válido: %v\n%s", base, i+1, err, doc)
			}
			if len(m) == 0 {
				continue
			}
			md, _ := m["metadata"].(map[string]any)
			if m["apiVersion"] == nil || m["kind"] == nil || md == nil || md["name"] == nil {
				fail("%s (documento %d): falta apiVersion, kind o metadata.name", base, i+1)
			}
			docs++
			if os.Getenv("HELMCHECK_PRINT") != "" {
				fmt.Printf("---\n# %s\n%s\n", base, strings.TrimSpace(doc))
			} else {
				fmt.Printf("ok  %-22s %s/%s\n", base, m["kind"], md["name"])
			}
		}
	}
	fmt.Printf("%d documentos válidos\n", docs)
}
