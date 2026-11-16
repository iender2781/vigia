# vigia-contracts

Contrato entre **Vigía** (notificaciones y guardias) y la **plataforma de contexto** (CMDB, topología y RCA).
Vigía recibe las alertas y avisa a las personas; la plataforma sabe qué es cada elemento, de qué depende y a
quién afecta. Se comunican por tres flujos de NATS JetStream y, para consultas puntuales, por la API REST
de la plataforma.

| Flujo | Publica | Sujetos | Para qué |
| --- | --- | --- | --- |
| `ALERTS` | Vigía | `vigia.alerts.<tenant>.fired`, `.resolved` | Alertas normalizadas por la ingesta de Vigía |
| `CONTEXT` | Plataforma | `ctx.alert.<tenant>.enriched`, `ctx.incident.<tenant>.opened/updated/resolved`, `ctx.ssii.<tenant>.state` | Contexto de cada alerta, incidencias correlacionadas y estado de los Sistemas de Información |
| `FEEDBACK` | Vigía | `vigia.alert.<tenant>.acked/closed`, `vigia.incident.<tenant>.root_cause`, `vigia.maintenance.<tenant>.started/ended` | Lo que hacen las personas: reconocer, confirmar causas, abrir mantenimientos |

Parámetros de los flujos y consumidores: [`streams.yaml`](streams.yaml).

## Formato

Todos los mensajes son **CloudEvents 1.0** en JSON (modo estructurado):
[`schemas/cloudevent.schema.json`](schemas/cloudevent.schema.json). El campo `type` lleva la versión mayor
al final (`es.vigia.ctx.alert.enriched.v1`) y `tenantid` es el UUID de la empresa en Vigía.

| Tipo | Esquema de `data` | Tipo Go |
| --- | --- | --- |
| `es.vigia.alert.fired.v1`, `...resolved.v1` | [`alert.v1`](schemas/alert.v1.schema.json) | `contracts.Alert` |
| `es.vigia.ctx.alert.enriched.v1` | [`ctx.alert.enriched.v1`](schemas/ctx.alert.enriched.v1.schema.json) | `contracts.AlertEnriched` |
| `es.vigia.ctx.incident.opened/updated/resolved.v1` | [`ctx.incident.v1`](schemas/ctx.incident.v1.schema.json) | `contracts.Incident` |
| `es.vigia.ctx.ssii.state.v1` | [`ctx.ssii.state.v1`](schemas/ctx.ssii.state.v1.schema.json) | `contracts.SSIIState` |
| `es.vigia.feedback.alert.acked/closed.v1` | [`feedback.alert.v1`](schemas/feedback.alert.v1.schema.json) | `contracts.AlertFeedback` |
| `es.vigia.feedback.root_cause.v1` | [`feedback.root_cause.v1`](schemas/feedback.root_cause.v1.schema.json) | `contracts.RootCause` |
| `es.vigia.feedback.maintenance.started/ended.v1` | [`feedback.maintenance.v1`](schemas/feedback.maintenance.v1.schema.json) | `contracts.Maintenance` |

Hay un ejemplo de cada mensaje en [`examples/`](examples). `go test ./contracts` comprueba que esquemas,
tipos Go y ejemplos coinciden; el CI lo ejecuta en cada cambio.

## Reglas de compatibilidad

1. Dentro de una versión (`v1`) solo se **añaden campos opcionales**. Nunca se quitan ni cambian de tipo.
2. Los consumidores **ignoran los campos que no conocen** (los tipos Go no usan `DisallowUnknownFields` fuera de los tests).
3. Un cambio incompatible crea un `type` nuevo (`...v2`); durante la transición el productor publica ambos.
4. Un mensaje que no se puede procesar va a `dlq.<sujeto>`, se mide y **nunca bloquea** el flujo.

## Identificadores compartidos

| Identificador | Dueño | Uso |
| --- | --- | --- |
| `tenantid` | Vigía | Empresa; RLS en ambas bases de datos |
| `fingerprint` | Alertmanager | Clave común de una alerta entre ambos proyectos |
| `alert_id` | Vigía | Alerta en Vigía |
| `ci.id` | Plataforma | Hash estable de la clave natural del CI |
| `ssii[].code` | Ontología (COD_SSII) | Sistema de Información; Vigía lo vincula a sus servicios |
| `incident.id` | Plataforma | Incidencia correlacionada |

## Cómo usa Vigía el contexto

1. Llega una alerta: Vigía la registra y la publica en `ALERTS`.
2. La plataforma responde en `CONTEXT` (objetivo: p99 < 2 s).
3. Vigía espera el contexto como mucho la **ventana de correlación** del servicio (propuesta: 0 s para
   críticas, 30 s para el resto). Si no llega, notifica sin contexto.
4. Si la alerta pertenece a una incidencia ya notificada, se añade a ella sin avisar de nuevo; si la
   incidencia empeora, se notifica la actualización.
5. Vigía decide a quién avisar cruzando `ssii` y `ci.class` con los vínculos de sus servicios.

Estado: **v1 en borrador**. La plataforma publica `CONTEXT` a partir de su fase 2–3; hasta entonces lee
Alertmanager directamente como respaldo.
