package engine

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ManualLine es una relación manual "Prefijo:Nombre => Prefijo:Nombre".
type ManualLine struct {
	Source string // fichero de origen (para el método de la relación y los avisos)
	N      int
	Line   string
	LP, LN string
	RP, RN string
}

var manualRx = regexp.MustCompile(`^\s*([^:=]+?)\s*:\s*(.+?)\s*=>\s*([^:=]+?)\s*:\s*(.+?)\s*$`)

func parseManual(source string, n int, line string) (ManualLine, bool) {
	line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
	if line == "" {
		return ManualLine{}, false
	}
	m := manualRx.FindStringSubmatch(line)
	if m == nil {
		return ManualLine{Source: source, N: n, Line: line}, true
	}
	return ManualLine{Source: source, N: n, Line: line, LP: m[1], LN: m[2], RP: m[3], RN: m[4]}, true
}

// LoadManualLines lee los ficheros heredados (relaciones.txt) y las entradas del YAML.
// Un fichero que no existe se ignora con aviso: se informa en el informe de calidad.
func LoadManualLines(o interface {
	ManualFiles() []string
	ManualEntries() (string, []string)
}) ([]ManualLine, error) {
	var out []ManualLine
	for _, path := range o.ManualFiles() {
		f, err := os.Open(path)
		if err != nil {
			out = append(out, ManualLine{Source: filepath.Base(path), Line: "no se puede leer el fichero: " + err.Error()})
			continue
		}
		sc := bufio.NewScanner(f)
		n := 0
		for sc.Scan() {
			n++
			t := strings.TrimPrefix(sc.Text(), "\uFEFF")
			if ml, ok := parseManual(filepath.Base(path), n, t); ok {
				out = append(out, ml)
			}
		}
		f.Close()
	}
	src, entries := o.ManualEntries()
	for i, e := range entries {
		if ml, ok := parseManual(src, i+1, e); ok {
			out = append(out, ml)
		}
	}
	return out, nil
}

// applyManual añade las relaciones manuales (equivale a apply_relations_files):
// el lado derecho depende del izquierdo; con "expand" se enlazan los descendientes
// del destino y, cuando ambos lados tienen sede, solo dentro de la misma sede.
func (e *Engine) applyManual(res *Result, sitesList map[string][]string) {
	mr := e.O.Manual
	if mr.Relation == "" || len(e.ManualLines) == 0 {
		return
	}
	prefixes := map[string]struct {
		classes, namePrefixes, expand []string
	}{}
	for k, p := range mr.Prefixes {
		prefixes[strings.ToLower(k)] = struct{ classes, namePrefixes, expand []string }{p.Classes, p.NamePrefixes, p.Expand}
	}
	techClass := map[string]bool{}
	for _, c := range e.O.Classes {
		techClass[c.Name] = true
	}
	site := map[string]map[string]bool{}
	for k, l := range sitesList {
		s := map[string]bool{}
		for _, x := range l {
			s[x] = true
		}
		site[k] = s
	}
	byClass := map[string]map[string][]string{}
	for _, key := range res.Order {
		c := res.CIs[key]
		if byClass[c.Class] == nil {
			byClass[c.Class] = map[string][]string{}
		}
		n := strings.ToLower(c.Name)
		byClass[c.Class][n] = append(byClass[c.Class][n], key)
	}
	incoming := map[[2]string][]string{}
	for _, r := range res.Rels {
		k := [2]string{r.To, r.Rel}
		incoming[k] = append(incoming[k], r.From)
	}

	resolve := func(prefix, name string) ([]string, bool, string) {
		st := ""
		if i := strings.Index(name, "@"); i >= 0 {
			name, st = name[:i], name[i+1:]
		}
		p, ok := prefixes[strings.ToLower(prefix)]
		if !ok && techClass[prefix] {
			p.classes, ok = []string{prefix}, true
		}
		if !ok {
			return nil, false, fmt.Sprintf("prefijo desconocido '%s'", prefix)
		}
		cand := []string{strings.ToLower(name)}
		for _, pre := range p.namePrefixes {
			cand = append(cand, strings.ToLower(pre+name))
		}
		var ids []string
		for _, t := range p.classes {
			for _, nm := range cand {
				ids = append(ids, byClass[t][nm]...)
			}
		}
		if st != "" {
			up := strings.ToUpper(st)
			var f []string
			for _, i := range ids {
				if s, has := site[i]; !has || s[up] {
					f = append(f, i)
				}
			}
			ids = f
		}
		if len(ids) == 0 {
			return nil, false, fmt.Sprintf("no hay ningún %s llamado '%s'", strings.Join(p.classes, "/"), name)
		}
		var resIDs []string
		for _, r := range p.expand {
			stack := append([]string(nil), ids...)
			for len(stack) > 0 {
				x := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				for _, y := range incoming[[2]string{x, r}] {
					if !contains(resIDs, y) {
						resIDs = append(resIDs, y)
						stack = append(stack, y)
					}
				}
			}
		}
		if len(resIDs) > 0 {
			return resIDs, true, ""
		}
		return ids, false, ""
	}

	for _, l := range e.ManualLines {
		if l.N == 0 {
			res.Stats.ManualErrors = append(res.Stats.ManualErrors, l.Source+": "+l.Line)
			continue
		}
		if l.LP == "" {
			res.Stats.ManualErrors = append(res.Stats.ManualErrors, fmt.Sprintf("%s:%d: línea no válida (formato Tipo:Nombre => Tipo:Nombre): %s", l.Source, l.N, l.Line))
			continue
		}
		left, _, e1 := resolve(l.LP, l.LN)
		right, expanded, e2 := resolve(l.RP, l.RN)
		if e1 != "" || e2 != "" {
			msg := e1
			if msg == "" {
				msg = e2
			}
			res.Stats.ManualErrors = append(res.Stats.ManualErrors, fmt.Sprintf("%s:%d: %s (%s)", l.Source, l.N, msg, l.Line))
			continue
		}
		res.Stats.ManualApplied++
		for _, b := range right {
			for _, a := range left {
				sa, sb := site[a], site[b]
				if len(sa) > 0 && len(sb) > 0 {
					if expanded && !subset(sb, sa) {
						continue
					}
					if !expanded && !intersects(sa, sb) {
						continue
					}
				}
				res.rel(b, mr.Relation, a, "fichero "+l.Source)
				res.Stats.ManualRels++
			}
		}
	}
}

func subset(a, b map[string]bool) bool {
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func intersects(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}
