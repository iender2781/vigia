package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
)

// Normalización de nombres y valores, equivalente a la del correlador Python
// (norm_wwn, norm_value, strip_domain) para que los resultados coincidan.

var (
	wwnPrefixRx = regexp.MustCompile(`^(naa\.|eui\.|wwn-0x|wwn-|0x)`)
	hexLongRx   = regexp.MustCompile(`^[0-9a-f]{16,}$`)
	ipv4Rx      = regexp.MustCompile(`^(\d{1,3}(?:\.\d{1,3}){3})(?::\d+)?$`)
	ipv6PortRx  = regexp.MustCompile(`^\[([0-9a-f:]+)\](?::\d+)?$`)
	hostPortRx  = regexp.MustCompile(`^([a-z0-9][a-z0-9._-]*):(\d+)$`)
	digitsDots  = regexp.MustCompile(`^[\d.]+`)
	compactRx   = regexp.MustCompile(`[-_.]`)
)

// NormWWN convierte naa.6008C0E1..., 60:08:c0:..., 0x6008... en 6008c0e1... ("" si no es un WWN).
func NormWWN(v string) string {
	t := wwnPrefixRx.ReplaceAllString(v, "")
	t2 := strings.NewReplacer(":", "", "-", "").Replace(t)
	if hexLongRx.MatchString(t2) && (t != v || strings.Contains(t, ":") || len(t2) >= 24) {
		return t2
	}
	return ""
}

// NormValue normaliza un valor de etiqueta para compararlo entre exporters.
func NormValue(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if w := NormWWN(v); w != "" {
		return w
	}
	if strings.Contains(v, "://") {
		if u, err := url.Parse(v); err == nil && u.Hostname() != "" {
			v = strings.ToLower(u.Hostname())
		}
	}
	if m := ipv4Rx.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if m := ipv6PortRx.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	if m := hostPortRx.FindStringSubmatch(v); m != nil {
		v = m[1]
	}
	return v
}

// StripDomain convierte un FQDN en nombre corto.
func StripDomain(v string) string {
	if strings.Count(v, ".") >= 2 && !digitsDots.MatchString(v) {
		return strings.SplitN(v, ".", 2)[0]
	}
	return v
}

// HashID genera el identificador estable de un CI a partir de su clave natural.
func HashID(naturalKey string) string {
	h := sha256.Sum256([]byte(naturalKey))
	return hex.EncodeToString(h[:12])
}

// toks separa un nombre en fragmentos en mayúsculas (como _toks en Python).
var tokSep = regexp.MustCompile(`[_\-\s.]+`)

func toks(n string) []string {
	var out []string
	for _, t := range tokSep.Split(strings.ToUpper(n), -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}
