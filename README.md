# Vigía

Monorepo de **Vigía**: notificaciones y guardias (en diseño) y su **plataforma de contexto**: CMDB viva,
topología y análisis de causa raíz construidos a partir de Prometheus.

Este repositorio contiene hoy las **fases 0 y 1 de la plataforma de contexto**:

- **Descubrimiento continuo**: cada ciclo lee de Prometheus solo las series que declara la ontología, construye la
  CMDB y la reconcilia con PostgreSQL (altas, cambios con historial, relaciones con validez temporal y ciclo de vida
  `activo → sin datos / obsoleto → retirado`).
- **Ontología en YAML** (`ontology/`), importada del `config_junta.json` del correlador Python y validada en el CI.
- **API REST y web** (React): búsqueda, ficha de cada elemento con su grafo de dependencias e impacto, Sistemas de
  Información, calidad de datos y ejecuciones.
- **Contrato con Vigía** (`contracts/`): mensajes CloudEvents por NATS JetStream, con esquemas, tipos Go y ejemplos.
- **Despliegue**: imagen distroless, chart de Helm, ejemplos GitOps para Argo CD + CloudNativePG + External Secrets.

Documentos: [diseño de la plataforma](https://claude.ai/artifact/QA9YbpYzj5saFu27rfGyj7) ·
[diseño de Vigía](https://claude.ai/artifact/J1vzTGQsoPHqFwX2Hfy9Ky) ·
plan de implantación (ver `docs/`).

## Estructura

```
cmd/contexto/                binario único: serve (roles api, discovery), migrate, replay, ontology
internal/contexto/
  ontology/                  carga, validación e importación de la ontología
  engine/                    motor de reglas (equivalente a build_cmdb del correlador Python)
  discovery/                 ciclo de descubrimiento, liderazgo e informe de calidad
  store/                     PostgreSQL: migraciones, reconciliación, consultas (RLS por tenant)
  api/                       API REST /api/v1, canal SSE, /healthz /readyz /metrics
  prom/                      cliente de Prometheus y Alertmanager; lectura de capturas
  web/                       web compilada embebida en el binario
internal/platform/metrics/   métricas en formato Prometheus sin dependencias
contracts/                   contrato Vigía ⇄ plataforma (vigia-contracts)
ontology/                    ontología (YAML)
web/contexto/                código de la web (React + TypeScript + Cytoscape.js)
deploy/helm/vigia-contexto/  chart de Helm
deploy/gitops/               valores por entorno, Argo CD, CloudNativePG, External Secrets
deploy/compose/              entorno local (PostgreSQL + Prometheus simulado + plataforma)
reference/                   correlador Python original y su configuración (referencia de tests)
tools/                       fakeprom (Prometheus simulado), helmcheck, comparación con Python
```

## Desarrollo

Requisitos: Go 1.23+, Node 22, PostgreSQL 16 (o Docker), Python 3 (solo para la comparación con el correlador).

```bash
make web build test              # web, binario y tests unitarios
make dev                         # entorno local completo en http://localhost:8080 (Docker)
make equivalence                 # misma CMDB que el correlador Python (captura sintética)
make equivalence CAPTURA=dir     # ... con una captura real (series.json, values.json, alerts.json)
CONTEXTO_TEST_DATABASE_URL=postgres://... make test-integration
```

Construir la CMDB a partir de una captura guardada, sin base de datos:

```bash
bin/contexto replay -ontology ontology -series-file series.json -values-file values.json -export /tmp/cmdb
```

## Configuración (variables de entorno)

| Variable | Por defecto | Descripción |
| --- | --- | --- |
| `CONTEXTO_DATABASE_URL` | — | Cadena de conexión a PostgreSQL (**secreto**) |
| `CONTEXTO_TENANT_ID` / `_NAME` | — | UUID y nombre de la empresa en Vigía |
| `CONTEXTO_ONTOLOGY_DIR` | `/etc/contexto/ontology` | Directorio de la ontología |
| `CONTEXTO_PROMETHEUS_URLS` | — | URLs de Prometheus separadas por comas (en HA se deduplican) |
| `CONTEXTO_PROMETHEUS_BEARER_TOKEN` / `_BASIC_AUTH` | — | Credenciales de Prometheus (**secreto**) |
| `CONTEXTO_PROMETHEUS_INSECURE` / `_TIMEOUT` | `false` / `60s` | TLS y tiempo máximo por petición |
| `CONTEXTO_ALERTMANAGER_URL` | — | Opcional: reglas `source: alerts` (respaldo hasta la fase 2) |
| `CONTEXTO_DISCOVERY_INTERVAL` | `5m` | Periodo del descubrimiento |
| `CONTEXTO_SERIES_LOOKBACK` | `10m` | Ventana de /api/v1/series |
| `CONTEXTO_STALE_AFTER_CYCLES` | `3` | Ciclos sin ver un CI (con exporter sano) para marcarlo obsoleto |
| `CONTEXTO_RETIRE_AFTER` | `168h` | Tiempo sin verlo para retirarlo |
| `CONTEXTO_EVENT_RETENTION_MONTHS` | `13` | Retención de eventos (particiones mensuales) |
| `CONTEXTO_HTTP_ADDR` / `_OPS_ADDR` | `:8080` / `:8081` | API y web / salud y métricas |
| `CONTEXTO_AUTH_MODE` | `token` | `token`, `proxy` (cabecera de un proxy de identidad) o `none` |
| `CONTEXTO_API_TOKENS` | — | Tokens de API separados por comas (**secreto**) |
| `CONTEXTO_AUTH_PROXY_HEADER` | `X-Auth-Request-User` | Cabecera con el usuario en modo `proxy` |
| `CONTEXTO_LOG_LEVEL` / `_FORMAT` | `info` / `json` | Registro |

## Estado y siguientes fases

| Fase | Contenido | Estado |
| --- | --- | --- |
| 0 | Repositorio Go, ontología YAML, contrato con Vigía, test de equivalencia | Hecho |
| 1 | Descubrimiento, PostgreSQL, ciclo de vida, API, web de búsqueda y ficha | Hecho |
| 2 | Rol `ingest` (flujo `ALERTS` de Vigía), estado propio y heredado | Pendiente |
| 3 | Rol `correlator` (incidencias y RCA), publicación en `CONTEXT` | Pendiente |
| 4 | Rol `enricher` (ITSM, directorio, vCenter, IPAM, CVE) | Pendiente |
| 5 | Servidor MCP | Pendiente |
