// Package contracts define los mensajes que intercambian Vigía y la plataforma de contexto
// por NATS JetStream. Es la versión en Go de los esquemas de contracts/schemas: un test
// comprueba que ambos coinciden. Ver contracts/README.md.
package contracts

import (
	"encoding/json"
	"fmt"
	"time"
)

// Tipos de evento (CloudEvents "type"). La versión mayor va al final: un cambio
// incompatible crea un tipo nuevo y, durante la transición, se publican los dos.
const (
	TypeAlertFired         = "es.vigia.alert.fired.v1"
	TypeAlertResolved      = "es.vigia.alert.resolved.v1"
	TypeAlertEnriched      = "es.vigia.ctx.alert.enriched.v1"
	TypeIncidentOpened     = "es.vigia.ctx.incident.opened.v1"
	TypeIncidentUpdated    = "es.vigia.ctx.incident.updated.v1"
	TypeIncidentResolved   = "es.vigia.ctx.incident.resolved.v1"
	TypeSSIIState          = "es.vigia.ctx.ssii.state.v1"
	TypeAlertAcked         = "es.vigia.feedback.alert.acked.v1"
	TypeAlertClosed        = "es.vigia.feedback.alert.closed.v1"
	TypeRootCause          = "es.vigia.feedback.root_cause.v1"
	TypeMaintenanceStarted = "es.vigia.feedback.maintenance.started.v1"
	TypeMaintenanceEnded   = "es.vigia.feedback.maintenance.ended.v1"
)

// Subject devuelve el sujeto de NATS de un tipo para un tenant.
func Subject(eventType, tenantID string) (string, error) {
	f, ok := subjects[eventType]
	if !ok {
		return "", fmt.Errorf("tipo de evento desconocido: %s", eventType)
	}
	return fmt.Sprintf(f, tenantID), nil
}

var subjects = map[string]string{
	TypeAlertFired:         "vigia.alerts.%s.fired",
	TypeAlertResolved:      "vigia.alerts.%s.resolved",
	TypeAlertEnriched:      "ctx.alert.%s.enriched",
	TypeIncidentOpened:     "ctx.incident.%s.opened",
	TypeIncidentUpdated:    "ctx.incident.%s.updated",
	TypeIncidentResolved:   "ctx.incident.%s.resolved",
	TypeSSIIState:          "ctx.ssii.%s.state",
	TypeAlertAcked:         "vigia.alert.%s.acked",
	TypeAlertClosed:        "vigia.alert.%s.closed",
	TypeRootCause:          "vigia.incident.%s.root_cause",
	TypeMaintenanceStarted: "vigia.maintenance.%s.started",
	TypeMaintenanceEnded:   "vigia.maintenance.%s.ended",
}

// Event es el sobre CloudEvents 1.0 de todos los mensajes.
type Event struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Time            time.Time       `json:"time"`
	Subject         string          `json:"subject,omitempty"`
	TenantID        string          `json:"tenantid"`
	DataContentType string          `json:"datacontenttype,omitempty"`
	Data            json.RawMessage `json:"data"`
}

// NewEvent construye un sobre con los datos serializados.
func NewEvent(id, source, eventType, subject, tenantID string, data any) (Event, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return Event{}, err
	}
	return Event{SpecVersion: "1.0", ID: id, Source: source, Type: eventType, Time: time.Now().UTC(),
		Subject: subject, TenantID: tenantID, DataContentType: "application/json", Data: b}, nil
}

// Alert: es.vigia.alert.fired.v1 / resolved.v1 (publica Vigía en ALERTS).
type Alert struct {
	AlertID      string            `json:"alert_id"`
	Fingerprint  string            `json:"fingerprint"`
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations,omitempty"`
	Severity     string            `json:"severity"`
	ServiceKey   string            `json:"service_key,omitempty"`
	Source       string            `json:"source"`
	StartsAt     time.Time         `json:"starts_at"`
	EndsAt       *time.Time        `json:"ends_at,omitempty"`
	GeneratorURL string            `json:"generator_url,omitempty"`
}

