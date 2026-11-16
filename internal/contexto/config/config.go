// Package config lee la configuración del servicio de variables de entorno (CONTEXTO_*).
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Config es la configuración completa del servicio.
type Config struct {
	// Base de datos e identidad
	DatabaseURL string // CONTEXTO_DATABASE_URL (secreto)
	TenantID    string // CONTEXTO_TENANT_ID: UUID de la empresa en Vigía
	TenantName  string // CONTEXTO_TENANT_NAME

	OntologyDir string // CONTEXTO_ONTOLOGY_DIR

	// Prometheus y Alertmanager
	PrometheusURLs     []string      // CONTEXTO_PROMETHEUS_URLS (separadas por comas)
	PrometheusBearer   string        // CONTEXTO_PROMETHEUS_BEARER_TOKEN (secreto)
	PrometheusBasic    string        // CONTEXTO_PROMETHEUS_BASIC_AUTH usuario:contraseña (secreto)
	PrometheusInsecure bool          // CONTEXTO_PROMETHEUS_INSECURE
	PrometheusTimeout  time.Duration // CONTEXTO_PROMETHEUS_TIMEOUT
	AlertmanagerURL    string        // CONTEXTO_ALERTMANAGER_URL (opcional)

	// Descubrimiento
	DiscoveryInterval      time.Duration // CONTEXTO_DISCOVERY_INTERVAL
	SeriesLookback         time.Duration // CONTEXTO_SERIES_LOOKBACK
	StaleAfterCycles       int           // CONTEXTO_STALE_AFTER_CYCLES
	RetireAfter            time.Duration // CONTEXTO_RETIRE_AFTER
	EventRetentionMonths   int           // CONTEXTO_EVENT_RETENTION_MONTHS
	PrometheusConcurrency  int           // CONTEXTO_PROMETHEUS_CONCURRENCY
	MatchersPerRequest     int           // CONTEXTO_MATCHERS_PER_REQUEST

	// HTTP
	HTTPAddr  string   // CONTEXTO_HTTP_ADDR (API y web)
	OpsAddr   string   // CONTEXTO_OPS_ADDR (/healthz, /readyz, /metrics)
	AuthMode  string   // CONTEXTO_AUTH_MODE: token | proxy | none
	APITokens []string // CONTEXTO_API_TOKENS (secreto, separados por comas)
	// Cabecera con el usuario cuando AUTH_MODE=proxy (la pone oauth2-proxy u otro proxy de identidad).
	AuthProxyHeader string // CONTEXTO_AUTH_PROXY_HEADER
	PublicURL       string // CONTEXTO_PUBLIC_URL

	LogLevel  string // CONTEXTO_LOG_LEVEL: debug | info | warn | error
	LogFormat string // CONTEXTO_LOG_FORMAT: json | text
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func list(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

var uuidRx = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// FromEnv lee la configuración. El primer error de formato se devuelve.
func FromEnv() (*Config, error) {
	var errs []error
	dur := func(k, def string) time.Duration {
		d, err := time.ParseDuration(env(k, def))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		}
		return d
	}
	num := func(k string, def int) int {
		n, err := strconv.Atoi(env(k, strconv.Itoa(def)))
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
		}
		return n
	}
	c := &Config{
		DatabaseURL:           env("CONTEXTO_DATABASE_URL", ""),
		TenantID:              env("CONTEXTO_TENANT_ID", "00000000-0000-0000-0000-000000000001"),
		TenantName:            env("CONTEXTO_TENANT_NAME", "Organización"),
		OntologyDir:           env("CONTEXTO_ONTOLOGY_DIR", "/etc/contexto/ontology"),
		PrometheusURLs:        list(env("CONTEXTO_PROMETHEUS_URLS", "")),
		PrometheusBearer:      env("CONTEXTO_PROMETHEUS_BEARER_TOKEN", ""),
		PrometheusBasic:       env("CONTEXTO_PROMETHEUS_BASIC_AUTH", ""),
		PrometheusInsecure:    env("CONTEXTO_PROMETHEUS_INSECURE", "false") == "true",
		PrometheusTimeout:     dur("CONTEXTO_PROMETHEUS_TIMEOUT", "60s"),
		AlertmanagerURL:       env("CONTEXTO_ALERTMANAGER_URL", ""),
		DiscoveryInterval:     dur("CONTEXTO_DISCOVERY_INTERVAL", "5m"),
		SeriesLookback:        dur("CONTEXTO_SERIES_LOOKBACK", "10m"),
		StaleAfterCycles:      num("CONTEXTO_STALE_AFTER_CYCLES", 3),
		RetireAfter:           dur("CONTEXTO_RETIRE_AFTER", "168h"),
		EventRetentionMonths:  num("CONTEXTO_EVENT_RETENTION_MONTHS", 13),
		PrometheusConcurrency: num("CONTEXTO_PROMETHEUS_CONCURRENCY", 4),
		MatchersPerRequest:    num("CONTEXTO_MATCHERS_PER_REQUEST", 20),
		HTTPAddr:              env("CONTEXTO_HTTP_ADDR", ":8080"),
		OpsAddr:               env("CONTEXTO_OPS_ADDR", ":8081"),
		AuthMode:              env("CONTEXTO_AUTH_MODE", "token"),
		APITokens:             list(env("CONTEXTO_API_TOKENS", "")),
		AuthProxyHeader:       env("CONTEXTO_AUTH_PROXY_HEADER", "X-Auth-Request-User"),
		PublicURL:             env("CONTEXTO_PUBLIC_URL", ""),
		LogLevel:              env("CONTEXTO_LOG_LEVEL", "info"),
		LogFormat:             env("CONTEXTO_LOG_FORMAT", "json"),
	}
	if !uuidRx.MatchString(c.TenantID) {
		errs = append(errs, fmt.Errorf("CONTEXTO_TENANT_ID debe ser un UUID"))
	}
	switch c.AuthMode {
	case "token", "proxy", "none":
	default:
		errs = append(errs, fmt.Errorf("CONTEXTO_AUTH_MODE debe ser token, proxy o none"))
	}
	return c, errors.Join(errs...)
}

// RequireDB comprueba que hay base de datos configurada.
func (c *Config) RequireDB() error {
	if c.DatabaseURL == "" {
		return errors.New("falta CONTEXTO_DATABASE_URL")
	}
	return nil
}

// StaleAfter es el tiempo sin ver un CI para marcarlo obsoleto.
func (c *Config) StaleAfter() time.Duration {
	return time.Duration(c.StaleAfterCycles) * c.DiscoveryInterval
}
