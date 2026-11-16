package engine

import (
	"regexp"
	"sort"

	"vigia/internal/contexto/ontology"
)

// Sites deduce la sede de cada CI (equivale a cmdb_sites del correlador Python):
// por nombre y atributos en las clases marcadas site_from_name; si no, heredada del
// primer padre de la misma tecnología; si tampoco, la unión de las de sus hijos.
func (e *Engine) Sites(res *Result) map[string][]string {
	type sp struct {
		site string
		rx   *regexp.Regexp
	}
	var pats []sp
	for _, p := range e.O.Sites.Patterns {
		pats = append(pats, sp{p.Site, regexp.MustCompile(p.Pattern)})
	}
	own := map[string]bool{}
	for _, c := range e.O.Classes {
		if c.SiteFromName {
			own[c.Name] = true
		}
	}
	ign := map[string]bool{}
	for _, a := range e.O.Sites.IgnoreAttrs {
		ign[a] = true
	}
	site := map[string]map[string]bool{}
	for _, key := range res.Order {
		c := res.CIs[key]
		if len(own) > 0 && !own[c.Class] {
			continue
		}
		txt := []string{c.Name}
		for k, v := range c.Attrs {
			if !ign[k] {
				txt = append(txt, v)
			}
		}
		st := map[string]bool{}
		for _, p := range pats {
			for _, t := range txt {
				if p.rx.MatchString(t) {
					st[p.site] = true
					break
				}
			}
		}
		if len(st) > 0 {
			site[key] = st
		}
	}
	out := map[string][]string{}
	inn := map[string][]string{}
	for _, r := range res.Rels {
		if r.Rel == ontology.SameEquipment {
			continue
		}
		if e.O.TechnologyOf(res.CIs[r.From].Class) == e.O.TechnologyOf(res.CIs[r.To].Class) {
			out[r.From] = append(out[r.From], r.To)
			inn[r.To] = append(inn[r.To], r.From)
		}
	}
	for pass := 0; pass < 8; pass++ {
		changed := false
		for _, i := range res.Order {
			if site[i] != nil {
				continue
			}
			for _, b := range out[i] {
				if site[b] != nil {
					site[i] = copySet(site[b])
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}
	for pass := 0; pass < 8; pass++ {
		changed := false
		for _, i := range res.Order {
			if site[i] != nil {
				continue
			}
			st := map[string]bool{}
			for _, a := range inn[i] {
				for s := range site[a] {
					st[s] = true
				}
			}
			if len(st) > 0 {
				site[i] = st
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	result := make(map[string][]string, len(site))
	for k, st := range site {
		result[k] = setList(st)
	}
	return result
}

func copySet(s map[string]bool) map[string]bool {
	o := make(map[string]bool, len(s))
	for k := range s {
		o[k] = true
	}
	return o
}

func setList(s map[string]bool) []string {
	o := make([]string, 0, len(s))
	for k := range s {
		o = append(o, k)
	}
	sort.Strings(o)
	return o
}