// CIRef identifica un CI en los mensajes.
type CIRef struct {
	ID         string   `json:"id"`
	Class      string   `json:"class"`
	Name       string   `json:"name"`
	Technology string   `json:"technology,omitempty"`
	Sites      []string `json:"sites,omitempty"`
}

// SSIIRef es un Sistema de Información con su estado.
type SSIIRef struct {
	Code  string `json:"code"`
	State string `json:"state"`
}

// AlertEnriched: es.vigia.ctx.alert.enriched.v1 (publica la plataforma en CONTEXT).
type AlertEnriched struct {
	AlertID     string           `json:"alert_id"`
	Fingerprint string           `json:"fingerprint"`
	CI          *CIRef           `json:"ci,omitempty"`
	Role        string           `json:"role"`
	Incident    *IncidentSummary `json:"incident,omitempty"`
	SSII        []SSIIRef        `json:"ssii"`
	Links       *Links           `json:"links,omitempty"`
}

// IncidentSummary resume la incidencia a la que pertenece una alerta.
type IncidentSummary struct {
	ID       string  `json:"id"`
	RootCI   *CIRef  `json:"root_ci,omitempty"`
	Score    float64 `json:"score,omitempty"`
	Affected int     `json:"affected"`
}

// Links son enlaces a la web de la plataforma.
type Links struct {
	CI       string `json:"ci,omitempty"`
	Incident string `json:"incident,omitempty"`
}

// Incident: es.vigia.ctx.incident.opened|updated|resolved.v1.
type Incident struct {
	ID         string      `json:"id"`
	Status     string      `json:"status"`
	OpenedAt   time.Time   `json:"opened_at"`
	ResolvedAt *time.Time  `json:"resolved_at,omitempty"`
	Candidates []Candidate `json:"candidates"`
	Affected   Affected    `json:"affected"`
	SSII       []SSIIRef   `json:"ssii"`
	Alerts     int         `json:"alerts"`
}

// Candidate es una causa probable con sus razones.
type Candidate struct {
	CIID    string   `json:"ci_id"`
	Class   string   `json:"class"`
	Name    string   `json:"name"`
	Score   float64  `json:"score"`
	Reasons []string `json:"reasons"`
}

// Affected resume los elementos afectados.
type Affected struct {
	CIs     int            `json:"cis"`
	ByClass map[string]int `json:"by_class,omitempty"`
}

// SSIIState: es.vigia.ctx.ssii.state.v1.
type SSIIState struct {
	Code        string    `json:"code"`
	State       string    `json:"state"`
	Previous    string    `json:"previous"`
	Since       time.Time `json:"since"`
	Explanation string    `json:"explanation,omitempty"`
	IncidentID  string    `json:"incident_id,omitempty"`
}

// AlertFeedback: es.vigia.feedback.alert.acked|closed.v1 (publica Vigía en FEEDBACK).
type AlertFeedback struct {
	AlertID     string    `json:"alert_id"`
	Fingerprint string    `json:"fingerprint"`
	By          string    `json:"by"`
	At          time.Time `json:"at"`
	Channel     string    `json:"channel,omitempty"`
	Note        string    `json:"note,omitempty"`
}

// RootCause: es.vigia.feedback.root_cause.v1.
type RootCause struct {
	IncidentID   string    `json:"incident_id"`
	CIID         string    `json:"ci_id"`
	Verdict      string    `json:"verdict"`
	PreviousCIID string    `json:"previous_ci_id,omitempty"`
	By           string    `json:"by"`
	At           time.Time `json:"at"`
	Resolution   string    `json:"resolution,omitempty"`
}

// Maintenance: es.vigia.feedback.maintenance.started|ended.v1.
type Maintenance struct {
	MaintenanceID string    `json:"maintenance_id"`
	Status        string    `json:"status"`
	StartsAt      time.Time `json:"starts_at"`
	EndsAt        time.Time `json:"ends_at"`
	Services      []string  `json:"services,omitempty"`
	SSII          []string  `json:"ssii,omitempty"`
	CIIDs         []string  `json:"ci_ids,omitempty"`
	Reason        string    `json:"reason,omitempty"`
	ChangeRef     string    `json:"change_ref,omitempty"`
	By            string    `json:"by"`
}
