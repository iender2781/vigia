package engine

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

// Puente datastore (vSphere) -> LUN (cabina) por convención de nombres y capacidad.
// Equivale a build_datastore_lun_bridge del correlador Python.

type sizeKey struct{ a, b string }

func sizeIndex(values []Value, metric string, k1, k2 string) map[sizeKey]float64 {
	idx := map[sizeKey]float64{}
	for _, v := range values {
		if v.Labels["__name__"] != metric {
			continue
		}
		a, ok1 := v.Labels[k1]
		b, ok2 := v.Labels[k2]
		if ok1 && ok2 && !math.IsNaN(v.Value) && v.Value > 0 {
			idx[sizeKey{a, b}] = v.Value
		}
	}
	return idx
}

func sameSize(a, b, tol float64) bool {
	return a != 0 && b != 0 && math.Abs(a-b) <= math.Max(tol*math.Max(a, b), 1<<30)
}

// BridgeLink es un datastore enlazado con su LUN.
type BridgeLink struct {
	VCenter, Datastore, Array, LUN, Method string
}

// Bridge calcula los enlaces datastore -> LUN y sus estadísticas.
func (e *Engine) Bridge(series []Labels, values []Value) ([]BridgeLink, map[string]int) {
	b := e.O.Settings.DatastoreLunBridge
	stats := map[string]int{}
	if b == nil {
		return nil, stats
	}
	tol := e.O.Settings.SizeMatchTolerance
	if tol == 0 {
		tol = 0.01
	}
	rx := start(b.DatastoreRegex)
	code2array := map[string]string{}
	luns := map[string]map[string]bool{}
	lunSize := map[sizeKey]float64{}
	dsMetric := b.DatastoreSizeMetric
	if dsMetric == "" {
		dsMetric = "vsphere_datastore_capacity_bytes"
	}
	dsSize := sizeIndex(values, dsMetric, "vcenter", "datastore")
	codeOf := func(crx *regexp.Regexp, v string) string {
		m := crx.FindStringSubmatch(v)
		if m == nil {
			return ""
		}
		return strings.ToUpper(strings.Join(m[1:], ""))
	}
	for _, src := range b.LunSources {
		if src.SizeMetric != "" {
			for k, v := range sizeIndex(values, src.SizeMetric, src.ArrayLabel, src.Label) {
				lunSize[k] = v
			}
		}
		var crx *regexp.Regexp
		if src.CodeRegex != "" {
			crx = regexp.MustCompile(`(?i)` + src.CodeRegex)
		}
		for _, x := range series {
			lun, ok := x[src.Label]
			if x["__name__"] != src.Metric || !ok {
				continue
			}
			arr := x[src.ArrayLabel]
			if luns[arr] == nil {
				luns[arr] = map[string]bool{}
			}
			luns[arr][lun] = true
			if crx != nil && x[src.CodeFrom] != "" {
				if c := codeOf(crx, x[src.CodeFrom]); c != "" {
					code2array[c] = arr
				}
			}
		}
		if src.CodeMetric != "" && crx != nil {
			for _, x := range series {
				if x["__name__"] == src.CodeMetric && x[src.CodeFrom] != "" {
					if c := codeOf(crx, x[src.CodeFrom]); c != "" {
						code2array[c] = x[src.ArrayLabel]
					}
				}
			}
		}
	}
	names := rx.SubexpNames()
	group := func(m []string, name string) string {
		for i, n := range names {
			if n == name && i < len(m) {
				return m[i]
			}
		}
		return ""
	}
	var out []BridgeLink
	seen := map[sizeKey]bool{}
	for _, x := range series {
		if x["__name__"] != b.DatastoreMetric {
			continue
		}
		ds, vc := x["datastore"], x["vcenter"]
		if ds == "" || seen[sizeKey{vc, ds}] {
			continue
		}
		seen[sizeKey{vc, ds}] = true
		m := rx.FindStringSubmatch(ds)
		if m == nil {
			stats["sin convención de nombre"]++
			continue
		}
		code := strings.ToUpper(group(m, "site") + group(m, "code"))
		arr, ok := code2array[code]
		if !ok {
			stats[fmt.Sprintf("cabina %s sin series", code)]++
			continue
		}
		core := toks(group(m, "core"))
		org, idx := strings.ToUpper(group(m, "org")), strings.ToUpper(group(m, "idx"))
		size := dsSize[sizeKey{vc, ds}]
		var arrLuns []string
		for l := range luns[arr] {
			arrLuns = append(arrLuns, l)
		}
		sort.Strings(arrLuns)
		var cand []string
		for _, l := range arrLuns {
			lt := toks(l)
			flat := strings.Join(lt, "")
			if len(lt) == 0 || lt[len(lt)-1] != idx || !contains(lt, org) {
				continue
			}
			okAll := true
			for _, c := range core {
				if !strings.Contains(flat, c) {
					okAll = false
					break
				}
			}
			if okAll {
				cand = append(cand, l)
			}
		}
		method := "nombre"
		if len(cand) > 1 {
			tail := strings.Join(core, "") + idx
			var exact []string
			for _, l := range cand {
				if strings.HasSuffix(strings.Join(toks(l), ""), tail) {
					exact = append(exact, l)
				}
			}
			if len(exact) == 1 {
				cand = exact
			}
		}
		if len(cand) > 1 && size != 0 {
			var same []string
			for _, l := range cand {
				if sameSize(size, lunSize[sizeKey{arr, l}], tol) {
					same = append(same, l)
				}
			}
			if len(same) == 1 {
				cand, method = same, "nombre + capacidad"
			}
		}
		if len(cand) == 0 && size != 0 {
			var same []string
			for _, l := range arrLuns {
				lt := toks(l)
				if contains(lt, org) && len(lt) > 0 && lt[len(lt)-1] == idx && sameSize(size, lunSize[sizeKey{arr, l}], tol) {
					same = append(same, l)
				}
			}
			if len(same) == 1 {
				cand, method = same, "capacidad"
			}
		}
		switch {
		case len(cand) == 1:
			ls := lunSize[sizeKey{arr, cand[0]}]
			if size != 0 && ls != 0 {
				if sameSize(size, ls, tol) {
					if !strings.Contains(method, "capacidad") {
						method = "nombre, capacidad confirmada"
					}
				} else {
					stats["enlazados con capacidad distinta (revisar)"]++
				}
			}
			stats["enlazados por "+method]++
			out = append(out, BridgeLink{VCenter: vc, Datastore: ds, Array: arr, LUN: cand[0], Method: method})
		case len(cand) > 1:
			stats["ambiguos"]++
		default:
			stats["sin LUN coincidente"]++
		}
	}
	return out, stats
}

func (e *Engine) applyBridge(res *Result, in Input) {
	s := e.O.Settings
	if s.DatastoreLunBridge == nil || s.DatastoreClass == "" || s.LunClass == "" {
		return
	}
	links, stats := e.Bridge(in.Series, in.Values)
	res.Stats.Bridge = stats
	for _, l := range links {
		ds := s.DatastoreClass + "|" + l.VCenter + "|" + l.Datastore
		lun := s.LunClass + "|" + l.Array + "|" + l.LUN
		if res.CIs[ds] != nil && res.CIs[lun] != nil {
			res.rel(ds, "está en", lun, "nombre del datastore")
		}
	}
}
