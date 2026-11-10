#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
correlador_alertas.py
=====================
Prototipo de correlación de alertas de Prometheus / Alertmanager.

1. Lee las alertas activas de Alertmanager (API v2) o de Prometheus (/api/v1/alerts).
2. Lee las series de Prometheus (/api/v1/series) y deduce automáticamente la
   topología a partir de las etiquetas que comparten las métricas de los
   distintos exporters (cabinas, balanceadores, servidores...).
3. Agrupa las alertas relacionadas, propone una causa probable por grupo y
   genera una página HTML autocontenida (o la sirve por HTTP).

Solo usa la librería estándar de Python (3.8+).

Ejemplos
--------
  # Demo con datos sintéticos (OceanStor, Unity, IBM, F5, node_exporter)
  python3 correlador_alertas.py --demo -o alertas.html

  # Datos reales -> fichero HTML (uno o varios Prometheus)
  python3 correlador_alertas.py --alertmanager http://alertmanager:9093 \\
                                --prometheus   http://prometheus-a:9090 \\
                                --prometheus   http://prometheus-b:9090 -o alertas.html

  # Servir como web, regenerando cada 60 s  ->  http://<servidor>:8080/
  python3 correlador_alertas.py --alertmanager ... --prometheus ... --serve --port 8080

  # Capturar datos crudos para analizarlos offline (o enviarlos para afinar)
  python3 correlador_alertas.py --alertmanager ... --prometheus ... --save-raw ./captura
  python3 correlador_alertas.py --alerts-file captura/alerts.json \\
                                --series-file captura/series.json -o alertas.html

Cómo se deduce la topología (resumen)
-------------------------------------
* Cada etiqueta "útil" de una serie (no job, severity, status, mode...) es un
  elemento.  Dos elementos que aparecen en la misma serie están relacionados.
* La dirección (quién depende de quién) sale de:
    - capas configurables por nombre de etiqueta (infraestructura física >
      recursos lógicos > host > servicio), y dentro de la misma capa
    - dependencias funcionales (cada LUN tiene un único pool -> el pool está
      por encima) o inclusión de métricas (la etiqueta array aparece en todas
      las métricas donde aparece controller -> array está por encima).
* Los valores con forma de IP / hostname y las etiquetas de tipo host
  (instance, host, nodename, member, address...) se unen entre exporters por
  valor normalizado (sin puerto, sin dominio, en minúsculas). Así
  `instance="10.1.1.11:9100"`, `member="10.1.1.11:443"` y
  `node_uname_info{nodename="srv01.corp"}` acaban siendo el mismo elemento.
* Los nombres que se repiten en distintos padres (CTE0.A en dos cabinas,
  /dev/sda1 en todos los servidores) se cualifican con su padre para no
  mezclar elementos distintos.
* Dos alertas se correlan si:
    - afectan al mismo elemento o una lleva el elemento de la otra en sus etiquetas,
    - el elemento de una está "aguas arriba" del de la otra (a pocos saltos), o
    - son la misma alerta en elementos hermanos que cuelgan del mismo padre.
"""

import argparse
import base64
import html
import ipaddress
import json
import os
import re
import ssl
import sys
import threading
import time
from collections import Counter, defaultdict, deque
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from itertools import chain
from urllib.parse import urlencode, urlparse
from urllib.request import Request, urlopen

VERSION = "0.1"

# --------------------------------------------------------------------------
# Configuración por defecto (se puede sobreescribir con --config fichero.json)
# --------------------------------------------------------------------------
DEFAULT_CONFIG = {
    # Etiquetas que nunca identifican un elemento
    "stop_labels": [
        "__name__", "job", "le", "quantile", "severity", "priority", "team", "owner",
        "env", "environment", "entorno", "monitor", "prometheus", "prometheus_replica",
        "replica", "receiver", "alertname", "alertstate", "alertgroup", "fstype", "cpu",
        "unit", "exporter", "collector", "scrape_job", "site", "nivel", "layer", "category",
        "aspect", "created_at", "message_id", "alert_group", "alert_category", "source",
        "event_id", "permission", "generation", "localizacion", "bridge_method", "cmdb_source",
    ],
    # Métricas que no aportan topología
    "exclude_metrics_regex": r"ALERTS|ALERTS_FOR_STATE|scrape_.*|up|python_.*|process_.*|go_.*",
    # Renombrado de etiquetas por exporter (para etiquetas genéricas como name/pool/node).
    # Se aplica la PRIMERA regla cuyo "when" (regex por etiqueta) cumple. Ver config de ejemplo.
    "relabel": [],
    # Límite de fanout por etiqueta destino (p. ej. un pool F5 depende de todos sus miembros)
    "fanout_overrides": {},
    # "Misma alerta en hermanos": solo si alerta al menos este % de los hijos del padre común
    "sibling_min_ratio": 0.5,
    # Alertas de capacidad/configuración: se deduplican y resumen, pero no son causa ni
    # efecto de otras (un snapshot antiguo no lo provoca un fallo de caminos).
    "non_causal_alert_regex": r"(?i).*(Snapshot|Tools|Versiones|Version|Licen|DataReduction|NoSavings|"
                              r"ThinProvisioning|Subscribed|Quota|Ocupacion|Usage|Full|Capacity|Cert|"
                              r"Unfixed|NativeAlert|FilesystemCritical).*",
    # Patrones (regex, nombre completo) de etiquetas que describen estado/atributos
    "stop_label_patterns": [
        r"(.*_)?(status|state|health|mode|type|model|version|vendor|firmware|fw|desc|"
        r"description|help|reason|role|speed|duplex|protocol|direction|kind|summary|"
        r"message|runbook|runbook_url|dashboard)",
        r"(.*_)?id|os|os_type",       # ids numéricos y atributos: no aportan relaciones
    ],
    # Valores que no identifican nada
    "generic_values": [
        "", "true", "false", "yes", "no", "none", "null", "nil", "n/a", "na", "unknown",
        "-", "ok", "up", "down", "normal", "online", "offline", "enabled", "disabled", "0", "1",
    ],
    "ignore_labels": [],          # etiquetas extra a ignorar
    "force_entity_labels": [],    # (reservado) etiquetas a tratar siempre como elemento
    "exclude_alertnames": ["Watchdog", "InfoInhibitor", "DeadMansSwitch"],
    # Capas: cuanto menor el número, más "aguas arriba" (más infraestructura)
    # El orden importa: una etiqueta se asigna a la PRIMERA capa cuyo patrón cumple.
    "layers": [
        {"name": "Infraestructura física / clúster",
         "pattern": r"(.*_)?(array|storage|storage_system|system|system_name|serial|sn|chassis|"
                    r"enclosure|controller|ctrl|bigip|f5|f5_device|switch|router|frame|cluster|"
                    r"cluster_name|vcenter|datacenter|dc|dc_name|sede|f5_tenant|tenant|"
                    r"f5_chassis_partition|f5_partition)"},
        {"name": "Almacenamiento lógico (pool, LUN, WWN...)",
         "pattern": r"(.*_)?(pool|storage_pool|storagepool|mdiskgrp|mdisk_grp|mdisk|disk_domain|"
                    r"raid_group|lun|lun_name|lun_id|volume|vol|vdisk|filesystem|fs|nas_server|"
                    r"share|lif|port|interface|disk|wwn|naa|lun_wwn|wwid|canonical_name|scsi_id|"
                    r"nasserver|vstore|lungroup|hostgroup|diskgroup)"},
        {"name": "Virtualización (ESXi, datastore)",
         "pattern": r"(.*_)?(esx|esxi|esx_host|esxi_host|vmhost|hypervisor|host_name|hostsystem|"
                    r"datastore|datastore_name|ds|ds_name|vsan)"},
        {"name": "Host / servidor / VM",
         "pattern": r"(.*_)?(host|hostname|nodename|node|instance|server|vm|vm_name|vmname|guest|"
                    r"guest_hostname|guest_ip|ip|address|addr|initiator|member|pool_member|"
                    r"f5_node|f5_node_address)"},
        {"name": "Servicio / aplicación",
         "pattern": r"(.*_)?(service|app|application|virtual_server|vs|vip|url|target|endpoint|website|"
                    r"f5_pool|f5_virtual_server)"},
    ],
    "default_layer_name": "Otros / sin clasificar",
    # Capas cuyas etiquetas se unen por valor entre exporters (sin el nombre de la etiqueta)
    "bare_layers": [4],
    # Otras etiquetas que también se unen por valor (ESXi: host_name="esx01.corp" = host="ESX01")
    "bare_label_patterns": [r"(.*_)?(esx|esxi|esx_host|esxi_host|vmhost|hypervisor|host_name|hostsystem)"],
    # Capas cuyas etiquetas son alias del mismo equipo (instance = nodename = vm_name...)
    "alias_layers": [4],
    "fd_threshold": 0.95,         # % de valores que deben cumplir la dependencia funcional
    "max_hops": 6,                # saltos máximos en la topología para correlar
    # Si un elemento depende de más de N elementos del mismo tipo (un ESXi que ve 20 LUNs)
    # no se sigue esa relación al correlar: es demasiado genérica.
    "max_parent_fanout": 3,
    "time_window_minutes": 0,     # 0 = sin ventana temporal
    "hub_series_ratio": 0.4,      # un elemento presente en >40% de las series es "global"
    "hub_min_series": 200,
    "alert_global_ratio": 0.5,    # etiqueta (no vista en series) en >50% de alertas = global
    "series_lookback_minutes": 10,
    "series_match": ['{job=~".+"}'],
    # Normalización de nombres de host (solo etiquetas de tipo host): [regex, reemplazo]
    "host_rewrite": [],
    # Compara hosts sin guiones/puntos: "s00e-hses-48" = "S00EHSES48"
    "host_compact": True,
    # Puente datastore (vSphere) -> LUN (cabina) por convención de nombres. Ver config de ejemplo.
    "datastore_lun_bridge": None,
    # Métricas cuyos VALORES se capturan como "CMDB" (info, capacidades, estados, mapeos)
    "cmdb_metrics_regex": r".*_info|.*capacity.*bytes|.*size_total_bytes|.*_total_mb|.*mapped.*|"
                          r".*health.*|.*running_status|vsphere_.*_state|.*initiators.*|.*paths.*|"
                          r"vsphere_datastore_.*|vsphere_vm_datastore_info|.*_up",
    # Tolerancia para considerar que dos capacidades son la misma (datastore vs LUN)
    "size_match_tolerance": 0.01,
    # Etiquetas que identifican el equipo de una alerta (para el resumen de avisos repetidos)
    "device_labels": ["array", "storage_system", "sede", "chassis", "f5_tenant", "f5_chassis_partition",
                      "vcenter", "system", "bigip", "device", "exported_instance", "instance"],
}

SEVERITY_RANK = {
    "critical": 4, "crit": 4, "disaster": 4, "emergency": 4, "fatal": 4, "p1": 4,
    "high": 3, "major": 3, "error": 3, "p2": 3,
    "warning": 2, "warn": 2, "minor": 2, "average": 2, "p3": 2,
    "info": 1, "informational": 1, "low": 1, "notice": 1, "p4": 1,
}

IPV4_RE = re.compile(r"^(\d{1,3}(?:\.\d{1,3}){3})(?::\d+)?$")
HOSTPORT_RE = re.compile(r"^([a-z0-9][a-z0-9._-]*):(\d+)$")
HOSTNAME_RE = re.compile(r"[a-z][a-z0-9]*(-[a-z0-9]+){2,}")   # srv-sap-01, no blade-1


def log(msg):
    print(msg, file=sys.stderr)


# --------------------------------------------------------------------------
# Utilidades
# --------------------------------------------------------------------------
WWN_PREFIX_RE = re.compile(r"^(naa\.|eui\.|wwn-0x|wwn-|0x)")


def norm_wwn(v):
    """naa.6008C0E1..., 60:08:c0:e1:..., 0x6008c0e1... -> 6008c0e1... (o None)."""
    t = WWN_PREFIX_RE.sub("", v)
    t2 = t.replace(":", "").replace("-", "")
    if re.fullmatch(r"[0-9a-f]{16,}", t2) and (t != v or ":" in t or len(t2) >= 24):
        return t2
    return None


def norm_value(v):
    """Normaliza un valor de etiqueta para poder compararlo entre exporters."""
    v = str(v).strip().lower()
    w = norm_wwn(v)
    if w:
        return w
    if "://" in v:
        v = urlparse(v).hostname or v
    m = IPV4_RE.match(v)
    if m:
        return m.group(1)
    m = re.match(r"^\[([0-9a-f:]+)\](?::\d+)?$", v)
    if m:
        return m.group(1)
    m = HOSTPORT_RE.match(v)
    if m:
        v = m.group(1)
    return v


def strip_domain(v):
    """FQDN -> nombre corto (solo para etiquetas de tipo host)."""
    if v.count(".") >= 2 and not re.match(r"^[\d.]+$", v):
        return v.split(".", 1)[0]
    return v


def identity_like(v):
    """¿El valor parece una IP, un WWN/NAA o un hostname (globalmente significativo)?"""
    if IPV4_RE.match(v) or re.fullmatch(r"[0-9a-f]{16,}", v):
        return True
    try:
        ipaddress.ip_address(v)
        return True
    except ValueError:
        pass
    return len(v) >= 6 and bool(HOSTNAME_RE.fullmatch(v))


def long_name(v):
    """Nombres largos y específicos (FS_NFS_E_ALFRESCO, publicidad.canalsur.es_pool...):
    se comparan entre etiquetas, pero se cualifican con su padre si se repiten en varios."""
    return len(v) >= 10 and bool(re.search(r"[a-z]", v)) and bool(re.search(r"[_.\-]", v)) \
        and not v.startswith("/")


def parse_time(s):
    if not s:
        return None
    try:
        s = s.strip().replace("Z", "+00:00")
        m = re.match(r"^(.*T\d\d:\d\d:\d\d)(\.\d+)?(.*)$", s)
        if m:
            frac = (m.group(2) or "")[:7]
            s = m.group(1) + frac + m.group(3)
        dt = datetime.fromisoformat(s)
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        if dt.year < 1990:          # Alertmanager usa 0001-01-01 para "sin valor"
            return None
        return dt
    except Exception:
        return None


def camel_tokens(name):
    return {t.lower() for t in re.findall(r"[A-Z]+(?![a-z])|[A-Z]?[a-z]+|\d+", name or "")}


def human_ago(dt, now):
    if not dt:
        return ""
    secs = int((now - dt).total_seconds())
    if secs < 0:
        secs = 0
    d, r = divmod(secs, 86400)
    h, r = divmod(r, 3600)
    m, _ = divmod(r, 60)
    if d:
        return f"hace {d}d {h}h"
    if h:
        return f"hace {h}h {m}m"
    return f"hace {m}m"


# --------------------------------------------------------------------------
# Acceso a las APIs
# --------------------------------------------------------------------------
class Http:
    def __init__(self, timeout=30, insecure=False, basic_auth=None, bearer=None):
        self.timeout = timeout
        self.ctx = ssl._create_unverified_context() if insecure else None
        self.headers = {"Accept": "application/json", "User-Agent": f"correlador-alertas/{VERSION}"}
        if basic_auth:
            self.headers["Authorization"] = "Basic " + base64.b64encode(basic_auth.encode()).decode()
        if bearer:
            self.headers["Authorization"] = "Bearer " + bearer

    def get_json(self, base, path, params=None):
        url = base.rstrip("/") + path
        if params:
            url += "?" + urlencode(params, doseq=True)
        req = Request(url, headers=self.headers)
        with urlopen(req, timeout=self.timeout, context=self.ctx) as r:
            return json.loads(r.read().decode("utf-8"))


def fetch_alerts_alertmanager(http, base):
    return http.get_json(base, "/api/v2/alerts",
                         {"active": "true", "silenced": "true", "inhibited": "true",
                          "unprocessed": "true"})


def fetch_alerts_prometheus(http, base):
    return http.get_json(base, "/api/v1/alerts")


def fetch_series(http, base, matches, lookback_min):
    end = time.time()
    start = end - lookback_min * 60
    params = [("match[]", m) for m in matches] + [("start", f"{start:.0f}"), ("end", f"{end:.0f}")]
    return http.get_json(base, "/api/v1/series", params)


def fetch_values(http, base, name_regex):
    """Valores actuales de las métricas "CMDB": /api/v1/query?query={__name__=~"..."}"""
    q = '{__name__=~"%s"}' % name_regex.replace('"', '\\"')
    return http.get_json(base, "/api/v1/query", {"query": q})


def fetch_metadata(http, base):
    return http.get_json(base, "/api/v1/metadata")


def normalize_values(raw):
    """Respuesta de /api/v1/query (vector) -> lista de etiquetas con "__value__"."""
    if isinstance(raw, dict):
        raw = raw.get("data", raw)
        if isinstance(raw, dict):
            raw = raw.get("result", [])
    out = []
    for r in raw or []:
        if "metric" in r:
            try:
                v = float(r.get("value", [0, "nan"])[1])
            except (TypeError, ValueError):
                v = float("nan")
            out.append({**r["metric"], "__value__": v})
        elif isinstance(r, dict):
            out.append(r)
    return out


def load_cmdb_csv(path):
    """CSV de CMDB (separador , o ;): cada fila se convierte en una serie cuyas etiquetas
    son las columnas. Ej: vcenter;cluster;datastore;array;lun"""
    import csv
    with open(path, encoding="utf-8-sig", newline="") as f:
        sample = f.read(4096)
        f.seek(0)
        dialect = csv.Sniffer().sniff(sample, delimiters=",;\t")
        rows = []
        for r in csv.DictReader(f, dialect=dialect):
            labels = {k.strip(): v.strip() for k, v in r.items() if k and v and v.strip()}
            if len(labels) >= 2:
                rows.append({"__name__": "cmdb_csv", "cmdb_source": os.path.basename(path), **labels})
    return rows


def normalize_alerts(raw, include_pending=False):
    """Acepta la respuesta de Alertmanager v2 o de Prometheus /api/v1/alerts."""
    if isinstance(raw, dict):
        raw = raw.get("data", raw)
        if isinstance(raw, dict):
            raw = raw.get("alerts", [])
    out = []
    for a in raw or []:
        labels = dict(a.get("labels") or {})
        ann = dict(a.get("annotations") or {})
        if "status" in a and isinstance(a["status"], dict):          # Alertmanager
            st = a["status"]
            silenced = bool(st.get("silencedBy"))
            inhibited = bool(st.get("inhibitedBy"))
            state = "silenciada" if silenced else "inhibida" if inhibited else \
                ("activa" if st.get("state") == "active" else st.get("state", ""))
            starts = parse_time(a.get("startsAt"))
            src = "alertmanager"
        else:                                                        # Prometheus
            pst = a.get("state", "firing")
            if pst == "pending" and not include_pending:
                continue
            if pst == "inactive":
                continue
            silenced = inhibited = False
            state = "activa" if pst == "firing" else "pendiente" if pst == "pending" else pst
            starts = parse_time(a.get("activeAt"))
            src = "prometheus"
        sev = str(labels.get("severity", "")).lower()
        out.append({
            "name": labels.get("alertname", "(sin nombre)"),
            "labels": labels,
            "annotations": ann,
            "starts": starts,
            "state": state,
            "silenced": silenced,
            "inhibited": inhibited,
            "severity": sev or "-",
            "sev_rank": SEVERITY_RANK.get(sev, 0),
            "summary": ann.get("summary") or ann.get("message") or ann.get("description") or "",
            "url": a.get("generatorURL", ""),
            "source": src,
        })
    return out


def normalize_raw_list(raw):
    """Respuesta de /api/v1/alerts -> lista de alertas en bruto."""
    if isinstance(raw, dict):
        raw = raw.get("data", raw)
        if isinstance(raw, dict):
            raw = raw.get("alerts", [])
    return raw or []


def normalize_series(raw):
    if isinstance(raw, dict):
        raw = raw.get("data", [])
    return [s for s in (raw or []) if isinstance(s, dict)]



# --------------------------------------------------------------------------
# Puente datastore -> LUN por convención de nombres
# --------------------------------------------------------------------------
def _toks(n):
    return [t for t in re.split(r"[_\-\s.]+", str(n).upper()) if t]


def _size_index(values, metric, keys):
    idx = {}
    for x in values or ():
        if x.get("__name__") == metric and all(k in x for k in keys):
            v = x.get("__value__")
            if v is not None and v == v and v > 0:
                idx[tuple(x[k] for k in keys)] = v
    return idx


def _same_size(a, b, tol):
    return a and b and abs(a - b) <= max(tol * max(a, b), 2 ** 30)   # 1% o 1 GiB


def datastore_backing(series, cfg):
    """(vcenter, datastore) -> texto con la cabina/LUN que lo soporta, para mostrarlo en la web.
    Fuentes, por orden: CSV de CMDB, puente por nombre y, si no, el código de cabina del nombre."""
    out = {}
    b = cfg.get("datastore_lun_bridge") or {}
    rx = re.compile(b["datastore_regex"]) if b.get("datastore_regex") else None
    sites = cfg.get("site_names", {})
    for x in series:
        if x.get("__name__") == "vsphere_datastore_capacity_bytes" and rx:
            m = rx.match(x.get("datastore", ""))
            key = (x.get("vcenter"), x.get("datastore"))
            if m and key not in out:
                site = m.groupdict().get("site") or ""
                out[key] = (f"cabina {m.group('code')} en {sites.get(site.upper(), site)}"
                            " (sin series de esa cabina: volumen desconocido)")
            elif key not in out:
                out[key] = "cabina desconocida (el nombre no sigue la convención; usar NAA o CSV de CMDB)"
    for x in series:
        if x.get("__name__") == "bridge_datastore_lun":
            out[(x.get("vcenter"), x.get("datastore"))] = \
                f"{x.get('array')} · LUN {x.get('lun')} (por {x.get('bridge_method', 'nombre')})"
    for x in series:
        if x.get("__name__") == "cmdb_csv" and x.get("datastore") and (x.get("array") or x.get("lun")):
            out[(x.get("vcenter"), x.get("datastore"))] = \
                f"{x.get('array', '?')} · {x.get('lun', '')} (CMDB {x.get('cmdb_source', 'CSV')})"
    return out


def build_datastore_lun_bridge(series, bcfg, values=None, tol=0.01):
    """Genera series sintéticas {datastore, lun, array} cuando el nombre del datastore
    permite identificar la LUN de la cabina que lo soporta.

    bcfg (config "datastore_lun_bridge"):
      datastore_metric   métrica de vSphere con la etiqueta datastore
      datastore_regex    regex con grupos: code (cabina), org, core (nombre), idx (sufijo)
      lun_sources        [{"metric": ..., "label": nombre de la LUN, "array_label": ...,
                           "code_from": etiqueta, "code_regex": regex con grupos que forman el código}]
    Devuelve (series_sintéticas, estadísticas)."""
    rx = re.compile(bcfg["datastore_regex"])
    code2array, luns = {}, defaultdict(set)
    lun_size = {}
    ds_size = _size_index(values, bcfg.get("datastore_size_metric", "vsphere_datastore_capacity_bytes"),
                          ("vcenter", "datastore"))
    for src in bcfg["lun_sources"]:
        if src.get("size_metric"):
            for k, v in _size_index(values, src["size_metric"], (src["array_label"], src["label"])).items():
                lun_size[k] = v
        crx = re.compile(src["code_regex"], re.I) if src.get("code_regex") else None
        for x in series:
            if x.get("__name__") != src["metric"] or src["label"] not in x:
                continue
            arr = x.get(src["array_label"], "")
            luns[arr].add(x[src["label"]])
            if crx and x.get(src.get("code_from", "")):
                m = crx.search(x[src["code_from"]])
                if m:
                    code2array["".join(m.groups()).upper()] = arr
        # algunas fuentes publican el código en otra métrica (p. ej. el pool)
        if src.get("code_metric") and crx:
            for x in series:
                if x.get("__name__") == src["code_metric"] and x.get(src.get("code_from", "")):
                    m = crx.search(x[src["code_from"]])
                    if m:
                        code2array["".join(m.groups()).upper()] = x.get(src["array_label"], "")
    out, stats, seen = [], Counter(), set()
    for x in series:
        if x.get("__name__") != bcfg["datastore_metric"]:
            continue
        ds, vc = x.get("datastore"), x.get("vcenter")
        if not ds or (vc, ds) in seen:
            continue
        seen.add((vc, ds))
        m = rx.match(ds)
        if not m:
            stats["sin convención de nombre"] += 1
            continue
        code = ((m.groupdict().get("site") or "") + m.group("code")).upper()
        arr = code2array.get(code)
        if arr is None:
            stats[f"cabina {code} sin series"] += 1
            continue
        core = _toks(m.group("core"))
        org, idx = m.group("org").upper(), m.group("idx").upper()
        size = ds_size.get((vc, ds))
        cand = []
        for l in luns[arr]:
            lt = _toks(l)
            flat = "".join(lt)
            if lt and lt[-1] == idx and org in lt and all(c in flat for c in core):
                cand.append(l)
        method = "nombre"
        if len(cand) > 1:   # desempate: la LUN cuyo nombre termina exactamente igual
            tail = "".join(core) + idx
            exact = [l for l in cand if "".join(_toks(l)).endswith(tail)]
            if len(exact) == 1:
                cand = exact
        if len(cand) > 1 and size:   # desempate por capacidad
            same = [l for l in cand if _same_size(size, lun_size.get((arr, l)), tol)]
            if len(same) == 1:
                cand, method = same, "nombre + capacidad"
        if not cand and size:        # sin candidato por nombre: misma org e índice y misma capacidad
            same = [l for l in luns[arr] if org in _toks(l) and _toks(l)[-1] == idx
                    and _same_size(size, lun_size.get((arr, l)), tol)]
            if len(same) == 1:
                cand, method = same, "capacidad"
        if len(cand) == 1:
            ls = lun_size.get((arr, cand[0]))
            if size and ls:
                if _same_size(size, ls, tol):
                    method = method if "capacidad" in method else "nombre, capacidad confirmada"
                else:
                    stats["enlazados con capacidad distinta (revisar)"] += 1
            stats[f"enlazados por {method}"] += 1
            out.append({"__name__": "bridge_datastore_lun", "vcenter": vc, "datastore": ds,
                        "lun": cand[0], "array": arr, "bridge_method": method})
        elif cand:
            stats["ambiguos"] += 1
        else:
            stats["sin LUN coincidente"] += 1
    return out, stats

# --------------------------------------------------------------------------
# Topología inferida
# --------------------------------------------------------------------------
class Topology:
    def __init__(self, series, cfg):
        self.cfg = cfg
        self._stop = {s.lower() for s in cfg["stop_labels"]} | {s.lower() for s in cfg["ignore_labels"]}
        self._stop_re = [re.compile(p, re.I) for p in cfg["stop_label_patterns"]]
        self._generic = {g.lower() for g in cfg["generic_values"]}
        self._layers = [(i + 1, l["name"], re.compile(l["pattern"], re.I))
                        for i, l in enumerate(cfg["layers"])]
        self.default_layer = len(self._layers) + 1
        self.bare_layers = set(cfg["bare_layers"])
        self._bare_re = [re.compile(p, re.I) for p in cfg["bare_label_patterns"]]
        self.alias_layers = set(cfg["alias_layers"])
        self._layer_cache = {}
        self._up_cache = {}
        self._excl_metric = re.compile(cfg["exclude_metrics_regex"]) if cfg.get("exclude_metrics_regex") else None
        self._host_rw = [(re.compile(a, re.I), b) for a, b in cfg.get("host_rewrite", [])]
        self._relabel = [({k: re.compile(v) for k, v in r.get("when", {}).items()}, r.get("rename", {}))
                         for r in cfg.get("relabel", [])]

        # ---- pasada 1: estadísticas por etiqueta -------------------------
        self.values = defaultdict(set)
        self.metrics = defaultdict(set)
        pair = defaultdict(lambda: defaultdict(set))
        cleaned = []
        # Un "instance" que aparece en varios jobs es el servidor de los exporters, no un equipo
        inst_jobs = defaultdict(set)
        for s in series:
            if s.get("instance") and s.get("job"):
                inst_jobs[norm_value(s["instance"])].add(s["job"])
        self.shared_instances = {i for i, j in inst_jobs.items() if len(j) > 1}
        for s in series:
            if self._excl_metric and self._excl_metric.fullmatch(s.get("__name__", "")):
                continue
            s = self.relabel(s)
            items = self.clean(s)
            if not items:
                continue
            cleaned.append((items, s))
            name = s.get("__name__", "")
            for k, v in items.items():
                self.values[k].add(v)
                self.metrics[k].add(name)
            ks = list(items.items())
            for k1, v1 in ks:
                for k2, v2 in ks:
                    if k1 != k2:
                        pair[(k1, k2)][v1].add(v2)
        self.n_series = len(cleaned)
        self.labels = set(self.values)
        self.distinct = {k: len(v) for k, v in self.values.items()}

        thr = cfg["fd_threshold"]
        self.fd = set()                    # (a, b): cada valor de a tiene un único b
        self.cooc = defaultdict(set)
        for (a, b), m in pair.items():
            self.cooc[a].add(b)
            if sum(1 for vs in m.values() if len(vs) == 1) >= thr * len(m):
                self.fd.add((a, b))
        del pair

        # etiquetas cuyos valores son WWN/NAA: identifican el mismo disco en todos los exporters
        self.wwn_labels = {k for k, vs in self.values.items()
                           if vs and sum(1 for v in vs if re.fullmatch(r"[0-9a-f]{16,}", v)) >= 0.9 * len(vs)}
        self.above = defaultdict(set)      # hijo -> padres (misma capa)
        for c in self.labels:
            for p in self.cooc[c]:
                if self._is_above(p, c):
                    self.above[c].add(p)
        self.anc = {c: self._closure(c) for c in self.labels}

        # ---- pasada 2: nombres ambiguos (se repiten bajo padres distintos)
        occ = defaultdict(list)            # (L, v) -> [{padre: valor}, ...]
        for items, _ in cleaned:
            for L, v in items.items():
                if self.is_bare(L, v):
                    continue
                ups = {P: pv for P, pv in items.items() if P != L and self.upL(L, P)}
                if ups:
                    occ[(L, v)].append(ups)
        self.qual = {}
        for key, lst in occ.items():
            vals = defaultdict(set)
            for o in lst:
                for P, pv in o.items():
                    vals[P].add(pv)
            conf = [P for P, vs in vals.items() if len(vs) > 1]
            if not conf:
                continue
            presence = Counter(P for o in lst for P in o)

            def resolves(P):
                # P resuelve la ambigüedad si cada valor de P fija el resto de padres en conflicto
                seen = defaultdict(lambda: defaultdict(set))
                for o in lst:
                    if P in o:
                        for Q in conf:
                            if Q != P and Q in o:
                                seen[o[P]][Q].add(o[Q])
                return all(len(x) <= 1 for d in seen.values() for x in d.values())
            spec = lambda P: (-len(self.anc.get(P, ())), -self.layer(P), P)
            good = sorted((P for P in conf if resolves(P)), key=lambda P: (-presence[P], spec(P)))
            rest = sorted((P for P in conf if P not in good), key=spec)
            self.qual[key] = good + rest
        del occ

        # ---- pasada 3: grafo de elementos --------------------------------
        self.up = defaultdict(set)         # nodo -> nodos de los que depende
        self.peer = defaultdict(set)       # relación sin dirección
        self.node_series = Counter()
        self.node_disp = {}
        self.node_label = {}
        edges = 0
        self.alias = {}                    # nodo -> nodo que lo soporta (datastore -> LUN)
        for items, raw in cleaned:
            ids = self.node_ids(items)
            if raw.get("__name__") in ("bridge_datastore_lun", "cmdb_csv") and "datastore" in ids and "lun" in ids:
                self.alias[ids["datastore"]] = ids["lun"]
            for L, nid in ids.items():
                self.node_series[nid] += 1
                self.node_label.setdefault(nid, L)
                self.node_disp.setdefault(nid, items[L] if self.is_bare(L, items[L]) else f"{L}={raw.get(L, items[L])}")
            labels = list(ids)
            upmap = {A: {B for B in labels if B != A and self.upL(A, B)} for A in labels}
            for A in labels:
                for B in labels:
                    if A == B or ids[A] == ids[B]:
                        continue
                    if B in upmap[A]:
                        # evita atajos: A->C->B ya existe
                        if not any(B in upmap[C] for C in upmap[A]
                                   if C != B and self.layer(C) not in self.alias_layers):
                            if ids[B] not in self.up[ids[A]]:
                                self.up[ids[A]].add(ids[B])
                                edges += 1
                    elif A not in upmap[B] and A < B and ((A, B) in self.fd or (B, A) in self.fd):
                        # relación sin dirección: solo si una identifica a la otra (no muchos-a-muchos)
                        if ids[B] not in self.peer[ids[A]]:
                            self.peer[ids[A]].add(ids[B])
                            self.peer[ids[B]].add(ids[A])
                            edges += 1
        self.n_edges = edges
        self.finalize()
        hub_thr = max(cfg["hub_min_series"], cfg["hub_series_ratio"] * max(self.n_series, 1))
        self.hubs = {n for n, c in self.node_series.items() if c > hub_thr}

    def finalize(self):
        """Recalcula las relaciones utilizables (límite de fanout) y el nº de hijos."""
        cfg = self.cfg
        fan = cfg["max_parent_fanout"]
        fov = cfg.get("fanout_overrides", {})

        def limited(adj):
            out = {}
            for u, vs in adj.items():
                byl = defaultdict(list)
                for v in vs:
                    byl[self.node_label.get(v, "")].append(v)
                out[u] = {v for L, grp in byl.items() if len(grp) <= fov.get(L, fan) for v in grp}
            return out
        self.up_ok = limited(self.up)
        self.peer_ok = limited(self.peer)
        # nº de hijos de cada nodo por etiqueta (para la regla de hermanos)
        self.down_count = defaultdict(Counter)
        for u, vs in self.up.items():
            for v in vs:
                self.down_count[v][self.node_label.get(u, "")] += 1

    # -- helpers ----------------------------------------------------------
    def is_stop(self, k):
        kl = k.lower()
        return kl.startswith("__") or kl in self._stop or any(r.fullmatch(kl) for r in self._stop_re)

    def relabel(self, labels):
        return self.relabel_map(labels)[0]

    def relabel_map(self, labels):
        for when, ren in self._relabel:
            if all(k in labels and r.fullmatch(str(labels[k])) for k, r in when.items()):
                return {ren.get(k, k): v for k, v in labels.items()}, ren
        return labels, {}

    def node_of(self, raw, label):
        """Nodo del grafo que corresponde a la etiqueta 'label' de una serie cruda."""
        lab, ren = self.relabel_map(raw)
        L = ren.get(label, label)
        items = self.clean(lab)
        if L not in items:
            return None
        return self.node_ids(items).get(L)

    def map_cmdb(self, cmdb):
        """CI -> nodo del grafo (y nodo -> CIs, para colgar las alertas de la CMDB)."""
        node = {}
        self.node_cis = defaultdict(set)
        for cid, c in cmdb["cis"].items():
            raws = ([(c["_raw"], c["_label"])] if c.get("_raw") is not None else []) + c.get("_raws", [])
            for raw, lab in raws:
                n = self.node_of(raw, lab)
                if n:
                    node.setdefault(cid, n)
                    self.node_cis[n].add(cid)
                    self.node_label.setdefault(n, c["_label"])
                    self.node_disp.setdefault(n, c["name"])
        return node

    def apply_cmdb(self, cmdb, node=None):
        """Las relaciones del modelo CMDB mandan: se añaden como dependencias y se quita
        la dirección contraria si la inferencia la había deducido al revés."""
        added = flipped = 0
        node = node if node is not None else self.map_cmdb(cmdb)
        informative = set((self.cfg.get("cmdb_model") or {}).get("informative_relations", []))
        # Elementos que no existen en el grafo (p. ej. "Miembro F5" = nodo:puerto) se atraviesan:
        # pool -> miembro -> nodo se convierte en pool -> nodo para correlar.
        adj = defaultdict(list)
        for a, r, b, m in cmdb["rels"]:
            if r not in informative and r != "es el mismo equipo que":
                adj[a].append((r, b))
        pairs = []
        for a, r, b, m in cmdb["rels"]:
            if r in informative:
                continue
            if r == "es el mismo equipo que" or b in node:
                pairs.append((a, r, b))
                continue
            seen, stack = {b}, [b]
            while stack:
                x = stack.pop()
                for _, y in adj.get(x, ()):
                    if y in seen:
                        continue
                    seen.add(y)
                    if y in node:
                        pairs.append((a, r, y))
                    else:
                        stack.append(y)
        for a, r, b in pairs:
            na, nb = node.get(a), node.get(b)
            if not na or not nb or na == nb:
                continue
            if r == "es el mismo equipo que":
                self.peer[na].add(nb)
                self.peer[nb].add(na)
                continue
            if na in self.up.get(nb, ()):
                self.up[nb].discard(na)
                flipped += 1
            if nb not in self.up[na]:
                self.up[na].add(nb)
                added += 1
            self.peer.get(na, set()).discard(nb)
            self.peer.get(nb, set()).discard(na)
        self.finalize()
        self.cmdb_stats = {"CIs del modelo": len(node), "relaciones añadidas": added,
                           "direcciones corregidas": flipped}
        return self.cmdb_stats

    def is_hostish(self, L):
        return self.layer(L) in self.bare_layers or any(r.fullmatch(L) for r in self._bare_re)

    def clean(self, labels):
        out = {}
        for k, v in labels.items():
            if self.is_stop(k):
                continue
            nv = norm_value(v)
            if k == "instance" and nv in getattr(self, "shared_instances", ()):
                continue
            if self.is_hostish(k):
                nv = strip_domain(nv)
                for rx, rp in self._host_rw:
                    nv = rx.sub(rp, nv)
            if nv in self._generic:
                continue
            out[k] = nv
        return out

    def layer(self, L):
        if L not in self._layer_cache:
            lay = self.default_layer
            for i, _, r in self._layers:
                if r.fullmatch(L):
                    lay = i
                    break
            self._layer_cache[L] = lay
        return self._layer_cache[L]

    def layer_name(self, n):
        for i, name, _ in self._layers:
            if i == n:
                return name
        return self.cfg["default_layer_name"]

    def _is_above(self, p, c):
        dc, dp = self.distinct[c], self.distinct[p]
        if (c, p) in self.fd and dc > dp:
            return True
        if (p, c) in self.fd and dp > dc:
            return False
        return self.metrics[c] < self.metrics[p]       # inclusión estricta de métricas

    def _closure(self, c):
        seen, stack = set(), list(self.above.get(c, ()))
        while stack:
            p = stack.pop()
            if p in seen or p == c:
                continue
            seen.add(p)
            stack.extend(self.above.get(p, ()))
        return seen

    def upL(self, a, b):
        """¿La etiqueta b está aguas arriba de la etiqueta a?"""
        key = (a, b)
        if key not in self._up_cache:
            la, lb = self.layer(a), self.layer(b)
            if la in self.alias_layers and lb in self.alias_layers:
                r = False
            elif (a in self.wwn_labels or b in self.wwn_labels) and la == lb:
                r = False         # el WWN es alias de la LUN de su misma capa
            elif la != lb:
                r = lb < la
            else:
                r = b in self.anc.get(a, ())
            self._up_cache[key] = r
        return self._up_cache[key]

    def is_bare(self, L, v):
        return (self.layer(L) in self.bare_layers or identity_like(v)
                or any(r.fullmatch(L) for r in self._bare_re))

    def node_ids(self, items):
        ids = {}

        def nid(L, depth=0):
            if L in ids:
                return ids[L]
            v = items[L]
            if self.is_bare(L, v):
                r = v
                if self.cfg.get("host_compact") and not IPV4_RE.match(v) and ":" not in v \
                        and not re.fullmatch(r"[0-9a-f]{16,}", v):
                    r = re.sub(r"[-_.]", "", v)
            else:
                r = v if long_name(v) else f"{L}={v}"
                q = self.qual.get((L, v))
                if q and depth < 5:
                    for P in q:
                        if P in items:
                            r += "@" + nid(P, depth + 1)
                            break
            ids[L] = r
            return r

        for L in items:
            nid(L)
        return ids

    def primary_label(self, alertname, labels):
        if not labels:
            return None
        known = [L for L in labels if L in self.labels or self.layer(L) != self.default_layer]
        cand = known or list(labels)
        # "instance" suele ser la IP/URL de scrape del propio equipo: mejor su nombre si lo hay
        if "instance" in cand and len(cand) > 1:
            cand = [L for L in cand if L != "instance"]
        toks = camel_tokens(alertname)
        kw = [L for L in cand if any(t in toks for t in re.split(r"[_\W]+", L.lower()) if len(t) > 1)]
        leafs = [L for L in cand if not any(K != L and self.upL(K, L) for K in cand)] or cand
        # el nombre de la alerta desempata entre hojas; si nombra un padre ("Cluster..."), manda la hoja
        kw_leafs = [L for L in leafs if L in kw]
        if kw_leafs:
            leafs = kw_leafs
        elif kw and len(leafs) > 1:
            leafs = [L for L in leafs if any(self.upL(L, K) for K in kw)] or leafs
        leafs.sort(key=lambda L: (-self.distinct.get(L, 0), self.layer(L), L))
        return leafs[0]

    def label_hierarchy(self):
        """Relaciones directas entre etiquetas (para mostrarlas en la web)."""
        rel = []
        for c in sorted(self.labels):
            ups = [p for p in self.labels if p != c and p in self.cooc[c] and self.upL(c, p)]
            direct = [p for p in ups if not any(p != q and self.upL(q, p) for q in ups)]
            for p in sorted(direct):
                rel.append((c, p))
        return rel


# --------------------------------------------------------------------------
# Correlación
# --------------------------------------------------------------------------
class Correlator:
    def __init__(self, topo, alerts, cfg):
        self.t = topo
        self.cfg = cfg
        excl = set(cfg["exclude_alertnames"])
        self.alerts = [a for a in alerts if a["name"] not in excl]

    def _reach(self, starts):
        t, maxh = self.t, self.cfg["max_hops"]
        dist = {s: 0 for s in starts}
        prev = {s: None for s in starts}
        dq = deque(starts)
        while dq:
            u = dq.popleft()
            if dist[u] >= maxh or (u in t.hubs and dist[u] > 0):
                continue
            for v in chain(t.up_ok.get(u, ()), t.peer_ok.get(u, ())):
                if v in dist or v in t.hubs:
                    continue
                dist[v] = dist[u] + 1
                prev[v] = u
                dq.append(v)
        return dist, prev

    @staticmethod
    def _path(prev, node):
        p = []
        while node is not None:
            p.append(node)
            node = prev.get(node)
        return list(reversed(p))

    def disp(self, n):
        return self.t.node_disp.get(n, n)

    def _canonical(self, a):
        """Elemento principal de la alerta, resolviendo alias (datastore -> LUN que lo soporta)."""
        if not a["P"]:
            return None
        p = next(iter(a["P"]))
        return self.t.alias.get(p, p)

    def _parents(self, a):
        """Padres directos del elemento: por topología y por las propias etiquetas de la alerta."""
        t, pl = self.t, a["primary"]
        up1 = set(chain.from_iterable(t.up_ok.get(p, ()) for p in a["P"]))
        if pl:
            ups = [L for L in a["ids"] if L != pl and t.upL(pl, L)
                   and (t.layer(L) not in t.alias_layers or t.layer(pl) in t.alias_layers)]
            for L in ups:
                if not any(K != L and t.upL(K, L) for K in ups):
                    up1.add(a["ids"][L])
        return up1

    def _entry(self, members, canon):
        """Agrupa en una sola entrada todas las alertas de un mismo elemento (deduplicación)."""
        far = datetime.max.replace(tzinfo=timezone.utc)
        members = sorted(members, key=lambda m: (-m["sev_rank"], m["starts"] or far))
        e = dict(members[0])
        e["members"] = members
        e["names"] = Counter(m["name"] for m in members)
        e["P"] = {canon} if canon else set(members[0]["P"])
        e["N"] = set().union(*(m["N"] for m in members)) | e["P"]
        e["starts"] = min((m["starts"] for m in members if m["starts"]), default=None)
        e["sev_rank"] = max(m["sev_rank"] for m in members)
        e["layer"] = min(m["layer"] for m in members)
        e["C"] = set().union(*(self._parents(m) for m in members)) - self.t.hubs - e["P"]
        return e

    def _device(self, a):
        for L in self.cfg["device_labels"]:
            if a["labels"].get(L):
                return f"{L}={a['labels'][L]}"
        return "—"

    def run(self):
        t, A0 = self.t, self.alerts
        # -- elementos de cada alerta ----------------------------------------
        for a in A0:
            a["labels"] = t.relabel(a["labels"])
            items = a["items"] = t.clean(a["labels"])
            a["ids"] = {L: n for L, n in t.node_ids(items).items() if n not in t.hubs}
        # etiquetas globales que solo están en las alertas (external_labels...)
        cnt = Counter(n for a in A0 for L, n in a["ids"].items() if L not in t.labels)
        lim = max(3, self.cfg["alert_global_ratio"] * len(A0))
        global_nodes = {n for n, c in cnt.items() if c > lim}
        for a in A0:
            a["ids"] = {L: n for L, n in a["ids"].items() if n not in global_nodes}
            for L, n in a["ids"].items():
                t.node_disp.setdefault(n, a["items"].get(L, n) if t.is_bare(L, a["items"].get(L, n)) else f"{L}={a['labels'].get(L)}")
                t.node_label.setdefault(n, L)
            pl = t.primary_label(a["name"], list(a["ids"]))
            a["primary"] = pl
            a["P"] = {a["ids"][pl]} if pl else set()
            a["N"] = set(a["ids"].values())
            a["layer"] = t.layer(pl) if pl else t.default_layer

        # -- deduplicación: una entrada por elemento --------------------------
        by, solo = defaultdict(list), []
        for a in A0:
            k = self._canonical(a)
            if k:
                by[k].append(a)
            else:
                solo.append(a)
        A = [self._entry(m, k) for k, m in by.items()] + [self._entry([a], None) for a in solo]
        ncr = self.cfg.get("non_causal_alert_regex")
        ncr = re.compile(ncr) if ncr else None
        for e in A:
            e["R"], e["prev"] = self._reach(e["P"])
            e["noncausal"] = bool(ncr) and all(ncr.fullmatch(nm) for nm in e["names"])

        # Regla de hermanos: el padre común solo cuenta si alerta una parte
        # significativa de sus hijos (evita "387 pools caídos = el tenant está mal").
        cnt_c = Counter((nm, u) for e in A for nm in e["names"] for u in e["C"])
        ratio = self.cfg["sibling_min_ratio"]
        for e in A:
            plab = t.node_label.get(next(iter(e["P"]))) if e["P"] else None
            keep = set()
            for u in e["C"]:
                total = t.down_count.get(u, {}).get(plab, 0)
                n_al = max(cnt_c[(nm, u)] for nm in e["names"])
                if n_al >= 2 and (total == 0 or n_al / total >= ratio):
                    keep.add(u)
            e["C"] = keep

        # -- pares relacionados ----------------------------------------------
        n = len(A)
        parent = list(range(n))

        def find(x):
            while parent[x] != x:
                parent[x] = parent[parent[x]]
                x = parent[x]
            return x

        upstream = defaultdict(set)       # i -> {j}: i está aguas arriba de j
        links = []                        # (i, j, texto, kind) que unieron entradas
        common_of = defaultdict(set)      # i -> padres comunes con hermanos
        window = self.cfg["time_window_minutes"] * 60
        for i in range(n):
            for j in range(i + 1, n):
                ai, aj = A[i], A[j]
                if window and ai["starts"] and aj["starts"] and \
                        abs((ai["starts"] - aj["starts"]).total_seconds()) > window:
                    continue
                rel = None
                if ai["noncausal"] or aj["noncausal"]:
                    # solo pueden agruparse como "misma alerta en hermanos" (va al resumen)
                    if set(ai["names"]) & set(aj["names"]) and ai["C"] & aj["C"]:
                        common = sorted(ai["C"] & aj["C"], key=lambda x: -t.layer(t.node_label.get(x, "")))
                        common_of[i].add(common[0])
                        common_of[j].add(common[0])
                    continue
                if ai["P"] & aj["N"]:
                    nd = next(iter(ai["P"] & aj["N"]))
                    upstream[i].add(j)
                    rel = ("direct", f"{aj['name']} lleva en sus etiquetas el elemento de {ai['name']}: {self.disp(nd)}")
                elif aj["P"] & ai["N"]:
                    nd = next(iter(aj["P"] & ai["N"]))
                    upstream[j].add(i)
                    rel = ("direct", f"{ai['name']} lleva en sus etiquetas el elemento de {aj['name']}: {self.disp(nd)}")
                else:
                    hit = [x for x in aj["P"] if x in ai["R"]]
                    if hit:
                        upstream[j].add(i)
                        path = self._path(ai["prev"], min(hit, key=lambda x: ai["R"][x]))
                        rel = ("path", " → ".join(self.disp(x) for x in path))
                    else:
                        hit = [x for x in ai["P"] if x in aj["R"]]
                        if hit:
                            upstream[i].add(j)
                            path = self._path(aj["prev"], min(hit, key=lambda x: aj["R"][x]))
                            rel = ("path", " → ".join(self.disp(x) for x in path))
                        elif set(ai["names"]) & set(aj["names"]) and ai["C"] & aj["C"]:
                            common = sorted(ai["C"] & aj["C"],
                                            key=lambda x: -t.layer(t.node_label.get(x, "")))
                            rel = ("sibling", f"misma alerta en elementos que cuelgan de {self.disp(common[0])}")
                            common_of[i].add(common[0])
                            common_of[j].add(common[0])
                if rel:
                    ri, rj = find(i), find(j)
                    if ri != rj:
                        parent[ri] = rj
                        links.append((i, j, rel[1], rel[0]))

        comp = defaultdict(list)
        for i in range(n):
            comp[find(i)].append(i)

        far_future = datetime.max.replace(tzinfo=timezone.utc)
        groups, singles = [], []
        for members in comp.values():
            mset = set(members)
            kinds = {k for i, j, _, k in links if i in mset}
            # Solo es incidencia si hay relación causa -> efecto entre elementos distintos.
            # Los "hermanos" (misma alerta bajo el mismo padre) van al resumen de avisos.
            if len(members) == 1 or not (kinds & {"direct", "path"}):
                for x in members:
                    A[x]["common"] = [self.disp(c) for c in common_of.get(x, ())]
                    singles.append(A[x])
                continue
            max_up = max((len(upstream[x] & mset) for x in members), default=0) or 1

            def key(x):
                cover = round(len(upstream[x] & mset) / max_up, 1)
                return (-cover, -A[x]["sev_rank"], A[x]["layer"], A[x]["starts"] or far_future)

            ordered = sorted(members, key=key)
            ra = A[ordered[0]]
            nup = len(upstream[ordered[0]] & mset)
            earliest = min((A[x]["starts"] for x in members if A[x]["starts"]), default=None)
            reasons = []
            if nup:
                reasons.append(f"su elemento está aguas arriba de {nup} de los {len(members) - 1} otros elementos")
            reasons.append(f"capa: {t.layer_name(ra['layer'])}")
            if ra["starts"] and ra["starts"] == earliest:
                reasons.append("es la más antigua")
            groups.append({
                "alerts": [A[x] for x in ordered],
                "root": ra,
                "root_reason": "; ".join(reasons),
                "links": [(A[i], A[j], txt, kind) for i, j, txt, kind in links if i in mset],
                "max_sev": max(A[x]["sev_rank"] for x in members),
                "earliest": earliest,
                "n_alerts": sum(len(A[x]["members"]) for x in members),
            })
        groups.sort(key=lambda g: (-g["max_sev"], -g["n_alerts"], g["earliest"] or far_future))

        # -- resumen de avisos sin relación causal: por (alerta, equipo) --------
        rows = defaultdict(list)
        for e in singles:
            rows[e["name"]].append(e)
        summary = []
        for name, es in rows.items():
            commons = Counter(c for e in es for c in e.get("common", []))
            devs = Counter(self._device(e) for e in es)
            dev = ", ".join(f"{d}" + (f" ({c})" if len(devs) > 1 else "") for d, c in devs.most_common())
            summary.append({
                "name": name, "device": dev, "devices": devs, "entries": sorted(es, key=lambda e: (-e["sev_rank"], e["starts"] or far_future)),
                "n_alerts": sum(len(e["members"]) for e in es),
                "max_sev": max(e["sev_rank"] for e in es),
                "severity": max(es, key=lambda e: e["sev_rank"])["severity"],
                "earliest": min((e["starts"] for e in es if e["starts"]), default=None),
                "commons": commons.most_common(3),
            })
        summary.sort(key=lambda r: (-r["max_sev"], -r["n_alerts"]))
        return {"groups": groups, "summary": summary, "n_alerts": len(A0), "n_elements": len(A)}


# --------------------------------------------------------------------------
# HTML
# --------------------------------------------------------------------------
CSS = """
:root{--bg:#f6f7f9;--card:#fff;--ink:#1d2330;--mut:#667085;--line:#e4e7ec;
--crit:#d92d20;--high:#f04438;--warn:#dc6803;--info:#1570ef;--none:#98a2b3;--ok:#079455}
@media (prefers-color-scheme:dark){:root{--bg:#0f1218;--card:#171b23;--ink:#e6e8ec;--mut:#98a2b3;--line:#2a303b}}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:var(--bg);color:var(--ink)}
header.top{padding:18px 28px;border-bottom:1px solid var(--line);background:var(--card);display:flex;flex-wrap:wrap;gap:16px;align-items:center;justify-content:space-between}
h1{font-size:18px;margin:0}.sub{color:var(--mut);font-size:12px}
.kpis{display:flex;gap:10px;flex-wrap:wrap}.kpi{border:1px solid var(--line);border-radius:10px;padding:6px 12px;min-width:92px}
.kpi b{display:block;font-size:20px}.kpi span{color:var(--mut);font-size:12px}
main{padding:20px 28px;max-width:1500px;margin:0 auto}
input#q{width:100%;max-width:420px;padding:8px 12px;border:1px solid var(--line);border-radius:8px;background:var(--card);color:var(--ink);font-size:14px}
h2{font-size:15px;margin:26px 0 10px}
.group{background:var(--card);border:1px solid var(--line);border-left:5px solid var(--none);border-radius:10px;margin:12px 0;overflow:hidden}
.group.s4{border-left-color:var(--crit)}.group.s3{border-left-color:var(--high)}.group.s2{border-left-color:var(--warn)}.group.s1{border-left-color:var(--info)}
.gh{padding:12px 16px;display:flex;gap:10px;align-items:center;flex-wrap:wrap}
.gh .t{font-weight:600;font-size:15px}.gh .m{color:var(--mut);font-size:12px}
.root{margin:0 16px 10px;padding:10px 12px;border-radius:8px;background:color-mix(in srgb,var(--crit) 8%,transparent)}
.root small{display:block;color:var(--mut)}
table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:7px 10px;border-top:1px solid var(--line);vertical-align:top}
th{font-size:11px;text-transform:uppercase;letter-spacing:.04em;color:var(--mut);font-weight:600}
tr.isroot td{font-weight:600}
.sev{display:inline-block;padding:1px 8px;border-radius:99px;font-size:11px;font-weight:700;color:#fff;background:var(--none);text-transform:uppercase}
.sev.s4{background:var(--crit)}.sev.s3{background:var(--high)}.sev.s2{background:var(--warn)}.sev.s1{background:var(--info)}
.dup{display:inline-block;margin-left:6px;padding:0 6px;border-radius:99px;font-size:11px;border:1px solid var(--line);color:var(--mut)}
.st{font-size:11px;color:var(--mut)}.ctx{color:var(--mut);font-size:12px}
code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12px}
details{margin:0 16px 12px}summary{cursor:pointer;color:var(--mut);font-size:12px}
details.more{margin:0}details.more>summary{padding:8px 16px}
details ul{margin:6px 0 0;padding-left:18px;font-size:12px}details li{margin:3px 0}
.box{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 16px}
.chips code{display:inline-block;margin:2px 4px 2px 0;padding:1px 6px;border:1px solid var(--line);border-radius:6px}
.lab{cursor:help;border-bottom:1px dotted var(--mut)}
td details{margin:4px 0 0}
.hidden{display:none}
footer{color:var(--mut);font-size:12px;padding:20px 28px}
"""

JS = """
const q=document.getElementById('q');
q&&q.addEventListener('input',()=>{const v=q.value.toLowerCase().trim();
document.querySelectorAll('[data-s]').forEach(e=>{e.classList.toggle('hidden',v&&!e.dataset.s.includes(v))});});
"""


def esc(s):
    return html.escape(str(s if s is not None else ""), quote=True)


def fmt_time(dt):
    return dt.astimezone().strftime("%d/%m %H:%M") if dt else ""


def _search(e):
    return " ".join(m["name"] + " " + " ".join(f"{k}={v}" for k, v in m["labels"].items()) + " " + m["summary"]
                    for m in e["members"]).lower()


DS_BACKING = {}


def entry_row(e, now, is_root=False):
    pl = e.get("primary")
    ent = f"<code>{esc(pl)}={esc(e['labels'].get(pl))}</code>" if pl else "<span class='ctx'>—</span>"
    ctx = [f"{k}={v}" for k, v in e["labels"].items() if k in e.get("ids", {}) and k != pl]
    others = [f"{n}" + (f" ×{c}" if c > 1 else "") for n, c in e["names"].items() if n != e["name"]]
    dup = len(e["members"])
    all_labels = ", ".join(f"{k}={v}" for k, v in sorted(e["labels"].items()))
    alias = ""
    if len({m["primary"] and m["labels"].get(m["primary"]) for m in e["members"]}) > 1:
        alias = "<div class='ctx'>mismo objeto: " + esc(", ".join(sorted({
            f"{m['primary']}={m['labels'].get(m['primary'])}" for m in e["members"] if m["primary"]}))) + "</div>"
    for m in e["members"]:
        ds = m["labels"].get("datastore")
        if ds:
            bk = DS_BACKING.get((m["labels"].get("vcenter"), ds))
            if bk:
                alias += f"<div class='ctx'><b>Almacenamiento:</b> {esc(bk)}</div>"
                break
    return (
        f"<tr class='{'isroot' if is_root else ''}' data-s='{esc(_search(e))}'>"
        f"<td>{'★' if is_root else ''}</td>"
        f"<td><span class='sev s{e['sev_rank']}'>{esc(e['severity'])}</span></td>"
        f"<td><span class='lab' title='{esc(all_labels)}'>{esc(e['name'])}</span>"
        f"{f'<span class=dup>×{dup}</span>' if dup > 1 else ''}"
        f"{'<div class=ctx>+ ' + esc(', '.join(others)) + '</div>' if others else ''}"
        f"<div class='st'>{esc(e['state'])}</div></td>"
        f"<td>{ent}<div class='ctx'>{esc(' · '.join(ctx))}</div>{alias}</td>"
        f"<td>{esc(fmt_time(e['starts']))}<div class='ctx'>{esc(human_ago(e['starts'], now))}</div></td>"
        f"<td>{esc(e['summary'])}</td></tr>"
    )


TABLE_HEAD = ("<table><thead><tr><th></th><th>Sev.</th><th>Alerta</th><th>Elemento</th>"
              "<th>Inicio</th><th>Resumen</th></tr></thead><tbody>")


def render_html(res, topo, meta, refresh=None):
    now = datetime.now(timezone.utc)
    DS_BACKING.clear()
    DS_BACKING.update(getattr(topo, "ds_backing", {}) or {})
    groups, summary = res["groups"], res["summary"]
    n_alerts = res["n_alerts"]
    items = len(groups) + len(summary)
    reduction = (1 - items / n_alerts) * 100 if n_alerts else 0
    out = ["<!DOCTYPE html><html lang='es'><head><meta charset='utf-8'>",
           "<meta name='viewport' content='width=device-width,initial-scale=1'>",
           f"<meta http-equiv='refresh' content='{int(refresh)}'>" if refresh else "",
           f"<title>{esc(UI_TITLE)} · lista</title><style>", CSS, "</style></head><body>",
           "<header class='top'><div><h1>Alertas correladas</h1>",
           f"<div class='sub'>Fuente: {esc(meta['source'])} · generado {esc(fmt_time(now))}"
           f"{' · refresco ' + str(int(refresh)) + ' s' if refresh else ''}</div></div>",
           "<div class='kpis'>",
           f"<div class='kpi'><b>{n_alerts}</b><span>alertas</span></div>",
           f"<div class='kpi'><b>{res['n_elements']}</b><span>elementos afectados</span></div>",
           f"<div class='kpi'><b>{len(groups)}</b><span>incidencias</span></div>",
           f"<div class='kpi'><b>{len(summary)}</b><span>tipos de aviso</span></div>",
           f"<div class='kpi'><b>{reduction:.0f}%</b><span>menos ruido</span></div>",
           "</div></header><main>",
           "<input id='q' type='search' placeholder='Filtrar por alerta, etiqueta, host, cabina, datastore...'>"]

    out.append(f"<h2>Incidencias: alertas con relación causa-efecto ({len(groups)})</h2>")
    if not groups:
        out.append("<div class='box'>No hay alertas relacionadas entre elementos distintos.</div>")
    for gi, g in enumerate(groups, 1):
        ra = g["root"]
        search = " ".join(_search(e) for e in g["alerts"])
        out.append(f"<section class='group s{g['max_sev']}' data-s='{esc(search)}'>")
        out.append(f"<div class='gh'><span class='t'>Incidencia {gi}</span>"
                   f"<span class='m'>{g['n_alerts']} alertas en {len(g['alerts'])} elementos · desde "
                   f"{esc(fmt_time(g['earliest']))} ({esc(human_ago(g['earliest'], now))})</span></div>")
        pl = ra.get("primary")
        el = f" en <code>{esc(pl)}={esc(ra['labels'].get(pl))}</code>" if pl else ""
        out.append(f"<div class='root'>Causa probable: <b>{esc(ra['name'])}</b>{el}"
                   f"<small>{esc(g['root_reason'])}</small></div>")
        out.append(TABLE_HEAD)
        show = 10
        for k, e in enumerate(g["alerts"]):
            if k == show:
                out.append("</tbody></table><details class='more'><summary>Ver los otros "
                           f"{len(g['alerts']) - show} elementos</summary>" + TABLE_HEAD)
            out.append(entry_row(e, now, is_root=(e is ra)))
        out.append("</tbody></table>")
        if len(g["alerts"]) > show:
            out.append("</details>")
            names = Counter(n for e in g["alerts"] for n in e["names"].elements())
            out.append("<div class='sub' style='margin:0 16px 8px'>Resumen: " +
                       ", ".join(f"{esc(n)} ×{c}" for n, c in names.most_common()) + "</div>")
        out.append("<details><summary>Por qué están relacionadas</summary><ul>")
        for a, b, txt, kind in g["links"][:40]:
            out.append(f"<li><b>{esc(a['name'])}</b> ↔ <b>{esc(b['name'])}</b>: <code>{esc(txt)}</code></li>")
        if len(g["links"]) > 40:
            out.append(f"<li>… y {len(g['links']) - 40} relaciones más</li>")
        out.append("</ul></details></section>")

    out.append(f"<h2>Avisos sin relación causa-efecto, agrupados por tipo ({len(summary)})</h2>")
    out.append("<div class='group' style='border-left-color:var(--line)'><table><thead><tr><th>Sev.</th>"
               "<th>Alerta</th><th>Equipos</th><th>Elementos</th><th>Desde</th><th>Detalle</th></tr></thead><tbody>")
    for r in summary:
        search = " ".join(_search(e) for e in r["entries"])
        common = ""
        if r["commons"]:
            common = "<div class='ctx'>posible causa común: " + esc(", ".join(c for c, _ in r["commons"])) + "</div>"
        def bk(e):
            v = DS_BACKING.get((e["labels"].get("vcenter"), e["labels"].get("datastore")))
            return f"<br><span class='ctx'>→ {esc(v)}</span>" if v else ""
        lis = "".join(
            f"<li><code>{esc(e['primary'])}={esc(e['labels'].get(e['primary']))}</code>"
            f"{' ×' + str(len(e['members'])) if len(e['members']) > 1 else ''} — {esc(e['summary'])}{bk(e)}</li>"
            if e.get("primary") else f"<li>{esc(e['summary'])}</li>" for e in r["entries"][:200])
        n_el = len(r["entries"])
        alerts_txt = f" <span class='ctx'>({r['n_alerts']} alertas)</span>" if r["n_alerts"] != n_el else ""
        out.append(
            f"<tr data-s='{esc(search)}'><td><span class='sev s{r['max_sev']}'>{esc(r['severity'])}</span></td>"
            f"<td>{esc(r['name'])}{common}</td><td class='ctx'>{esc(r['device'])}</td>"
            f"<td>{n_el}{alerts_txt}</td>"
            f"<td>{esc(fmt_time(r['earliest']))}<div class='ctx'>{esc(human_ago(r['earliest'], now))}</div></td>"
            f"<td><details><summary>{'ver ' + str(n_el) + ' elementos' if n_el > 1 else 'ver'}</summary>"
            f"<ul>{lis}</ul></details></td></tr>")
    out.append("</tbody></table></div>")

    # Topología
    hier = topo.label_hierarchy()
    by_layer = defaultdict(list)
    for L in sorted(topo.labels):
        by_layer[topo.layer(L)].append(L)
    out.append("<h2>Topología deducida de las métricas</h2><div class='box'>")
    out.append(f"<p class='sub'>{topo.n_series} series analizadas · {len(topo.node_series)} elementos · "
               f"{topo.n_edges} relaciones · {len(topo.qual)} nombres ambiguos cualificados con su padre · "
               f"{len(topo.hubs)} elementos globales ignorados</p>")
    cs = getattr(topo, "cmdb_stats", None)
    if cs:
        out.append("<p class='sub'>Modelo CMDB aplicado: " + ", ".join(f"{esc(k)}: {v}" for k, v in cs.items()) + "</p>")
    bs = getattr(topo, "bridge_stats", None)
    if bs:
        out.append("<p class='sub'>Datastores enlazados con su LUN por nombre: " +
                   ", ".join(f"{esc(k)}: {v}" for k, v in sorted(bs.items(), key=lambda kv: -kv[1])) + "</p>")
    if topo.labels:
        out.append("<p><b>Etiquetas por capa</b></p>")
        for lay in sorted(by_layer):
            out.append(f"<div class='chips'><span class='sub'>{esc(topo.layer_name(lay))}:</span> "
                       + "".join(f"<code>{esc(L)} ({topo.distinct[L]})</code>" for L in by_layer[lay]) + "</div>")
        out.append("<details><summary>Relaciones entre etiquetas (hijo → depende de)</summary><ul>")
        for c, p in hier:
            out.append(f"<li><code>{esc(c)}</code> → <code>{esc(p)}</code></li>")
        out.append("</ul></details>")
    out.append("</div></main>")
    out.append(f"<footer>correlador_alertas.py v{VERSION} · prototipo</footer>")
    out.append(f"<script>{JS}</script></body></html>")
    return "".join(out)


def result_json(res):
    def a2d(a):
        return {"alertname": a["name"], "severity": a["severity"], "state": a["state"],
                "startsAt": a["starts"].isoformat() if a["starts"] else None,
                "element": a.get("primary") and f"{a['primary']}={a['labels'].get(a['primary'])}",
                "labels": a["labels"], "summary": a["summary"]}

    def e2d(e):
        d = a2d(e)
        d["alerts"] = [a2d(m) for m in e["members"]]
        return d
    return {
        "incidents": [{"root_cause": e2d(g["root"]), "reason": g["root_reason"],
                       "elements": [e2d(e) for e in g["alerts"]],
                       "links": [{"a": a["name"], "b": b["name"], "why": t} for a, b, t, _ in g["links"]]}
                      for g in res["groups"]],
        "summary": [{"alertname": r["name"], "device": r["device"], "elements": len(r["entries"]),
                     "alerts": r["n_alerts"], "severity": r["severity"],
                     "common_parent": [c for c, _ in r["commons"]],
                     "items": [e2d(e) for e in r["entries"]]} for r in res["summary"]],
        "n_alerts": res["n_alerts"], "n_elements": res["n_elements"],
    }


# --------------------------------------------------------------------------
# CMDB a partir de las métricas
# --------------------------------------------------------------------------
def build_cmdb(series, cfg, alerts=None, values=None):
    """Aplica el modelo declarativo cfg["cmdb_model"] a las series y devuelve
    {"cis": {id: {...}}, "rels": [(origen, relación, destino, método)]}.

    Cada regla del modelo:
      {"metric": "nombre" | "metric_regex": "regex", "where": {etiqueta: regex},
       "cis":  {alias: {"type": "VM", "key": [etiquetas], "name": etiqueta,
                        "attrs": {"Nombre visible": etiqueta}, "set_name": bool}},
       "rels": [[alias_origen, "relación", alias_destino], ...]}   # origen depende de destino
    """
    model = cfg.get("cmdb_model") or {}
    rules = []
    for r in model.get("rules", []):
        rules.append((r.get("metric"), re.compile(r["metric_regex"]) if r.get("metric_regex") else None,
                      {k: re.compile(v) for k, v in r.get("where", {}).items()}, r))
    # etiquetas calculadas: {"nueva": ["origen", "regex con un grupo"]}
    # etiquetas calculadas: ["origen", "regex"], {"format": "{a}:{b}"} o {"from": "origen", "map": {...}}
    comp = {id(r): [(k, v, None) if isinstance(v, dict) else (k, v[0], re.compile(v[1]))
                    for k, v in r.get("computed", {}).items()] for *_, r in rules}
    cis, rels, relset = {}, [], set()
    vidx = {frozenset((k, v) for k, v in x.items() if k != "__value__"): x.get("__value__") for x in (values or ())}
    rel_attrs = defaultdict(dict)      # (origen, relación, destino) -> {atributo: valor}

    def ci(spec, x):
        if not all(x.get(k) for k in spec["key"]):
            return None
        cid = spec["type"] + "|" + "|".join(x[k] for k in spec["key"])
        c = cis.get(cid)
        name = x.get(spec.get("name", spec["key"][-1]), "")
        if c is None:
            c = cis[cid] = {"id": cid, "type": spec["type"], "name": name, "attrs": {}, "sources": set(),
                            "_raw": x, "_label": spec.get("name", spec["key"][-1]), "_raws": []}
        elif spec.get("set_name") and name:
            c["name"] = name
        if x.get("__name__") == "ALERTS" and len(c["_raws"]) < 200:
            c["_raws"].append((x, spec.get("name", spec["key"][-1])))   # para colgar cada alerta de su CI
        for disp, lab in spec.get("attrs", {}).items():
            if x.get(lab):
                c["attrs"][disp] = x[lab]
        if spec.get("value_attr") and vidx:
            val = vidx.get(frozenset((k, v) for k, v in x.items() if not k.startswith("__") or k == "__name__"))
            if val is not None and val == val:
                sval = str(int(val)) if float(val).is_integer() else f"{val:g}"
                c["attrs"][spec["value_attr"]] = spec.get("value_map", {}).get(sval, sval)
        c["sources"].add(x.get("__name__", ""))
        return cid

    def rel(a, r, b, method="métricas"):
        if a and b and a != b and (a, r, b) not in relset:
            relset.add((a, r, b))
            rels.append((a, r, b, method))

    by_name = defaultdict(list)
    for x in series:
        by_name[x.get("__name__", "")].append(x)
    alert_rows = [{**a, "__name__": "ALERTS", "__ref": str(k)} for k, a in enumerate(alerts or ())]
    alert_ci = defaultdict(list)       # nº de alerta -> CIs a los que se cuelga (en orden de reglas)
    for metric, mrx, where, r in rules:
        if r.get("source") == "alerts":
            groups_ = [alert_rows]
        else:
            groups_ = [by_name.get(n, ()) for n in ([metric] if metric else [n for n in by_name if mrx.fullmatch(n)])]
        for rows in groups_:
            for x in rows:
                if not all(k in x and rx.fullmatch(str(x[k])) for k, rx in where.items()):
                    continue
                if comp[id(r)]:
                    x = dict(x)
                    for k, src, rx in comp[id(r)]:
                        if rx is None:
                            if "sorted" in src:            # {"sorted": ["instance", "peer"], "sep": " / "}
                                vals = [x.get(k_) for k_ in src["sorted"]]
                                if all(vals):
                                    x[k] = src.get("sep", "/").join(sorted(vals))
                                continue
                            if "first" in src:             # {"first": ["exported_instance", "instance"], "regex": "^([^:]+)"}
                                for k_ in src["first"]:
                                    v_ = x.get(k_)
                                    if v_:
                                        m_ = re.match(src["regex"], v_) if src.get("regex") else None
                                        x[k] = (m_.group(1) if m_ and m_.groups() else v_)
                                        break
                                continue
                            if "map" in src or "map_regex" in src:
                                # {"from": "vbsite", "map": {"00": "CICA"}} o "map_regex": {"^10\\.47\\.": "CICA"}
                                v_ = x.get(src["from"], "")
                                val = (src.get("map") or {}).get(v_)
                                if not val:
                                    val = next((sv for rx_, sv in (src.get("map_regex") or {}).items()
                                                if v_ and re.search(rx_, v_)), None)
                                val = val or src.get("default")
                                if val:
                                    x[k] = val
                                continue
                            try:                           # {"format": "{node}:{port}"}
                                x[k] = src["format"].format_map(x)
                            except (KeyError, ValueError):
                                pass
                            continue
                        m = rx.match(x.get(src, ""))
                        if m and m.group(1):
                            x[k] = m.group(1)
                ids = {alias: ci(spec, x) for alias, spec in r["cis"].items()}
                if x.get("__ref") is not None:
                    tgt = ids.get(r.get("attach", next(iter(r["cis"]))))
                    if tgt and tgt not in alert_ci[int(x["__ref"])]:
                        alert_ci[int(x["__ref"])].append(tgt)
                for spec_rel in r.get("rels", []):
                    a, rr, b = spec_rel[:3]
                    rel(ids.get(a), rr, ids.get(b), r.get("method", "métricas"))
                    if len(spec_rel) > 3 and ids.get(a) and ids.get(b):
                        for disp, lab in spec_rel[3].items():
                            if x.get(lab):
                                rel_attrs[(ids[a], rr, ids[b])][disp] = x[lab]

    # Datastore -> LUN por nombre (mismo puente que usa el correlador)
    b = cfg.get("datastore_lun_bridge")
    if b and model.get("datastore_type") and model.get("lun_type"):
        extra, _ = build_datastore_lun_bridge(series, b)
        for e in extra:
            ds = f"{model['datastore_type']}|{e['vcenter']}|{e['datastore']}"
            lun = f"{model['lun_type']}|{e['array']}|{e['lun']}"
            if ds in cis and lun in cis:
                rel(ds, "está en", lun, "nombre del datastore")

    # Mismo equipo visto por distintos exporters (ESXi = host en cabina, VM = nodo F5...)
    def hkey(name):
        v = strip_domain(norm_value(name))
        for rx, rp in [(re.compile(a, re.I), b) for a, b in cfg.get("host_rewrite", [])]:
            v = rx.sub(rp, v)
        return v if IPV4_RE.match(v) else re.sub(r"[-_.]", "", v)
    groups = defaultdict(list)
    for c in cis.values():
        if c["type"] in model.get("same_host_types", []):
            groups[hkey(c["name"])].append(c["id"])
    for k, ids in groups.items():
        types = {cis[i]["type"] for i in ids}
        if len(types) > 1:
            ids = sorted(ids)
            for i in ids[1:]:
                rel(i, "es el mismo equipo que", ids[0], "nombre normalizado")
    for c in cis.values():
        c["sources"] = sorted(s_ if s_ != "ALERTS" else "alertas" for s_ in c["sources"])
    out = {"cis": cis, "rels": rels, "alert_ci": dict(alert_ci), "rel_attrs": dict(rel_attrs)}
    apply_relations_files(out, cfg, rel)
    return out


def load_relations_file(path):
    """Fichero de relaciones manuales, una por línea:  Vsys_PA:CEFTA => F5:EMP
    '#' inicia un comentario; un nombre puede llevar sede: Vsys_PA:CENTRAL@CICA."""
    out = []
    with open(path, encoding="utf-8-sig") as f:
        for n, line in enumerate(f, 1):
            line = line.split("#", 1)[0].strip()
            if not line:
                continue
            m = re.fullmatch(r"\s*([^:=]+?)\s*:\s*(.+?)\s*=>\s*([^:=]+?)\s*:\s*(.+?)\s*", line)
            if not m:
                log(f"AVISO: {os.path.basename(path)}:{n}: línea no válida (formato Tipo:Nombre => Tipo:Nombre): {line}")
                continue
            out.append((os.path.basename(path), n, line) + m.groups())
    return out


def apply_relations_files(cmdb, cfg, rel):
    """Añade a la CMDB las relaciones de los ficheros de cfg["relations_file"]["files"].
    Cada prefijo (Vsys_PA, F5...) se traduce a tipos de CI; 'expand' extiende el destino a los
    elementos que cuelgan de él (p. ej. tenants de un organismo) y solo se enlaza dentro de la
    misma sede cuando ambos lados la tienen."""
    rcfg = cfg.get("relations_file") or {}
    files = rcfg.get("files") or []
    if not files:
        return
    cis = cmdb["cis"]
    prefixes = {k.lower(): v for k, v in rcfg.get("prefixes", {}).items()}
    tech_types = {t for ts in (cfg.get("cmdb_model") or {}).get("technologies", {}).values() for t in ts}
    site, _ = cmdb_sites(cmdb, cfg)
    by_type = defaultdict(lambda: defaultdict(list))   # tipo -> nombre normalizado -> [ids]
    for i, c in cis.items():
        by_type[c["type"]][c["name"].lower()].append(i)
    incoming = defaultdict(list)
    for a, r, b, m in cmdb["rels"]:
        incoming[(b, r)].append(a)

    def resolve(prefix, name):
        name, _, st = name.partition("@")
        p = prefixes.get(prefix.lower())
        if p is None and prefix in tech_types:
            p = {"types": [prefix]}
        if p is None:
            return None, f"prefijo desconocido '{prefix}'"
        cand = [name.lower()] + [(pre + name).lower() for pre in p.get("name_prefixes", [])]
        ids = []
        for t in p["types"]:
            for nm in cand:
                ids += by_type[t].get(nm, [])
        if st:
            ids = [i for i in ids if st.upper() in site.get(i, {st.upper()})]
        if not ids:
            return None, f"no hay ningún {'/'.join(p['types'])} llamado '{name}'"
        res = []
        for r in p.get("expand", []):                     # destino -> lo que cuelga de él
            stack = list(ids)
            while stack:
                x = stack.pop()
                for y in incoming.get((x, r), ()):
                    if y not in res:
                        res.append(y)
                        stack.append(y)
        # Si se expande (organismo -> organismo en sede -> tenants) se enlazan los descendientes,
        # no el organismo global: así un vsys de CICA no arrastra a los tenants de ZOCO.
        return (res, True) if res else (list(ids), False), None

    relname = rcfg.get("relation", "pasa por el firewall")
    n_ok = n_rel = 0
    for path in files:
        try:
            lines = load_relations_file(path)
        except OSError as e:
            log(f"AVISO: no se puede leer el fichero de relaciones {path}: {e}")
            continue
        for fname, n, line, lp, ln, rp, rn in lines:
            left, e1 = resolve(lp, ln)
            right, e2 = resolve(rp, rn)
            if e1 or e2:
                log(f"AVISO: {fname}:{n}: {e1 or e2} ({line})")
                continue
            n_ok += 1
            (left, _), (right, expanded) = left, right
            for b in right:                               # el lado derecho depende del izquierdo
                for a in left:
                    sa, sb = site.get(a), site.get(b)
                    if sa and sb and not ((sb <= sa) if expanded else (sa & sb)):
                        continue
                    rel(b, relname, a, f"fichero {fname}")
                    n_rel += 1
    log(f"Ficheros de relaciones: {n_ok} líneas aplicadas, {n_rel} relaciones")


def write_cmdb(cmdb, outdir, meta):
    import csv
    os.makedirs(outdir, exist_ok=True)
    cis, rels = cmdb["cis"], cmdb["rels"]
    with open(os.path.join(outdir, "cmdb_ci.csv"), "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f, delimiter=";")
        w.writerow(["id", "tipo", "nombre", "atributos", "metricas"])
        for c in sorted(cis.values(), key=lambda c: (c["type"], c["name"])):
            w.writerow([c["id"], c["type"], c["name"],
                        " | ".join(f"{k}={v}" for k, v in c["attrs"].items()), ",".join(c["sources"])])
    with open(os.path.join(outdir, "cmdb_relaciones.csv"), "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f, delimiter=";")
        w.writerow(["origen_tipo", "origen", "relacion", "destino_tipo", "destino", "metodo", "atributos"])
        ra = cmdb.get("rel_attrs", {})
        for a, r, b, m in rels:
            w.writerow([cis[a]["type"], cis[a]["name"], r, cis[b]["type"], cis[b]["name"], m,
                        " | ".join(f"{k}={v}" for k, v in ra.get((a, r, b), {}).items())])
    with open(os.path.join(outdir, "cmdb.html"), "w", encoding="utf-8") as f:
        f.write(render_cmdb_html(cmdb, meta))


CMDB_JS = r"""
const D = JSON.parse(document.getElementById('data').textContent);
const C = D.cis, R = D.rels, out = {}, inn = {}, al = {}, SAME = 'es el mismo equipo que';
R.forEach(([a, r, b, m], i) => {
  if (r === SAME) { (al[a] = al[a] || []).push(b); (al[b] = al[b] || []).push(a); return; }
  (out[a] = out[a] || []).push(i); (inn[b] = inn[b] || []).push(i); });
const $ = s => document.querySelector(s), esc = s => String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
let typeFilter = null;
function chip(i){ const c = C[i]; return `<a href="#${i}" class="ci"><span class="ty">${esc(c[1])}</span> ${esc(c[2])}</a>`; }
function renderTypes(){
  const n = {}; C.forEach(c => n[c[1]] = (n[c[1]] || 0) + 1);
  $('#types').innerHTML = Object.entries(n).sort((a,b)=>b[1]-a[1]).map(([t,k]) =>
    `<button data-t="${esc(t)}" class="${typeFilter===t?'on':''}">${esc(t)} <b>${k}</b></button>`).join('');
  document.querySelectorAll('#types button').forEach(b => b.onclick = () => { typeFilter = typeFilter===b.dataset.t ? null : b.dataset.t; renderTypes(); search(); });
}
function search(){
  const q = $('#q').value.toLowerCase().trim(); const res = [];
  for (let i = 0; i < C.length && res.length < 300; i++) {
    const c = C[i]; if (typeFilter && c[1] !== typeFilter) continue;
    if (!q || (c[2] + ' ' + c[1] + ' ' + Object.values(c[3]).join(' ')).toLowerCase().includes(q)) res.push(i);
  }
  $('#list').innerHTML = res.map(i => `<li>${chip(i)}</li>`).join('') || '<li class="mut">Sin resultados</li>';
}
function upTree(i, depth, seen){
  if (depth > 8) return '';
  const items = [];
  (al[i] || []).forEach(b => { if (seen.has(b)) return; seen.add(b);
    items.push(`<li><span class="rel">${SAME}</span> ${chip(b)}${upTree(b, depth+1, seen)}</li>`); });
  (out[i] || []).forEach(k => { const [a, r, b, m] = R[k]; if (seen.has(b)) return;
    const s2 = new Set(seen); s2.add(b);
    items.push(`<li><span class="rel">${esc(r)}</span> ${chip(b)}${m!=='métricas'?` <span class="mut">(${esc(m)})</span>`:''}${upTree(b, depth+1, s2)}</li>`); });
  return items.length ? '<ul>' + items.join('') + '</ul>' : '';
}
function downAll(i){
  const seen = new Set([i]), q = [i], cnt = {};
  while (q.length) { const x = q.shift();
    (al[x] || []).forEach(a => { if (!seen.has(a)) { seen.add(a); q.push(a); } });
    (inn[x] || []).forEach(k => { const a = R[k][0];
    if (!seen.has(a)) { seen.add(a); q.push(a); cnt[C[a][1]] = (cnt[C[a][1]] || 0) + 1; } }); }
  return cnt;
}
function show(i){
  const c = C[i]; if (!c) return;
  const attrs = Object.entries(c[3]).map(([k,v]) => `<tr><th>${esc(k)}</th><td>${esc(v)}</td></tr>`).join('');
  const direct = [...(inn[i] || []), ...(al[i] || []).flatMap(x => inn[x] || [])].map(k => R[k]);
  const byType = {}; direct.forEach(([a, r]) => { const t = C[a][1] + ' — ' + r; (byType[t] = byType[t] || []).push(a); });
  const total = downAll(i);
  $('#detail').innerHTML = `<div class="ty big">${esc(c[1])}</div><h2>${esc(c[2])}</h2>
    ${attrs ? `<table class="attrs">${attrs}</table>` : ''}
    <h3>Depende de</h3>${upTree(i, 0, new Set([i])) || '<p class="mut">Nada (elemento raíz).</p>'}
    <h3>Dependen de él directamente</h3>${Object.keys(byType).length ? Object.entries(byType).map(([t, xs]) =>
      `<details ${xs.length <= 8 ? 'open' : ''}><summary>${esc(t)} (${xs.length})</summary><ul>${xs.slice(0, 500).map(a => `<li>${chip(a)}</li>`).join('')}</ul></details>`).join('')
      : '<p class="mut">Nada.</p>'}
    ${Object.keys(total).length ? `<h3>Impacto total si falla</h3><p>${Object.entries(total).sort((a,b)=>b[1]-a[1]).map(([t,n]) => `<span class="pill">${esc(t)}: ${n}</span>`).join(' ')}</p>` : ''}
    <p class="mut">Métricas de origen: ${esc(c[4].join(', '))}</p>`;
}
$('#q').addEventListener('input', search);
window.addEventListener('hashchange', () => show(+location.hash.slice(1)));
renderTypes(); search(); if (location.hash) show(+location.hash.slice(1));
"""

CMDB_CSS = """
:root{--bg:#f6f7f9;--card:#fff;--ink:#1d2330;--mut:#667085;--line:#e4e7ec;--acc:#1570ef}
@media (prefers-color-scheme:dark){:root{--bg:#0f1218;--card:#171b23;--ink:#e6e8ec;--mut:#98a2b3;--line:#2a303b;--acc:#53b1fd}}
*{box-sizing:border-box}body{margin:0;font:14px/1.45 system-ui,-apple-system,Segoe UI,Roboto,sans-serif;background:var(--bg);color:var(--ink)}
header{padding:16px 24px;background:var(--card);border-bottom:1px solid var(--line)}h1{margin:0;font-size:18px}
.sub,.mut{color:var(--mut);font-size:12px}
#types{display:flex;flex-wrap:wrap;gap:6px;margin-top:10px}
#types button{border:1px solid var(--line);background:var(--card);color:var(--ink);border-radius:99px;padding:3px 10px;font-size:12px;cursor:pointer}
#types button.on{border-color:var(--acc);color:var(--acc)}
main{display:grid;grid-template-columns:minmax(260px,380px) 1fr;gap:16px;padding:16px 24px}
.panel{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:12px 14px;min-width:0}
#q{width:100%;padding:8px 10px;border:1px solid var(--line);border-radius:8px;background:var(--bg);color:var(--ink)}
#list{list-style:none;margin:10px 0 0;padding:0;max-height:75vh;overflow:auto}#list li{padding:3px 0;border-bottom:1px solid var(--line)}
a.ci{color:var(--ink);text-decoration:none}a.ci:hover{color:var(--acc)}
.ty{display:inline-block;font-size:11px;color:var(--mut);border:1px solid var(--line);border-radius:6px;padding:0 5px}
.ty.big{font-size:12px}h2{margin:6px 0 10px;font-size:18px;word-break:break-all}h3{font-size:13px;margin:18px 0 6px;color:var(--mut);text-transform:uppercase;letter-spacing:.04em}
ul{margin:4px 0;padding-left:18px}li{margin:2px 0}.rel{color:var(--mut);font-size:12px}
table.attrs{border-collapse:collapse}table.attrs th{text-align:left;color:var(--mut);font-weight:500;padding:2px 12px 2px 0}
.pill{display:inline-block;border:1px solid var(--line);border-radius:99px;padding:1px 8px;margin:2px;font-size:12px}
summary{cursor:pointer}
@media (max-width:800px){main{grid-template-columns:1fr}}
"""


def render_cmdb_html(cmdb, meta):
    cis = list(cmdb["cis"].values())
    idx = {c["id"]: i for i, c in enumerate(cis)}
    data = {"cis": [[c["id"], c["type"], c["name"], c["attrs"], c["sources"]] for c in cis],
            "rels": [[idx[a], r, idx[b], m] for a, r, b, m in cmdb["rels"]]}
    payload = json.dumps(data, ensure_ascii=False).replace("</", "<\\/")
    return ("<!DOCTYPE html><html lang='es'><head><meta charset='utf-8'>"
            "<meta name='viewport' content='width=device-width,initial-scale=1'><title>CMDB desde Prometheus</title>"
            f"<style>{CMDB_CSS}</style></head><body><header><h1>CMDB generada desde Prometheus</h1>"
            f"<div class='sub'>{len(cis)} elementos · {len(cmdb['rels'])} relaciones · fuente: {esc(meta.get('source', ''))} · "
            f"generada {esc(fmt_time(datetime.now(timezone.utc)))}</div><div id='types'></div></header>"
            "<main><div class='panel'><input id='q' type='search' placeholder='Buscar: NCJA, s00e-hses-48, UNITY-2-ZOCO, pool...'>"
            "<ul id='list'></ul></div><div class='panel' id='detail'><p class='mut'>Elige un elemento de la lista "
            "para ver de qué depende y qué depende de él.</p></div></main>"
            f"<script type='application/json' id='data'>{payload}</script><script>{CMDB_JS}</script></body></html>")



# --------------------------------------------------------------------------
# Vista drill-down: sede -> tecnología -> equipo -> elemento (con grafo de relaciones)
# --------------------------------------------------------------------------
def cmdb_sites(cmdb, cfg):
    """Sede de cada CI: por su nombre/atributos (solo tipos 'físicos') o heredada del primer
    padre de la misma tecnología; si no, de sus hijos (p. ej. un vCenter con ESXi en dos sedes)."""
    model = cfg.get("cmdb_model") or {}
    pats = {k: re.compile(v) for k, v in cfg.get("site_patterns", {}).items()}
    tech = {t: k for k, ts in model.get("technologies", {}).items() for t in ts}
    own_types = set(model.get("site_from_name_types", []))
    cis = cmdb["cis"]
    site = {}
    for i, c in cis.items():
        if own_types and c["type"] not in own_types:
            continue
        ign = set(model.get("site_ignore_attrs", []))
        txt = [c["name"]] + [v for k, v in c["attrs"].items() if k not in ign]
        st = {k for k, p in pats.items() if any(p.search(t) for t in txt)}
        if st:
            site[i] = st
    out, inn = defaultdict(list), defaultdict(list)
    for a, r, b, m in cmdb["rels"]:
        if r != "es el mismo equipo que" and tech.get(cis[a]["type"]) == tech.get(cis[b]["type"]):
            out[a].append(b)
            inn[b].append(a)
    for _ in range(8):
        changed = False
        for i in cis:
            if i not in site:
                for b in out[i]:
                    if b in site:
                        site[i] = set(site[b])
                        changed = True
                        break
        if not changed:
            break
    for _ in range(8):
        changed = False
        for i in cis:
            if i not in site:
                st = set().union(*[site.get(a, set()) for a in inn[i]])
                if st:
                    site[i] = st
                    changed = True
        if not changed:
            break
    return site, tech


def render_drill_html(res, topo, cmdb, cfg, meta, refresh=None):
    now = datetime.now(timezone.utc)
    model = cfg.get("cmdb_model") or {}
    site, tech = cmdb_sites(cmdb, cfg)
    cis = list(cmdb["cis"].values())
    idx = {c["id"]: k for k, c in enumerate(cis)}
    node_cis = getattr(topo, "node_cis", {})
    pats = {k: re.compile(v) for k, v in cfg.get("site_patterns", {}).items()}

    # -- entradas (alertas deduplicadas por elemento) --------------------
    entries, ci_entries, other = [], defaultdict(list), defaultdict(list)
    all_entries = [e for g in res["groups"] for e in g["alerts"]] + [e for r in res["summary"] for e in r["entries"]]
    eid = {}
    for e in all_entries:
        k = len(entries)
        eid[id(e)] = k
        pl = e.get("primary")
        bk = None
        for m in e["members"]:
            ds = m["labels"].get("datastore")
            if ds:
                bk = DS_BACKING.get((m["labels"].get("vcenter"), ds)) or bk
        entries.append({
            "s": e["sev_rank"], "sev": e["severity"], "n": e["name"],
            "names": [f"{n}" + (f" ×{c}" if c > 1 else "") for n, c in e["names"].items()],
            "el": f"{pl}={e['labels'].get(pl)}" if pl else "",
            "sum": e["summary"], "t": fmt_time(e["starts"]), "ago": human_ago(e["starts"], now),
            "st": e["state"], "c": len(e["members"]), "bk": bk,
        })
        mapped = set()
        for m in e["members"]:
            for n in m["P"]:
                for cid in node_cis.get(n, ()):
                    if cid in idx:
                        mapped.add(idx[cid])
        # Si las reglas que leen las propias alertas dan un CI de ese tipo, manda sobre el grafo:
        # HG_TST existe en las dos FlashSystem, pero la alerta es de una sola.
        ac = {idx[cid] for m in e["members"] for cid in cmdb.get("alert_ci", {}).get(m.get("_idx"), []) if cid in idx}
        if ac and mapped:
            ac_types = {cis[x]["type"] for x in ac}
            mapped = {x for x in mapped if cis[x]["type"] not in ac_types or x in ac}
        if not mapped:   # reglas del modelo que leen las propias alertas: primer CI que encaja
            for m in e["members"]:
                for cid in (cmdb.get("alert_ci", {}).get(m.get("_idx"), [])[:1]):
                    if cid in idx:
                        mapped.add(idx[cid])
        for ci in mapped:
            ci_entries[ci].append(k)
        if not mapped:
            dev = None
            for L in cfg["device_labels"]:
                if e["labels"].get(L):
                    dev = f"{L}={e['labels'][L]}"
                    break
            txt = " ".join(str(v) for v in e["labels"].values())
            st = sorted(k2 for k2, p in pats.items() if p.search(txt)) or ["Sin sede"]
            for s_ in st:
                other[s_].append((dev or "—", k))
    incidents = []
    for g in res["groups"]:
        r = g["root"]
        root_k = eid[id(r)]
        root_ci = next((ci for ci, ks in ci_entries.items() if root_k in ks), None)
        incidents.append({"root": root_k, "ci": root_ci, "n": g["n_alerts"], "ne": len(g["alerts"]),
                          "why": g["root_reason"], "s": g["max_sev"], "ks": [eid[id(e)] for e in g["alerts"]],
                          "site": sorted(site.get(cis[root_ci]["id"], [])) if root_ci is not None else []})
    techs = list(model.get("technologies", {}).keys())
    site_names = sorted({s_ for st in site.values() for s_ in st}) + ["Sin sede"]
    data = {
        "techs": techs, "dev": model.get("device_types", {}), "sites": site_names, "nav": model.get("nav_parent", {}), "member_view": model.get("member_view", {}),
        "cis": [[c["id"], c["type"], c["name"], c["attrs"], tech.get(c["type"], "Otros"),
                 sorted(site.get(c["id"], ["Sin sede"]))] for c in cis],
        "rels": [[idx[a], r, idx[b], m, cmdb.get("rel_attrs", {}).get((a, r, b), {})] for a, r, b, m in cmdb["rels"]],
        "info_rels": model.get("informative_relations", []),
        "E": entries, "ciE": {str(k): v for k, v in ci_entries.items()},
        "inc": incidents,
        "other": {s_: [[d, k] for d, k in v] for s_, v in other.items()},
        "meta": {"source": meta.get("source", ""), "gen": fmt_time(now), "n_alerts": res["n_alerts"],
                 "n_el": res["n_elements"], "lista": meta.get("lista", ""), "refresh": refresh or 0},
    }
    style = cfg.get("ui_style", "a") if cfg.get("ui_style") in ("a", "c") else "a"
    theme = cfg.get("ui_theme", "") if cfg.get("ui_theme") in ("dark", "light") else ""
    payload = json.dumps(data, ensure_ascii=False).replace("</", "<\\/")
    boot = ("(function(){var d=document.documentElement,st='%s',th='%s';"
            "try{st=localStorage.getItem('cgs.style')||st;th=localStorage.getItem('cgs.theme')||th;}catch(e){}"
            "if(!th)th=matchMedia('(prefers-color-scheme: dark)').matches?'dark':'light';"
            "d.dataset.style=st;d.dataset.theme=th;})();") % (style, theme)
    return ("<!DOCTYPE html><html lang='es'><head><meta charset='utf-8'>"
            "<meta name='viewport' content='width=device-width,initial-scale=1'>"
            + (f"<meta http-equiv='refresh' content='{int(refresh)}'>" if refresh else "") +
            f"<title>{esc(UI_TITLE)}</title><script>{boot}</script>"
            "<link rel='preconnect' href='https://fonts.googleapis.com'>"
            "<link rel='stylesheet' href='https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700"
            "&family=JetBrains+Mono:wght@400;600;700&family=Space+Grotesk:wght@500;600;700&family=IBM+Plex+Mono:wght@400;600&display=swap'>"
            f"<style>{DRILL_CSS}</style></head><body>" + DRILL_SHELL.replace("{TITLE}", esc(UI_TITLE)) +
            f"<script type='application/json' id='data'>{payload}</script><script>{DRILL_JS}</script></body></html>")


UI_TITLE = "[CGS] Pasarela Eventos Consolidación"

ICONS = {
    "search": '<circle cx="11" cy="11" r="7"/><path d="M20 20l-4-4"/>',
    "sun": '<circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/>',
    "moon": '<path d="M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z"/>',
}


def _svg(name, size=15):
    return (f'<svg aria-hidden="true" width="{size}" height="{size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" '
            f'stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">{ICONS[name]}</svg>')


DRILL_SHELL = (
    "<header class='top'><a class='brand' href='#' aria-label='Inicio'><span class='bm'>CGS</span></a>"
    "<div class='ttl'><h1>{TITLE}</h1><div class='sub' id='meta'></div></div>"
    "<label class='srch'>" + _svg("search") + "<span class='sr'>Buscar elemento</span>"
    "<input id='q' type='search' placeholder='Buscar: NCJA, s00e-hses-48, UNITY-2-ZOCO…'></label>"
    "<div class='seg' role='group' aria-label='Estilo'>"
    "<button type='button' data-set-style='a' title='Consola NOC'>A · NOC</button>"
    "<button type='button' data-set-style='c' title='Tabla operativa'>C · Tabla</button></div>"
    "<div class='seg' role='group' aria-label='Tema'>"
    "<button type='button' data-set-theme='dark'>" + _svg("moon", 14) + "Oscuro</button>"
    "<button type='button' data-set-theme='light'>" + _svg("sun", 14) + "Claro</button></div></header>"
    "<div class='tabs' id='tabs'></div>"
    "<div class='shell'><aside class='side' id='side' aria-label='Navegación'></aside>"
    "<div class='mainw'><nav id='crumbs' aria-label='Ruta'></nav><main id='view'></main></div></div>")

DRILL_CSS = """
:root,[data-style=a][data-theme=dark]{--bg:#0A0E14;--card:#111821;--card2:#1A2330;--ink:#E6EDF3;--mut:#94A3B8;--line:#202B3A;--acc:#22D3EE;--accfg:#04232A;
--s4:#F87171;--s3:#FB923C;--s2:#FBBF24;--s1:#60A5FA;--s0:#7C8AA0;--ok:#4ADE80;--okbg:#123526;--bfg:#14161A;
--ff:'Inter',system-ui,-apple-system,'Segoe UI',Roboto,sans-serif;--fh:var(--ff);--fm:'JetBrains Mono',ui-monospace,SFMono-Regular,Consolas,monospace;--r:8px;--rs:6px}
[data-style=a][data-theme=light]{--bg:#F3F5F8;--card:#FFFFFF;--card2:#E8EDF3;--ink:#0F172A;--mut:#526075;--line:#D9E0E8;--acc:#0E7490;--accfg:#FFFFFF;
--s4:#DC2626;--s3:#C2410C;--s2:#B45309;--s1:#2563EB;--s0:#64748B;--ok:#15803D;--okbg:#DCFCE7;--bfg:#FFFFFF}
[data-style=c]{--ff:'Inter',system-ui,-apple-system,'Segoe UI',Roboto,sans-serif;--fh:'Space Grotesk','Inter',system-ui,sans-serif;--fm:'IBM Plex Mono',ui-monospace,SFMono-Regular,Consolas,monospace;--r:10px;--rs:8px}
[data-style=c][data-theme=dark]{--bg:#0B0D17;--card:#121527;--card2:#1C2038;--ink:#E8EAF6;--mut:#9AA0BF;--line:#242A45;--acc:#A78BFA;--accfg:#160B33;
--s4:#FB7185;--s3:#FB923C;--s2:#FDBA74;--s1:#7DD3FC;--s0:#8088A8;--ok:#6EE7B7;--okbg:#123128;--bfg:#14101C}
[data-style=c][data-theme=light]{--bg:#FFFFFF;--card:#FFFFFF;--card2:#F1F0FB;--ink:#14122B;--mut:#5A5873;--line:#E3E2EE;--acc:#6D28D9;--accfg:#FFFFFF;
--s4:#E11D48;--s3:#C2410C;--s2:#B45309;--s1:#0369A1;--s0:#6B6985;--ok:#047857;--okbg:#D1FAE5;--bfg:#FFFFFF}
*{box-sizing:border-box}html{color-scheme:dark}[data-theme=light]{color-scheme:light}
body{margin:0;font:14px/1.45 var(--ff);background:var(--bg);color:var(--ink)}
a{color:var(--acc);text-decoration:none}a:hover{text-decoration:underline}
code,.mono{font-family:var(--fm);font-size:12.5px}
.sr{position:absolute;width:1px;height:1px;overflow:hidden;clip:rect(0 0 0 0)}
button{font:inherit;color:inherit}
:focus-visible{outline:2px solid var(--acc);outline-offset:2px}
/* cabecera */
header.top{display:flex;gap:14px;align-items:center;flex-wrap:wrap;padding:12px 24px;background:var(--card);border-bottom:1px solid var(--line)}
.brand{display:none}.brand:hover{text-decoration:none}
.bm{display:inline-flex;align-items:center;justify-content:center;height:30px;padding:0 8px;border:1px solid var(--acc);border-radius:var(--rs);font:700 13px var(--fm);color:var(--acc)}
[data-style=c] .bm{border:0;padding:0;font:700 15px var(--fh)}[data-style=c] .bm::before{content:'['}[data-style=c] .bm::after{content:']'}
.ttl{flex:1 1 320px;min-width:0}h1{margin:0;font:600 18px/1.25 var(--fh);letter-spacing:-.01em}
[data-style=c] h1{font-size:20px}
.sub,.mut{color:var(--mut);font-size:12px}.sub{font-family:var(--fm);font-size:11px;margin-top:2px}
.srch{display:flex;align-items:center;gap:8px;height:36px;padding:0 10px;border:1px solid var(--line);border-radius:var(--rs);background:var(--bg);color:var(--mut);width:330px;max-width:100%}
.srch input{flex:1;min-width:0;border:0;outline:0;background:transparent;color:var(--ink);font:13px var(--ff)}
.seg{display:inline-flex;padding:3px;border:1px solid var(--line);border-radius:calc(var(--rs) + 3px);background:var(--bg)}
.seg button{display:inline-flex;align-items:center;gap:6px;height:30px;padding:0 11px;border:0;border-radius:var(--rs);background:transparent;color:var(--mut);font-size:12.5px;cursor:pointer}
.seg button[aria-pressed=true]{background:var(--card2);color:var(--ink);font-weight:600}
/* pestañas (C) y lateral (A) */
.tabs{display:none}
[data-style=c] .tabs{display:flex;flex-wrap:wrap;gap:12px;align-items:center;justify-content:space-between;padding:10px 24px;border-bottom:1px solid var(--line);background:var(--bg)}
.tabs nav{display:flex;flex-wrap:wrap;gap:4px}.tabs a{display:inline-flex;align-items:center;height:30px;padding:0 12px;border-radius:var(--rs);color:var(--mut);font-size:13px}
.tabs a.on{background:var(--card2);color:var(--ink);font-weight:600}.tabs a:hover{text-decoration:none;color:var(--ink)}
.chips{display:flex;flex-wrap:wrap;gap:6px;align-items:center}
.chip{display:inline-flex;align-items:center;gap:6px;height:28px;padding:0 11px;border:1px solid var(--line);border-radius:99px;background:var(--card);font-size:12px;color:var(--ink)}
.chip:hover{text-decoration:none;border-color:var(--acc)}.chip.on{border-color:var(--acc);color:var(--acc);font-weight:600}
.dot{display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--ok);flex-shrink:0}
.dot.s4{background:var(--s4)}.dot.s3{background:var(--s3)}.dot.s2{background:var(--s2)}.dot.s1{background:var(--s1)}.dot.s0{background:var(--s0)}
.shell{display:flex;flex-wrap:wrap;align-items:flex-start}
.side{display:none}
[data-style=a] .side{display:flex;flex-direction:column;gap:18px;flex:1 1 210px;max-width:236px;padding:18px 12px;border-right:1px solid var(--line);align-self:stretch;min-height:calc(100vh - 60px)}
.side h4{margin:0 0 4px;padding:0 10px;font:600 11px var(--fm);letter-spacing:.08em;color:var(--mut);text-transform:uppercase}
.side a{display:flex;align-items:center;gap:10px;min-height:34px;padding:0 10px;border-radius:var(--rs);color:var(--mut);font-size:13px}
.side a:hover{text-decoration:none;background:var(--card);color:var(--ink)}.side a.on{background:var(--card2);color:var(--ink);font-weight:600}
.side a .n{margin-left:auto;font:11px var(--fm);color:var(--mut)}
.side .src{margin-top:auto;padding:10px;border:1px solid var(--line);border-radius:var(--r);font:11px/1.7 var(--fm);color:var(--mut)}
.mainw{flex:999 1 600px;min-width:0}
nav#crumbs{padding:14px 24px 0;font-size:13px}nav#crumbs span{color:var(--mut);margin:0 6px}nav#crumbs b{font-weight:600}
main{padding:12px 24px 40px}
/* bloques comunes */
.box{background:var(--card);border:1px solid var(--line);border-radius:var(--r);padding:14px 16px;margin:12px 0;overflow-x:auto}
h2{font:600 15px var(--fh);margin:22px 0 8px}h3{font:600 14px var(--fh);margin:0}
.b{display:inline-flex;align-items:center;justify-content:center;min-width:22px;height:20px;padding:0 7px;border-radius:99px;font:600 11.5px var(--fm);color:var(--bfg);margin-left:3px;vertical-align:middle}
[data-style=c] .b{border-radius:99px}[data-style=a] .b{border-radius:4px}
.b4{background:var(--s4)}.b3{background:var(--s3)}.b2{background:var(--s2)}.b1{background:var(--s1)}.b0{background:var(--s0)}.bok{background:var(--okbg);color:var(--ok)}
.ty{display:inline-block;font-size:11px;color:var(--mut);border:1px solid var(--line);border-radius:5px;padding:0 5px;margin-right:4px}
table{width:100%;border-collapse:collapse}th,td{text-align:left;padding:7px 8px;border-top:1px solid var(--line);vertical-align:top}
th{font:500 11px var(--ff);text-transform:uppercase;color:var(--mut);letter-spacing:.05em}
a.ci{color:var(--ink)}a.ci:hover{color:var(--acc);text-decoration:none}
.attrs td,.attrs th{border:0;padding:2px 16px 2px 0;text-transform:none;font-size:13px;letter-spacing:0}.attrs th{color:var(--mut);font-weight:500}
.graph{overflow:auto;border:1px solid var(--line);border-radius:var(--r);background:var(--bg)}
.graph svg{display:block}
.n rect{fill:var(--card);stroke:var(--line);stroke-width:1}.n.sel rect{stroke:var(--acc);stroke-width:2}
.n.s4 rect{stroke:var(--s4);stroke-width:2}.n.s3 rect{stroke:var(--s3);stroke-width:2}.n.s2 rect{stroke:var(--s2);stroke-width:2}.n.s1 rect{stroke:var(--s1);stroke-width:1.5}
.n text{fill:var(--ink);font:12px var(--fm)}.n text.t{fill:var(--mut);font:10px var(--ff)}.n{cursor:pointer}.n.more{cursor:default}.n:hover rect{fill:var(--card2)}
.e{fill:none;stroke:var(--s0);stroke-width:1;opacity:.7}.e.x{stroke:var(--acc);stroke-dasharray:4 3;opacity:1}
.lbl{fill:var(--mut);font:11px var(--ff)}
details summary{cursor:pointer;padding:5px 0}.flt{margin:6px 0;padding:6px 9px;border:1px solid var(--line);border-radius:var(--rs);background:var(--bg);color:var(--ink);width:260px}
ul{padding-left:20px}li{margin:2px 0}
/* cifras */
.kpis{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin:4px 0 16px}
.kpi{display:flex;flex-direction:column;gap:4px;padding:13px 16px;background:var(--card);border:1px solid var(--line);border-radius:var(--r)}
.kpi span{font:600 11px var(--fm);letter-spacing:.08em;color:var(--mut);text-transform:uppercase}.kpi b{font:600 28px/1.1 var(--fm)}
.kpi.k4 b{color:var(--s4)}.kpi.k2 b{color:var(--s2)}.kpi.k1 b{color:var(--s1)}.kpi.ka b{color:var(--acc)}
/* incidencias */
.panel{background:var(--card);border:1px solid var(--line);border-radius:var(--r);overflow:hidden;margin:0 0 16px}
.panel>header{display:flex;justify-content:space-between;align-items:center;gap:10px;padding:12px 16px}
.irow{display:grid;grid-template-columns:auto minmax(0,1fr) auto;gap:14px;align-items:center;padding:11px 16px;border-top:1px solid var(--line);color:var(--ink)}
.irow:hover{background:var(--card2);text-decoration:none}
.irow .nm{font:600 13px var(--fm)}.r .irow{grid-template-columns:auto minmax(0,1fr)}.r .irow .rt{grid-column:2;text-align:left}.irow .rt{text-align:right;font:11px var(--fm);color:var(--mut)}.irow .rt b{color:var(--ink);font-weight:500}
/* A: columnas por tecnología */
.tcols{display:grid;grid-template-columns:repeat(auto-fit,minmax(230px,1fr));gap:12px;align-items:start}
.tcol{background:var(--card);border:1px solid var(--line);border-radius:var(--r);overflow:hidden;min-width:0;scroll-margin-top:12px}
.tcol>header{display:flex;align-items:center;gap:8px;padding:11px 12px}.tcol>header h3{flex:1}
.tcol .sg{padding:8px 12px 2px;font:600 10.5px var(--fm);letter-spacing:.06em;color:var(--mut);text-transform:uppercase;border-top:1px solid var(--line)}
.drow{display:flex;flex-direction:column;gap:3px;padding:9px 12px;border-top:1px solid var(--line);color:var(--ink)}
.drow:hover{background:var(--card2);text-decoration:none}
.drow .l1{display:flex;justify-content:space-between;align-items:center;gap:8px}
.drow .dn{font:600 13px var(--fm);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.drow .dm{font-size:12px;color:var(--mut);overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.bs{display:inline-flex;flex-shrink:0}
/* C: tabla + panel de incidencia */
.split{display:flex;flex-wrap:wrap;gap:24px;align-items:flex-start}.split>.l{flex:999 1 600px;min-width:0}.split>.r{flex:1 1 300px;max-width:400px}
.tbl{overflow-x:auto;border:1px solid var(--line);border-radius:var(--r);background:var(--card)}
.tbl table{min-width:680px}.tbl th,.tbl td{padding:9px 16px;vertical-align:middle}
.tbl tr.g th{padding:14px 16px 8px;background:var(--bg);text-transform:none;letter-spacing:0;font:600 14px var(--fh);color:var(--ink)}
.tbl tr.g th .mut{font:11px var(--fm);margin-left:6px}
.tbl tr.d{cursor:pointer}.tbl tr.d:hover td{background:var(--card2)}
.dist{display:flex;gap:2px;height:6px;border-radius:3px;overflow:hidden;min-width:120px;background:var(--card2)}
.dist span{display:block}.dist .s4{background:var(--s4)}.dist .s3{background:var(--s3)}.dist .s2{background:var(--s2)}.dist .s1{background:var(--s1)}.dist .s0{background:var(--s0)}.dist .ok{background:var(--ok);opacity:.45;flex:1}
.ipanel{display:flex;flex-direction:column;gap:12px;padding:16px;background:var(--card);border:1px solid var(--line);border-radius:var(--r);margin-bottom:14px}
.ipanel h2{margin:0;font-size:17px;word-break:break-word}.ipanel p{margin:0;font-size:13px;color:var(--mut)}
.cap{font:500 11px var(--ff);letter-spacing:.06em;color:var(--mut);text-transform:uppercase}
.stats{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:8px;font:12px var(--fm)}.stats div{padding:8px 10px;border-radius:var(--rs);background:var(--card2)}
.chain{margin:0;padding:0;list-style:none}
.chain li{display:grid;grid-template-columns:16px minmax(0,1fr);gap:10px;align-items:start;margin:0;padding-bottom:10px;position:relative}
.chain li::before{content:'';position:absolute;left:4px;top:16px;bottom:0;width:2px;background:var(--line)}.chain li:last-child::before{display:none}
.chain .pt{margin-top:4px;width:10px;height:10px;border-radius:50%;border:2px solid var(--s0)}
.chain .pt.s4{border-color:var(--s4)}.chain .pt.s3{border-color:var(--s3)}.chain .pt.s2{border-color:var(--s2)}.chain .pt.s1{border-color:var(--s1)}.chain .pt.root{background:var(--s4);border-color:var(--s4)}
.chain .k{font-size:11px;color:var(--mut)}.chain .v{font:13px var(--fm);word-break:break-all}.chain .w{font-size:11px}
.btn{display:inline-flex;align-items:center;justify-content:center;min-height:40px;padding:0 16px;border-radius:var(--rs);background:var(--acc);color:var(--accfg);font-weight:600;font-size:13px}.btn:hover{text-decoration:none;filter:brightness(1.08)}
/* portada: tarjetas de sede */
.grid{display:grid;gap:14px;grid-template-columns:repeat(auto-fill,minmax(280px,1fr))}
.card{display:block;background:var(--card);border:1px solid var(--line);border-radius:var(--r);padding:14px 16px;color:var(--ink)}
.card:hover{border-color:var(--acc);text-decoration:none}
.card h3{display:flex;align-items:center;gap:8px;margin:0 0 6px;font-size:16px}.card .row{display:flex;justify-content:space-between;gap:8px;padding:5px 0;border-top:1px solid var(--line)}
@media (max-width:760px){.kpis{grid-template-columns:repeat(2,minmax(0,1fr))}header.top{padding:12px 14px}main,nav#crumbs{padding-left:14px;padding-right:14px}.srch{width:100%}}
"""

DRILL_JS = r"""
const D = JSON.parse(document.getElementById('data').textContent);
const C = D.cis, R = D.rels, E = D.E, SAME = 'es el mismo equipo que';
const out = {}, inn = {}, al = {};
R.forEach(([a, r, b], k) => { if (r === SAME) { (al[a]=al[a]||[]).push(b); (al[b]=al[b]||[]).push(a); return; }
  (out[a]=out[a]||[]).push(k); (inn[b]=inn[b]||[]).push(k); });
const ciE = i => D.ciE[i] || [];
const esc = s => String(s ?? '').replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const $ = s => document.querySelector(s);
document.getElementById('meta').textContent = `${D.meta.n_alerts} alertas · ${D.meta.n_el} elementos afectados · ${D.meta.source} · ${D.meta.gen}`;
// --- acumulado de alertas de un CI y todo lo que depende de él (misma tecnología)
const roll = {};
let curSite = null;                       // sede del recorrido actual (filtra equipos compartidos)
const inSite = (x, s) => !s || C[x][5].includes(s);
function rollup(i, site){
  const s = site === undefined ? (inSite(i, curSite) ? curSite : null) : site;
  const key = i + '|' + (s || '');
  if (roll[key]) return roll[key];
  const seen = new Set([i]), q = [i], es = new Set();
  while (q.length) { const x = q.pop(); if (inSite(x, s)) ciE(x).forEach(k => es.add(k));
    (inn[x]||[]).forEach(k => { const a = R[k][0]; if (!seen.has(a) && C[a][4] === C[i][4] && inSite(a, s)) { seen.add(a); q.push(a); } }); }
  return roll[key] = es;
}
function sevCount(es){ const c = [0,0,0,0,0]; es.forEach(k => c[E[k].s]++); return c; }
function badges(es){ const c = sevCount(es); if (!es.size && !es.length) return '<span class="b bok">OK</span>';
  return [4,3,2,1,0].filter(s => c[s]).map(s => `<span class="b b${s}" title="${['sin severidad','info','warning','high','critical'][s]}">${c[s]}</span>`).join(''); }
function maxSev(es){ let m = -1; es.forEach(k => { if (E[k].s > m) m = E[k].s; }); return m; }
function ciHref(i){ return `#ci=${i}` + (curSite && inSite(i, curSite) ? `&site=${encodeURIComponent(curSite)}` : ''); }
function ciLink(i, withType=true){ return `<a class="ci" href="${ciHref(i)}">${withType?`<span class="ty">${esc(C[i][1])}</span>`:''}${esc(C[i][2])}</a>`; }
function devicesOf(site, tech){ const types = D.dev[tech] || [];
  return C.map((c,i)=>i).filter(i => types.includes(C[i][1]) && (site==='*' || C[i][5].includes(site))); }
function siteSet(site, tech){ const s = new Set(); devicesOf(site, tech).forEach(i => rollup(i, site).forEach(k => s.add(k)));
  (D.other[site]||[]).forEach(([d,k]) => { if (tech==='Otros') s.add(k); }); return s; }
// --- vistas
function crumbs(parts){ $('#crumbs').innerHTML = parts.map(([t,h]) => h ? `<a href="${h}">${esc(t)}</a>` : `<b>${esc(t)}</b>`).join('<span>›</span>'); }
function entryRows(ks){
  ks = [...ks].sort((a,b) => E[b].s - E[a].s);
  return `<table><thead><tr><th>Sev.</th><th>Alerta</th><th>Elemento</th><th>Desde</th><th>Resumen</th></tr></thead><tbody>${ks.map(k => { const e = E[k];
    return `<tr><td><span class="b b${e.s}">${esc(e.sev)}</span></td><td>${esc(e.n)}${e.c>1?` <span class="mut">×${e.c}</span>`:''}${e.names.length>1?`<div class="mut">${esc(e.names.join(', '))}</div>`:''}<div class="mut">${esc(e.st)}</div></td>
    <td><code>${esc(e.el)}</code>${e.bk?`<div class="mut">→ ${esc(e.bk)}</div>`:''}</td><td>${esc(e.t)}<div class="mut">${esc(e.ago)}</div></td><td>${esc(e.sum)}</td></tr>`; }).join('')}</tbody></table>`;
}
// --- estilo (A/C) y tema (oscuro/claro), recordados en el navegador
const ROOT = document.documentElement;
function syncSeg(){ document.querySelectorAll('[data-set-style]').forEach(b => b.setAttribute('aria-pressed', b.dataset.setStyle === ROOT.dataset.style));
  document.querySelectorAll('[data-set-theme]').forEach(b => b.setAttribute('aria-pressed', b.dataset.setTheme === ROOT.dataset.theme)); }
function setPref(k, v){ ROOT.dataset[k] = v; try { localStorage.setItem('cgs.' + k, v); } catch (e) {} syncSeg(); route(); }
document.querySelectorAll('[data-set-style]').forEach(b => b.addEventListener('click', () => setPref('style', b.dataset.setStyle)));
document.querySelectorAll('[data-set-theme]').forEach(b => b.addEventListener('click', () => setPref('theme', b.dataset.setTheme)));
syncSeg();
const isC = () => ROOT.dataset.style === 'c';
const fmtN = n => n.toLocaleString('es-ES');
// --- cifras de un conjunto de alertas
function kpiCounts(es){ const c = sevCount(es); return {crit: c[4] + c[3], warn: c[2], info: c[1] + c[0]}; }
function siteAll(s){ const all = new Set(); D.techs.forEach(t => siteSet(s, t).forEach(k => all.add(k))); (D.other[s]||[]).forEach(([d,k]) => all.add(k)); return all; }
function allAlerts(){ const all = new Set(); D.sites.forEach(s => siteAll(s).forEach(k => all.add(k))); E.forEach((e, k) => all.add(k)); return all; }
function incsOf(s){ return D.inc.map((x, n) => [x, n]).filter(([x]) => !s || !x.site.length || x.site.includes(s)); }
function kpiHtml(es, nInc){ const k = kpiCounts(es);
  return `<div class="kpis"><div class="kpi k4"><span>Críticas</span><b>${fmtN(k.crit)}</b></div><div class="kpi k2"><span>Avisos</span><b>${fmtN(k.warn)}</b></div>
  <div class="kpi k1"><span>Info</span><b>${fmtN(k.info)}</b></div><div class="kpi ka"><span>Incidencias</span><b>${nInc}</b></div></div>`; }
const sevName = s => ['sin severidad','info','aviso','alta','crítica'][s];
function incRows(list){
  return list.map(([x, n]) => { const e = E[x.root];
    return `<a class="irow" href="#inc=${n}"><span class="dot s${x.s}"></span><div style="min-width:0"><div><span class="nm">${esc(e.n)}</span>
      <span class="mut"> · causa: ${x.ci != null ? esc(C[x.ci][1] + ' ' + C[x.ci][2]) : esc(e.el)}</span></div><div class="mut">${esc(e.sum)}</div></div>
      <div class="rt"><b>${x.ne} elementos · ${fmtN(x.n)} alertas</b><br>${esc(x.site.join(', ') || '—')} · ${esc(e.ago)}</div></a>`; }).join(''); }
// --- cadena de impacto de una incidencia: contexto › causa › efectos por tipo
const entryCI = {}; Object.entries(D.ciE).forEach(([ci, ks]) => ks.forEach(k => { if (entryCI[k] == null) entryCI[k] = +ci; }));
function chainHtml(x){
  const items = [];
  if (x.ci != null) { const up = upChain(x.ci).slice(1).reverse();
    up.forEach(u => items.push([C[u][1], ciLink(u, false), 'contexto', maxSev(rollup(u))]));
    items.push([C[x.ci][1], ciLink(x.ci, false), 'causa probable', 'root']); }
  const by = {}; x.ks.forEach(k => { if (k === x.root) return; const ci = entryCI[k];
    const t = ci != null ? C[ci][1] : 'Sin CMDB'; (by[t] = by[t] || {n: 0, s: 0, ci: new Set()}); by[t].n++; by[t].s = Math.max(by[t].s, E[k].s); if (ci != null) by[t].ci.add(ci); });
  Object.entries(by).sort((a, b) => b[1].n - a[1].n).forEach(([t, v]) => {
    const one = v.ci.size === 1 ? ciLink([...v.ci][0], false) : `${v.ci.size || v.n} elementos`;
    items.push([t, one, `efecto · ${v.n} aviso(s)`, v.s]); });
  return `<ol class="chain">${items.map(([k, v, w, s]) => `<li><span class="pt ${s === 'root' ? 'root' : 's' + s}"></span><div><div class="k">${esc(k)}</div><div class="v">${v}</div>
    <div class="w" style="color:var(--${s === 'root' ? 's4' : w === 'contexto' || s < 1 ? 'mut' : 's' + s})">${esc(w)}</div></div></li>`).join('')}</ol>`;
}
function incPanel(x, n, total){ const e = E[x.root];
  return `<section class="ipanel"><div style="display:flex;justify-content:space-between;align-items:center;gap:8px"><span class="cap">Incidencia ${n + 1} de ${total}</span><span class="b b${x.s}">${esc(sevName(x.s))}</span></div>
    <h2>${esc(e.n)}</h2><p>${esc(e.sum)}</p>
    <div class="stats"><div>${x.ne} elementos</div><div>${fmtN(x.n)} alertas</div></div>
    <span class="cap">Cadena de impacto</span>${chainHtml(x)}
    <p class="mut">${esc(x.why)}</p>
    ${x.ci != null ? `<a class="btn" href="${ciHref(x.ci)}">Ver elemento y relaciones</a>` : ''}</section>`; }
// --- navegación lateral (A) y pestañas (C)
function chrome(view, site){
  const sites = D.sites.filter(s => siteAll(s).size || D.techs.some(t => devicesOf(s, t).length));
  $('#side').innerHTML = `<div><h4>Vistas</h4><a href="#" class="${view==='home'?'on':''}">Resumen</a><a href="#inc" class="${view==='inc'?'on':''}">Incidencias<span class="n">${D.inc.length}</span></a>
    ${D.meta.lista ? `<a href="${esc(D.meta.lista)}">Lista de alertas</a>` : ''}</div>
    <div><h4>Sedes</h4>${sites.map(s => `<a href="#site=${encodeURIComponent(s)}" class="${s===site?'on':''}"><span class="dot s${maxSev(siteAll(s))}"></span>${esc(s)}<span class="n">${fmtN(siteAll(s).size)}</span></a>`).join('')}</div>
    ${site ? `<div><h4>Tecnologías · ${esc(site)}</h4>${D.techs.map(t => { const n = devicesOf(site, t).length; if (!n) return '';
      return `<a href="#site=${encodeURIComponent(site)}&t=${encodeURIComponent(t)}"><span class="dot s${maxSev(siteSet(site, t))}"></span>${esc(t)}<span class="n">${n}</span></a>`; }).join('')}</div>` : ''}
    <div class="src">Fuente<br><span style="color:var(--ink)">${esc(D.meta.source)}</span><br>Generado ${esc(D.meta.gen)}${D.meta.refresh ? `<br>Auto-refresco ${D.meta.refresh} s` : ''}</div>`;
  const tab = (h, t, on) => `<a href="${h}" class="${on?'on':''}">${t}</a>`;
  $('#tabs').innerHTML = `<nav aria-label="Vistas">${tab('#', 'Resumen', view==='home')}${tab(site ? '#site=' + encodeURIComponent(site) : '#site=' + encodeURIComponent(sites[0] || ''), 'Por sede', view==='site' || view==='ci')}
    ${tab('#inc', `Incidencias (${D.inc.length})`, view==='inc')}${D.meta.lista ? tab(esc(D.meta.lista), 'Lista de alertas', false) : ''}</nav>
    <div class="chips"><span class="mut">Sede</span>${sites.map(s => `<a class="chip ${s===site?'on':''}" href="#site=${encodeURIComponent(s)}"><span class="dot s${maxSev(siteAll(s))}"></span>${esc(s)}</a>`).join('')}</div>`;
}
function home(){
  curSite = null; chrome('home', null); crumbs([['Resumen']]);
  const cards = D.sites.map(s => {
    const all = siteAll(s); const rows = D.techs.map(t => { const n = devicesOf(s,t).length; if (!n) return '';
      return `<div class="row"><span>${esc(t)} <span class="mut">(${n})</span></span><span>${badges(siteSet(s, t))}</span></div>`; }).join('');
    const oth = new Set((D.other[s]||[]).map(([d,k]) => k));
    if (!rows && !oth.size) return '';
    return `<a class="card" href="#site=${encodeURIComponent(s)}"><h3><span class="dot s${maxSev(all)}"></span>${esc(s)}</h3>${rows}
      ${oth.size?`<div class="row"><span>Otros (sin CMDB)</span><span>${badges(oth)}</span></div>`:''}</a>`; }).join('');
  $('#view').innerHTML = kpiHtml(allAlerts(), D.inc.length) +
    `<section class="panel"><header><h3>Incidencias correladas</h3><a href="#inc">Ver detalle</a></header>${incRows(incsOf(null)) || '<p class="mut" style="padding:0 16px 12px">Ninguna.</p>'}</section>
    <div class="grid">${cards}</div>`;
}
function devMeta(i){ return Object.entries(C[i][3]).filter(([k]) => k !== 'Sede').slice(0, 2).map(([k,v]) => v).join(' · '); }
function sortedDevs(s, t){ const ord = D.dev[t] || [];
  return devicesOf(s, t).sort((a,b) => ord.indexOf(C[a][1]) - ord.indexOf(C[b][1]) || maxSev(rollup(b, s)) - maxSev(rollup(a, s)) || rollup(b, s).size - rollup(a, s).size); }
function distBar(es){ const c = sevCount(es), tot = es.size;
  if (!tot) return '<div class="dist"><span class="ok"></span></div>';
  return `<div class="dist" title="${[4,3,2,1,0].filter(s=>c[s]).map(s=>c[s]+' '+sevName(s)).join(' · ')}">${[4,3,2,1,0].filter(s => c[s]).map(s => `<span class="s${s}" style="flex:${c[s]} 1 0"></span>`).join('')}</div>`; }
function sitePage(s, focusTech){
  curSite = s; chrome('site', s); crumbs([['Resumen','#'],[s]]);
  const incs = incsOf(s), all = siteAll(s);
  const oth = D.other[s] || [];
  const byDev = {}; oth.forEach(([d,k]) => (byDev[d]=byDev[d]||[]).push(k));
  const othHtml = oth.length ? `<div class="box"><h3>Otros equipos (sin modelo CMDB) ${badges(new Set(oth.map(x=>x[1])))}</h3>${Object.entries(byDev).map(([d,ks]) =>
    `<details><summary><code>${esc(d)}</code> ${badges(new Set(ks))}</summary>${entryRows(ks)}</details>`).join('')}</div>` : '';
  if (!isC()) {
    const cols = D.techs.map(t => { const devs = sortedDevs(s, t); if (!devs.length) return '';
      const multi = (D.dev[t]||[]).length > 1;
      return `<section class="tcol" id="t-${encodeURIComponent(t)}"><header><span class="dot s${maxSev(siteSet(s,t))}"></span><h3>${esc(t)}</h3><span class="mut mono">${devs.length}</span></header>
        ${devs.map((i, k) => `${multi && (k === 0 || C[devs[k-1]][1] !== C[i][1]) ? `<div class="sg">${esc(C[i][1])}</div>` : ''}
        <a class="drow" href="#ci=${i}&site=${encodeURIComponent(s)}"><div class="l1"><span class="dn" title="${esc(C[i][2])}">${esc(C[i][2])}</span><span class="bs">${badges(rollup(i, s))}</span></div>
        <span class="dm">${esc([C[i][5].length > 1 ? 'varias sedes' : '', devMeta(i) || C[i][1]].filter(Boolean).join(' · '))}</span></a>`).join('')}</section>`; }).join('');
    $('#view').innerHTML = kpiHtml(all, incs.length) +
      `<section class="panel"><header><h3>Incidencias correladas · ${esc(s)}</h3><a href="#inc">Ver detalle</a></header>${incRows(incs) || '<p class="mut" style="padding:0 16px 12px">Ninguna en esta sede.</p>'}</section>
      <div class="tcols">${cols}</div>${othHtml}`;
  } else {
    let rows = '';
    D.techs.forEach(t => { const devs = sortedDevs(s, t); if (!devs.length) return;
      rows += `<tr class="g" id="t-${encodeURIComponent(t)}"><th colspan="4" scope="colgroup"><span class="dot s${maxSev(siteSet(s,t))}"></span> ${esc(t)}<span class="mut">${devs.length} equipo(s)</span></th></tr>`;
      devs.forEach(i => { const r = rollup(i, s);
        rows += `<tr class="d" onclick="location.hash='ci=${i}&site=${encodeURIComponent(s)}'"><td><a class="ci" href="#ci=${i}&site=${encodeURIComponent(s)}"><span class="mono" style="font-weight:600">${esc(C[i][2])}</span></a>
          <div class="mut">${esc([C[i][5].length > 1 ? 'varias sedes' : '', devMeta(i)].filter(Boolean).join(' · '))}</div></td><td class="mut" style="white-space:nowrap">${esc(C[i][1])}</td>
          <td style="width:28%">${distBar(r)}</td><td style="text-align:right;white-space:nowrap">${badges(r)}</td></tr>`; }); });
    const k = kpiCounts(all);
    $('#view').innerHTML = `<div class="split"><div class="l">
      <div style="display:flex;justify-content:space-between;align-items:baseline;gap:12px;flex-wrap:wrap;margin-bottom:10px"><h2 style="margin:0">${esc(s)} · estado por tecnología</h2>
      <div class="chips"><span class="chip"><span class="dot s4"></span>Críticas <b class="mono">${fmtN(k.crit)}</b></span><span class="chip"><span class="dot s2"></span>Avisos <b class="mono">${fmtN(k.warn)}</b></span><span class="chip"><span class="dot s1"></span>Info <b class="mono">${fmtN(k.info)}</b></span></div></div>
      <div class="tbl"><table><thead><tr><th>Equipo</th><th>Tipo</th><th>Distribución</th><th style="text-align:right">Alertas</th></tr></thead><tbody>${rows}</tbody></table></div>${othHtml}</div>
      <aside class="r">${incs.length ? incPanel(incs[0][0], incs[0][1], D.inc.length) : '<div class="ipanel"><span class="cap">Incidencias</span><p>Ninguna en esta sede.</p></div>'}
      ${incs.length > 1 ? `<section class="panel"><header><h3>Otras incidencias</h3></header>${incRows(incs.slice(1))}</section>` : ''}</aside></div>`;
  }
  if (focusTech) { const el = document.getElementById('t-' + encodeURIComponent(focusTech)); if (el) setTimeout(() => el.scrollIntoView({block: 'start'}), 0); }
}
function incPage(n){
  curSite = null; chrome('inc', null); crumbs([['Resumen','#'],['Incidencias']]);
  if (!D.inc.length) { $('#view').innerHTML = '<p class="mut">No hay incidencias con relación causa-efecto.</p>'; return; }
  const sel = (n != null && D.inc[n]) ? n : 0;
  $('#view').innerHTML = `<div class="split"><div class="l"><section class="panel"><header><h3>Incidencias correladas (${D.inc.length})</h3></header>${incRows(D.inc.map((x, k) => [x, k]))}</section>
    <h2>Alertas de la incidencia</h2><div class="box">${entryRows(D.inc[sel].ks)}</div></div>
    <aside class="r">${incPanel(D.inc[sel], sel, D.inc.length)}</aside></div>`;
}
// --- grafo de relaciones alrededor de un CI
function graph(i){
  const LIM = 8, W = 236, BW = 212, BH = 40, GAP = 50;
  const score = n => { const r = rollup(n); return maxSev(r) * 100000 + r.size; };
  const cols = {0: [i]}, more = {}, placed = new Set([i]);
  // aguas arriba (de qué depende) y aguas abajo (qué depende de él), nivel a nivel
  // y solo a partir de lo ya dibujado, para que todas las líneas tengan sus dos extremos
  const expand = (dir, maxd) => { for (let d = 1; d <= maxd; d++) {
    const prev = cols[(d-1)*dir] || [], cand = new Map();
    prev.forEach(x => {
      const nb = dir > 0 ? [...(out[x]||[]).map(k => R[k][2]), ...(x === i ? (al[x]||[]) : [])]
                         : (inn[x]||[]).map(k => R[k][0]);
      nb.forEach(n => { if (!placed.has(n)) cand.set(n, 1); }); });
    if (!cand.size) break;
    const list = [...cand.keys()].sort((a, b) => score(b) - score(a) || C[a][1].localeCompare(C[b][1]));
    const lim = dir > 0 ? LIM + 4 : LIM;
    const shown = list.length > lim ? list.slice(0, lim - 1) : list;
    if (list.length > lim) more[d*dir] = list.length - shown.length;
    shown.forEach(n => placed.add(n)); cols[d*dir] = shown; } };
  expand(1, 4); expand(-1, 2);
  const keys = Object.keys(cols).map(Number).sort((a,b)=>a-b), minC = keys[0];
  const pos = new Map(); let H = 0;
  keys.forEach(c => { const n = cols[c].length + (more[c]?1:0); H = Math.max(H, n*GAP);
    cols[c].forEach((x, k) => pos.set(x, [(c-minC)*W + 16, k*GAP + 34])); });
  const Wt = keys.length*W + 20; H += 50;
  const lab = c => c === 0 ? 'elemento' : c > 0 ? `depende de${c>1?' (nivel '+c+')':''}` : `dependen de él${c<-1?' (nivel '+(-c)+')':''}`;
  let svg = `<svg width="${Wt}" height="${H}" xmlns="http://www.w3.org/2000/svg">`;
  keys.forEach(c => svg += `<text class="lbl" x="${(c-minC)*W+16}" y="18">${lab(c)}</text>`);
  R.forEach(([a, r, b]) => { if (pos.has(a) && pos.has(b)) { const [x1,y1] = pos.get(a), [x2,y2] = pos.get(b);
    const xs = x1 < x2 ? [x1+BW, x2] : x1 > x2 ? [x1, x2+BW] : [x1+BW, x2+BW], mx = x1 === x2 ? x1+BW+30 : (xs[0]+xs[1])/2;
    const cross = C[a][4] !== C[b][4] || r === SAME;
    svg += `<path class="e${cross?' x':''}" d="M${xs[0]},${y1+BH/2} C${mx},${y1+BH/2} ${mx},${y2+BH/2} ${xs[1]},${y2+BH/2}"><title>${esc(C[a][2]+' — '+r+' — '+C[b][2])}</title></path>`; } });
  pos.forEach(([x, y], n) => { const own = ciE(n).length, r = rollup(n), s = maxSev(new Set(ciE(n)));
    const nm = C[n][2].length > 29 ? C[n][2].slice(0, 13) + '…' + C[n][2].slice(-15) : C[n][2];
    const info = own ? ` · ${own} alerta(s)` : r.size ? ` · ${r.size} en dependientes` : '';
    svg += `<g class="n${n===i?' sel':''} s${s}" onclick="location.hash='${ciHref(n).slice(1)}'"><title>${esc(C[n][1]+': '+C[n][2])}</title><rect x="${x}" y="${y}" width="${BW}" height="${BH}" rx="7"/>
      <text class="t" x="${x+8}" y="${y+14}">${esc(C[n][1] + info)}</text><text x="${x+8}" y="${y+30}">${esc(nm)}</text></g>`; });
  Object.entries(more).forEach(([c, n]) => { const y = cols[c].length*GAP + 34, x = (c-minC)*W + 16;
    svg += `<g class="n more"><rect x="${x}" y="${y}" width="${BW}" height="${BH}" rx="7" style="stroke-dasharray:4 3"/><text x="${x+8}" y="${y+25}">+${n} más (ver lista de abajo)</text></g>`; });
  return svg + '</svg>';
}
// Ruta hacia arriba dentro de su tecnología (p. ej. VM › ESXi › clúster › vCenter)
function upChain(x){ const p = [x]; let g = 0;
  while (g++ < 8) { const ups = (out[x]||[]).map(k => R[k][2]).filter(b => C[b][4] === C[x][4]);
    const pref = (D.nav || {})[C[x][1]]; const up = (pref && ups.find(b => C[b][1] === pref)) ?? ups[0];
    if (up == null) break; p.push(up); x = up; }
  return p; }
function locations(i){ return (al[i]||[]).filter(a => C[a][4] !== C[i][4]).map(a =>
  `<div class="mut" style="margin-top:4px">${esc(C[a][4])}: ${upChain(a).map(x => `<span class="ty">${esc(C[x][1])}</span>${ciLink(x, false)}`).join(' › ')}</div>`).join(''); }
function ciPage(i, s){
  const c = C[i]; const site = (s && inSite(i, s)) ? s : c[5][0];
  curSite = site;
  const path = []; let x = i, guard = 0;
  while (guard++ < 8) { const ups = (out[x]||[]).map(k => R[k][2]).filter(b => C[b][4] === c[4]);
    const pref = (D.nav || {})[C[x][1]];
    const up = (pref && ups.find(b => C[b][1] === pref)) ?? ups[0]; if (up == null) break; path.unshift(up); x = up; }
  chrome('ci', site);
  crumbs([['Resumen','#'],[site,'#site='+encodeURIComponent(site)],[c[4], '#site='+encodeURIComponent(site)+'&t='+encodeURIComponent(c[4])], ...path.map(p => [C[p][2], '#ci='+p]), [c[2]]]);
  const attrs = Object.entries(c[3]).map(([k,v]) => `<tr><th>${esc(k)}</th><td>${esc(v)}</td></tr>`).join('');
  const kids = {}; (inn[i]||[]).forEach(k => { const [a, r] = R[k]; const key = C[a][1]+' — '+r; (kids[key]=kids[key]||[]).push(a); });
  (al[i]||[]).forEach(a => (kids['Mismo equipo en otra fuente']=kids['Mismo equipo en otra fuente']||[]).push(a));
  const kidsHtml = Object.entries(kids).map(([t, xs], g) => { xs.sort((a,b) => maxSev(rollup(b)) - maxSev(rollup(a)) || C[a][2].localeCompare(C[b][2]));
    const withAl = xs.filter(a => rollup(a).size).length;
    return `<details ${xs.length<=12||withAl?'open':''}><summary><b>${esc(t)}</b> (${xs.length}${withAl?`, ${withAl} con alertas`:''})</summary>
      ${xs.length>12?`<input class="flt" placeholder="Filtrar..." oninput="filt(this)"> <label class="mut"><input type="checkbox" ${withAl?'checked':''} onchange="filt(this.parentNode.previousElementSibling)"> solo con alertas</label>`:''}
      <table class="kids"><tbody>${xs.map(a => { const at = Object.entries(C[a][3]).slice(0, 3).map(([k, v]) => `${k}: ${v}`).join(' · ');
        const loc = (al[a]||[]).filter(b => C[b][4] !== C[a][4]).map(b => upChain(b).map(x => ciLink(x, false)).join(' › ')).join('<br>');
        return `<tr data-n="${esc(C[a][2].toLowerCase())}" data-a="${rollup(a).size?1:0}"><td>${ciLink(a,false)}${at ? `<div class="mut">${esc(at)}</div>` : ''}</td>
        <td class="mut">${loc}</td><td style="text-align:right">${badges(rollup(a))}</td></tr>`; }).join('')}</tbody></table></details>`; }).join('');
  const deps = depTables(i);
  const direct = ciE(i);
  $('#view').innerHTML = `<div class="box"><span class="ty">${esc(c[1])}</span><span class="ty">${esc(c[4])}</span>${c[5].map(x=>`<span class="ty">${esc(x)}</span>`).join('')}${c[5].length>1?`<span class="mut"> · mostrando solo ${esc(site)}</span>`:''}
    <h2 style="margin:6px 0">${esc(c[2])} ${badges(rollup(i))}</h2>${attrs?`<table class="attrs">${attrs}</table>`:''}${locations(i)}
    </div>${deps}
    <h2>Relaciones</h2><div class="graph">${graph(i)}</div>
    <p class="mut">Línea azul discontinua: relación con otra tecnología (p. ej. datastore → LUN, ESXi = host en cabina). Borde de color: alertas del elemento.</p>
    <h2>Alertas de este elemento (${direct.length})</h2>${direct.length ? `<div class="box">${entryRows(direct)}</div>` : '<p class="mut">Ninguna.</p>'}
    ${impactHtml(i)}
    ${kidsHtml ? `<h2>Elementos que dependen de él directamente</h2><div class="box">${kidsHtml}</div>` : ''}`;
  document.querySelectorAll('.flt').forEach(filt);
}
// Todo lo que depende (directa o indirectamente) de un elemento, en cualquier tecnología
function impact(i){
  const seen = new Set([i]), q = [i], by = {};
  (al[i]||[]).forEach(a => { seen.add(a); q.push(a); });
  while (q.length) { const x = q.shift();
    (al[x]||[]).forEach(a => { if (!seen.has(a)) { seen.add(a); q.push(a); (by[C[a][1]] = by[C[a][1]] || []).push(a); } });
    (inn[x]||[]).forEach(k => { const a = R[k][0]; if (!seen.has(a)) { seen.add(a); q.push(a); (by[C[a][1]] = by[C[a][1]] || []).push(a); } }); }
  return by;
}
const IMPACT_FIRST = ['Host group', 'Host en cabina', 'ESXi', 'VM', 'Datastore', 'Virtual server F5', 'Pool F5', 'Tenant F5'];
// Equipo raíz al que pertenece un CI (cabina, chasis, vCenter...), para distinguir nombres repetidos
function ownerOf(i){ let x = i, g = 0; const roots = Object.values(D.dev).flat();
  while (g++ < 10 && !roots.includes(C[x][1])) { const up = (out[x]||[]).map(k => R[k][2]).find(b => C[b][4] === C[i][4]); if (up == null) break; x = up; }
  return x === i ? C[i][5].join('+') : C[x][2]; }
function impactHtml(i){
  const by = impact(i), types = Object.keys(by);
  if (!types.length) return '';
  types.sort((a, b) => (IMPACT_FIRST.indexOf(a) + 1 || 99) - (IMPACT_FIRST.indexOf(b) + 1 || 99) || by[b].length - by[a].length);
  return `<h2>Impacto si falla</h2><div class="box"><p class="mut">Elementos que dependen de este, directa o indirectamente (también en otras tecnologías).</p>` +
    types.map(t => `<details ${by[t].length <= 10 && IMPACT_FIRST.includes(t) ? 'open' : ''}><summary><b>${esc(t)}</b> (${by[t].length})</summary>
      <ul>${by[t].slice(0, 300).map(a => { const dup = by[t].some(b => b !== a && C[b][2] === C[a][2]);
        const where = dup ? ` <span class="mut">· ${esc(ownerOf(a))}</span>` : '';
        return `<li>${ciLink(a, false)}${where} ${ciE(a).length ? badges(new Set(ciE(a))) : ''}</li>`; }).join('')}</ul></details>`).join('') + '</div>';
}
// "Depende de" agrupado por relación, con atributos del destino y de la propia relación
const STATE_CLS = {green: 'ok', red: 'b4', blue: 'b1', gray: 'b0', grey: 'b0', yellow: 'b2', enabled: 'ok',
                   disabled: 'b0', forced_offline: 'b2', offline: 'b4', online: 'ok', available: 'ok'};
function cell(v){ const c = STATE_CLS[String(v).toLowerCase()];
  return c ? `<span class="b ${c === 'ok' ? 'bok' : c}">${esc(v)}</span>` : esc(v); }
// Tabla de miembros de un pool: miembro, nodo, IP, estados y probe efectiva
function memberTable(i, xs){
  const mv = (D.member_view || {})[C[i][1]]; if (!mv) return null;
  const outRel = (x, rel) => (out[x]||[]).map(k => R[k]).filter(r => r[1] === rel);
  const poolMons = outRel(i, mv.probe_rel).map(([, , b, , ra]) => [b, ra]);
  const rows = xs.map(([m]) => {
    const node = (outRel(m, mv.node_rel)[0] || [])[2];
    const own = outRel(m, mv.probe_rel).map(([, , b, , ra]) => [b, ra]);
    const nodeMons = node != null ? outRel(node, mv.probe_rel).map(([, , b, , ra]) => [b, ra]) : [];
    const probes = own.length ? own.map(p => [...p, 'miembro']) : poolMons.map(p => [...p, 'pool']);
    const pr = probes.map(([b, ra, lvl]) => `${ciLink(b, false)} <span class="mut">${esc([C[b][3].Tipo, ra['Lógica'], lvl].filter(Boolean).join(' · '))}</span>`)
      .concat(nodeMons.map(([b, ra]) => `${ciLink(b, false)} <span class="mut">${esc([C[b][3].Tipo, 'nodo'].filter(Boolean).join(' · '))}</span>`)).join('<br>');
    const a = C[m][3], n = node != null ? C[node][3] : {};
    const al = new Set([...ciE(m), ...(node != null ? ciE(node) : [])]);
    return `<tr><td>${ciLink(m, false)}</td><td>${node != null ? ciLink(node, false) : ''}</td><td>${esc(n.IP || '')}</td>
      <td>${cell(n['Estado nodo'] || '')}</td><td>${cell(a.Disponibilidad || '')}</td><td>${cell(a['Estado admin'] || '')}</td>
      <td>${pr || '<span class="mut">sin probe</span>'}</td><td>${al.size ? badges(al) : ''}</td></tr>`; });
  const down = xs.filter(([m]) => String(C[m][3].Disponibilidad).toLowerCase() === 'red').length;
  return `<details open><summary><b>${esc(mv.rel)}</b> · Miembros (${xs.length}${down ? `, ${down} caídos` : ''})</summary>
    <table><thead><tr><th>Miembro</th><th>Nodo</th><th>IP</th><th>Estado nodo</th><th>Disponibilidad</th><th>Estado admin</th><th>Probe efectiva</th><th>Alertas</th></tr></thead>
    <tbody>${rows.join('')}</tbody></table></details>`;
}
function depTables(i){
  const groups = {};
  (out[i]||[]).forEach(k => { const [a, r, b, m, ra] = R[k]; (groups[r] = groups[r] || []).push([b, m, ra || {}]); });
  if (!Object.keys(groups).length) return '';
  const mv = (D.member_view || {})[C[i][1]];
  return '<h2>Depende de</h2><div class="box">' + Object.entries(groups).map(([r, xs]) => {
    if (mv && r === mv.rel) { const t = memberTable(i, xs); if (t) return t; }
    const tAttrs = [...new Set(xs.flatMap(([b]) => Object.keys(C[b][3])))];
    const rAttrs = [...new Set(xs.flatMap(([, , ra]) => Object.keys(ra)))];
    const types = [...new Set(xs.map(([b]) => C[b][1]))];
    xs.sort((p, q) => (ciE(q[0]).length - ciE(p[0]).length) || C[p[0]][2].localeCompare(C[q[0]][2]));
    const nAl = xs.filter(([b]) => ciE(b).length).length;
    return `<details ${xs.length <= 40 ? 'open' : ''}><summary><b>${esc(r)}</b> · ${esc(types.join(', '))} (${xs.length}${nAl ? `, ${nAl} con alertas` : ''})${(D.info_rels||[]).includes(r) ? ' <span class="mut">informativa</span>' : ''}</summary>
      <table><thead><tr><th>${esc(types.join(' / '))}</th>${tAttrs.map(t => `<th>${esc(t)}</th>`).join('')}${rAttrs.map(t => `<th>${esc(t)}</th>`).join('')}<th>Alertas</th></tr></thead><tbody>
      ${xs.map(([b, m, ra]) => `<tr><td>${ciLink(b, false)}${m !== 'métricas' ? ` <span class="mut">(${esc(m)})</span>` : ''}</td>
        ${tAttrs.map(t => `<td>${cell(C[b][3][t] ?? '')}</td>`).join('')}${rAttrs.map(t => `<td>${cell(ra[t] ?? '')}</td>`).join('')}
        <td>${ciE(b).length ? badges(new Set(ciE(b))) : ''}</td></tr>`).join('')}</tbody></table></details>`; }).join('') + '</div>';
}
function filt(inp){ const box = inp.parentNode, v = inp.value.toLowerCase(), only = box.querySelector('input[type=checkbox]')?.checked;
  box.querySelectorAll('tr[data-n]').forEach(tr => tr.style.display = (!v || tr.dataset.n.includes(v)) && (!only || tr.dataset.a === '1') ? '' : 'none'); }
function searchPage(q){
  chrome('search', null); crumbs([['Resumen','#'],['Buscar: '+q]]); q = q.toLowerCase();
  const res = C.map((c,i)=>i).filter(i => C[i][2].toLowerCase().includes(q)).slice(0, 300)
    .sort((a,b) => maxSev(rollup(b)) - maxSev(rollup(a)));
  $('#view').innerHTML = `<div class="box"><table><tbody>${res.map(i => `<tr><td>${ciLink(i)}</td><td class="mut">${esc(C[i][4])} · ${esc(C[i][5].join(', '))}</td><td style="text-align:right">${badges(rollup(i))}</td></tr>`).join('') || '<tr><td class="mut">Sin resultados</td></tr>'}</tbody></table></div>`;
}
function route(){
  const p = new URLSearchParams(location.hash.slice(1));
  curSite = p.get('site');
  window.scrollTo(0, 0);
  if (p.has('ci')) ciPage(+p.get('ci'), p.get('site')); else if (p.has('site')) sitePage(p.get('site'), p.get('t'));
  else if (location.hash.startsWith('#inc')) incPage(p.has('inc') ? +p.get('inc') : null);
  else if (p.has('q')) searchPage(p.get('q')); else home();
}
let tq; $('#q').addEventListener('input', e => { clearTimeout(tq); tq = setTimeout(() => { location.hash = e.target.value ? 'q=' + encodeURIComponent(e.target.value) : ''; }, 300); });
window.addEventListener('hashchange', route); route();
"""


# --------------------------------------------------------------------------
# Datos de demostración
# --------------------------------------------------------------------------
def demo_data():
    S = []

    def s(name, **lb):
        S.append({"__name__": name, **lb})

    # Huawei OceanStor
    # LUN, controladora propietaria, pool, WWN, hosts (ESXi) a los que está mapeada
    esx_mad = ["ESX-MAD-01", "ESX-MAD-02", "ESX-MAD-03"]
    oceanstor = {
        "OS-MAD-01": [("LUN_VMFS_SAP", "CTE0.A", "StoragePool001", "6008c0e1000a0b0c0000000000000001", esx_mad),
                      ("LUN_VMFS_ORA", "CTE0.B", "StoragePool002", "6008c0e1000a0b0c0000000000000002", esx_mad),
                      ("LUN_VMFS_TMPL", "CTE0.B", "StoragePool002", "6008c0e1000a0b0c0000000000000003", esx_mad),
                      ("LUN_VMFS_ISO", "CTE0.A", "StoragePool001", "6008c0e1000a0b0c0000000000000004", esx_mad)],
        "OS-BCN-01": [("LUN_FILE_01", "CTE0.A", "StoragePool001", "6008c0e1000a0b0d0000000000000001", ["SRV-FILE-01"])],
    }
    for arr, luns in oceanstor.items():
        for c in ("CTE0.A", "CTE0.B"):
            s("oceanstor_controller_health", job="oceanstor", array=arr, controller=c, health_status="normal")
            s("oceanstor_controller_cpu_usage", job="oceanstor", array=arr, controller=c)
        for p in sorted({x[2] for x in luns}):
            s("oceanstor_pool_used_ratio", job="oceanstor", array=arr, pool=p)
        for lun, c, p, wwn, hosts in luns:
            s("oceanstor_lun_latency_ms", job="oceanstor", array=arr, controller=c, pool=p, lun=lun, wwn=wwn.upper())
            s("oceanstor_lun_iops", job="oceanstor", array=arr, controller=c, pool=p, lun=lun, wwn=wwn.upper())
            for h in hosts:
                s("oceanstor_host_lun_mapping", job="oceanstor", array=arr, host=h, lun=lun)
    # VMware vCenter / VCF (etiquetas al estilo de vmware_exporter)
    datastores = [("DS_SAP_01", "6008c0e1000a0b0c0000000000000001"), ("DS_ORA_01", "6008c0e1000a0b0c0000000000000002"),
                  ("DS_TMPL", "6008c0e1000a0b0c0000000000000003"), ("DS_ISO", "6008c0e1000a0b0c0000000000000004")]
    for ds, wwn in datastores:
        s("vmware_datastore_capacity_size", job="vmware", dc_name="DC-MAD", ds_name=ds, ds_naa=f"naa.{wwn}")
        s("vmware_datastore_latency_ms", job="vmware", dc_name="DC-MAD", ds_name=ds)
        for esx in esx_mad:
            s("vmware_host_datastore_mounted", job="vmware", dc_name="DC-MAD", cluster_name="CL-MAD-PROD",
              host_name=f"{esx.lower()}.corp.local", ds_name=ds)
    for esx in esx_mad:
        s("vmware_host_memory_usage", job="vmware", dc_name="DC-MAD", cluster_name="CL-MAD-PROD",
          host_name=f"{esx.lower()}.corp.local")
    vms = [("srv-sap-01", "esx-mad-01", "DS_SAP_01"), ("srv-sap-02", "esx-mad-01", "DS_SAP_01"),
           ("srv-ora-01", "esx-mad-02", "DS_ORA_01"), ("srv-ora-02", "esx-mad-02", "DS_ORA_01"),
           ("srv-bck-01", "esx-mad-03", "DS_ORA_01"), ("srv-bck-02", "esx-mad-03", "DS_ORA_01")]
    for vm, esx, ds in vms:
        for m in ("vmware_vm_cpu_ready_ms", "vmware_vm_mem_usage", "vmware_vm_power_state"):
            s(m, job="vmware", dc_name="DC-MAD", cluster_name="CL-MAD-PROD",
              host_name=f"{esx}.corp.local", vm_name=vm)
        s("vmware_vm_datastore_usage", job="vmware", vm_name=vm, ds_name=ds)
    # Dell EMC Unity
    for p in ("Pool0", "Pool1"):
        s("unity_pool_used_ratio", job="unity", system="UNITY-BCN-01", pool=p)
    for lun, p, h in [("vol_web01", "Pool0", "srv-web-01"), ("vol_web02", "Pool0", "srv-web-02"),
                      ("vol_app01", "Pool1", None)]:
        s("unity_lun_iops", job="unity", system="UNITY-BCN-01", pool=p, lun=lun)
        if h:
            s("unity_host_lun_info", job="unity", system="UNITY-BCN-01", host=h, lun=lun)
    # IBM FlashSystem
    for g in ("Pool0", "Pool1"):
        s("ibm_mdiskgrp_used_ratio", job="ibm", storage_system="IBM-FS-01", mdiskgrp=g)
    for vd, g, h in [("vd_backup01", "Pool1", "srv-bck-01"), ("vd_backup02", "Pool1", "srv-bck-02"),
                     ("vd_arch01", "Pool0", "srv-bck-01")]:
        s("ibm_vdisk_latency_ms", job="ibm", storage_system="IBM-FS-01", mdiskgrp=g, vdisk=vd)
        s("ibm_host_vdisk_map", job="ibm", storage_system="IBM-FS-01", host=h, vdisk=vd)
    # F5 BIG-IP
    for dev, vs, pool, members in [("bigip-mad-01", "vs_sap_https", "pool_sap_https", ["10.1.1.11", "10.1.1.12"]),
                                   ("bigip-bcn-01", "vs_web_https", "pool_web_https", ["10.2.1.31", "10.2.1.32"])]:
        s("f5_virtual_server_status", job="f5", bigip=dev, virtual_server=vs, pool=pool)
        for m in members:
            s("f5_pool_member_status", job="f5", bigip=dev, pool=pool, member=f"{m}:443")
    # node_exporter
    hosts = [("srv-sap-01", "10.1.1.11"), ("srv-sap-02", "10.1.1.12"), ("srv-ora-01", "10.1.1.21"),
             ("srv-ora-02", "10.1.1.22"), ("srv-file-01", "10.2.1.41"), ("srv-web-01", "10.2.1.31"),
             ("srv-web-02", "10.2.1.32"), ("srv-bck-01", "10.1.1.51"), ("srv-bck-02", "10.1.1.52")]
    for h, ip in hosts:
        inst = f"{ip}:9100"
        s("up", job="node", instance=inst)
        s("node_uname_info", job="node", instance=inst, nodename=f"{h}.corp.local")
        for dev, mp in (("/dev/sda1", "/"), ("/dev/sdb1", "/data")):
            s("node_filesystem_avail_bytes", job="node", instance=inst, device=dev, mountpoint=mp, fstype="ext4")
        for cpu in range(4):
            for mode in ("idle", "user", "system"):
                s("node_cpu_seconds_total", job="node", instance=inst, cpu=str(cpu), mode=mode)

    now = datetime.now(timezone.utc)

    def al(name, sev, mins, summary, silenced=False, **lb):
        return {"labels": {"alertname": name, "severity": sev, "datacenter": "es", **lb},
                "annotations": {"summary": summary},
                "startsAt": (now - timedelta(minutes=mins)).isoformat().replace("+00:00", "Z"),
                "status": {"state": "suppressed" if silenced else "active",
                           "silencedBy": ["demo"] if silenced else [], "inhibitedBy": []},
                "fingerprint": f"{abs(hash((name, tuple(lb.items())))):x}"}

    alerts = [
        al("OceanStorControllerFault", "critical", 62, "Controladora CTE0.A de OS-MAD-01 en estado Fault",
           job="oceanstor", array="OS-MAD-01", controller="CTE0.A"),
        al("OceanStorLunLatencyHigh", "warning", 60, "Latencia de LUN_VMFS_SAP > 20 ms",
           job="oceanstor", array="OS-MAD-01", controller="CTE0.A", pool="StoragePool001",
           lun="LUN_VMFS_SAP", wwn="6008C0E1000A0B0C0000000000000001"),
        al("VMwareDatastoreLatencyHigh", "warning", 59, "Latencia del datastore DS_SAP_01 > 30 ms",
           job="vmware", dc_name="DC-MAD", ds_name="DS_SAP_01"),
        al("HostDiskIOWaitHigh", "warning", 58, "iowait > 40 % en 10.1.1.11",
           job="node", instance="10.1.1.11:9100"),
        al("F5PoolMemberDown", "critical", 57, "Miembro 10.1.1.12:443 caído en pool_sap_https",
           job="f5", bigip="bigip-mad-01", pool="pool_sap_https", member="10.1.1.12:443"),
        al("VMwareHostMemoryHigh", "critical", 45, "Memoria del ESXi esx-mad-02 al 97 %",
           job="vmware", dc_name="DC-MAD", cluster_name="CL-MAD-PROD", host_name="esx-mad-02.corp.local"),
        al("VMwareVMCpuReadyHigh", "warning", 44, "CPU ready de srv-ora-01 > 10 %",
           job="vmware", dc_name="DC-MAD", cluster_name="CL-MAD-PROD", host_name="esx-mad-02.corp.local",
           vm_name="srv-ora-01"),
        al("VMwareVMCpuReadyHigh", "warning", 44, "CPU ready de srv-ora-02 > 10 %",
           job="vmware", dc_name="DC-MAD", cluster_name="CL-MAD-PROD", host_name="esx-mad-02.corp.local",
           vm_name="srv-ora-02"),
        al("HostHighCpuLoad", "warning", 41, "CPU > 90 % en 10.1.1.21",
           job="node", instance="10.1.1.21:9100"),
        al("HostHighCpuLoad", "warning", 30, "CPU > 90 % en 10.2.1.41",
           job="node", instance="10.2.1.41:9100"),
        al("UnityPoolAlmostFull", "warning", 300, "Pool0 de UNITY-BCN-01 al 92 %",
           job="unity", system="UNITY-BCN-01", pool="Pool0"),
        al("HostFilesystemAlmostFull", "warning", 120, "/data al 95 % en 10.2.1.31",
           job="node", instance="10.2.1.31:9100", device="/dev/sdb1", mountpoint="/data", fstype="ext4"),
        al("IBMPoolAlmostFull", "warning", 200, "Pool0 de IBM-FS-01 al 90 %",
           job="ibm", storage_system="IBM-FS-01", mdiskgrp="Pool0"),
        al("IBMVdiskLatencyHigh", "warning", 25, "Latencia de vd_backup01 > 30 ms",
           job="ibm", storage_system="IBM-FS-01", mdiskgrp="Pool1", vdisk="vd_backup01"),
        al("IBMVdiskLatencyHigh", "warning", 24, "Latencia de vd_backup02 > 30 ms",
           job="ibm", storage_system="IBM-FS-01", mdiskgrp="Pool1", vdisk="vd_backup02"),
        al("OceanStorDiskPreFail", "info", 900, "Disco CTE0.10 con errores de sector", silenced=True,
           job="oceanstor", array="OS-BCN-01", disk="CTE0.10"),
        al("Watchdog", "none", 5000, "Alerta de vida del pipeline"),
    ]
    return alerts, {"status": "success", "data": S}


# --------------------------------------------------------------------------
# Programa principal
# --------------------------------------------------------------------------
def load_config(path):
    cfg = json.loads(json.dumps(DEFAULT_CONFIG))
    if path:
        with open(path, encoding="utf-8") as f:
            user = json.load(f)
        cfg.update(user)
        rf = cfg.get("relations_file") or {}
        base = os.path.dirname(os.path.abspath(path))     # rutas relativas al fichero de config
        rf["files"] = [f if os.path.isabs(f) else os.path.join(base, f) for f in rf.get("files") or []]
    return cfg


def collect(args, cfg, http):
    """Devuelve (alertas_normalizadas, series, descripción de la fuente)."""
    if args.demo:
        raw_alerts, raw_series = demo_data()
        return (normalize_alerts(raw_alerts), normalize_series(raw_series), "datos de demostración",
                raw_alerts, raw_series, [], None, [])
    raw_alerts = raw_series = None
    src = []
    # --prometheus se puede repetir y también admite URLs separadas por comas
    proms = [u.strip() for p in (args.prometheus or []) for u in p.split(",") if u.strip()]
    if args.alerts_file:
        with open(args.alerts_file, encoding="utf-8") as f:
            raw_alerts = json.load(f)
        src.append(os.path.basename(args.alerts_file))
    elif args.alertmanager:
        raw_alerts = fetch_alerts_alertmanager(http, args.alertmanager)
        src.append(f"Alertmanager {args.alertmanager}")
    elif proms:
        merged, seen, ok = [], set(), []
        for url in proms:
            try:
                for a in normalize_raw_list(fetch_alerts_prometheus(http, url)):
                    key = tuple(sorted((a.get("labels") or {}).items()))
                    if key not in seen:          # mismas alertas en Prometheus en HA
                        seen.add(key)
                        merged.append(a)
                ok.append(url)
            except Exception as e:
                log(f"AVISO: no se pudieron leer las alertas de {url}: {e}")
        if not ok:
            raise SystemExit("No se ha podido leer ningún Prometheus")
        raw_alerts = merged
        src.append(f"alertas de Prometheus ({', '.join(ok)})")
    else:
        raise SystemExit("Indica --alertmanager, --prometheus, --alerts-file o --demo")
    if args.series_file:
        with open(args.series_file, encoding="utf-8") as f:
            raw_series = json.load(f)
        src.append(os.path.basename(args.series_file))
    elif proms and not args.no_series:
        matches = args.series_match or cfg["series_match"]
        merged, seen, ok = [], set(), []
        for url in proms:
            try:
                n0 = len(merged)
                for sr in normalize_series(fetch_series(http, url, matches, cfg["series_lookback_minutes"])):
                    key = frozenset(sr.items())
                    if key not in seen:          # series repetidas entre Prometheus
                        seen.add(key)
                        merged.append(sr)
                ok.append(f"{url} ({len(merged) - n0} series nuevas)")
            except Exception as e:
                log(f"AVISO: no se pudieron leer las series de {url}: {e}")
        raw_series = merged
        if ok:
            src.append("series de " + ", ".join(ok))
    # Valores "CMDB" y metadatos
    raw_values, raw_meta = None, None
    if args.values_file:
        with open(args.values_file, encoding="utf-8") as f:
            raw_values = json.load(f)
        src.append(os.path.basename(args.values_file))
    elif proms and (args.values or args.save_raw):
        vals, meta, seen, ok = [], {}, set(), []
        for url in proms:
            try:
                for v in normalize_values(fetch_values(http, url, cfg["cmdb_metrics_regex"])):
                    key = frozenset((k, x) for k, x in v.items() if k != "__value__")
                    if key not in seen:
                        seen.add(key)
                        vals.append(v)
                ok.append(url)
                try:
                    md = fetch_metadata(http, url)
                    meta.update(md.get("data", md) if isinstance(md, dict) else {})
                except Exception as e:
                    log(f"AVISO: sin metadatos de {url}: {e}")
            except Exception as e:
                log(f"AVISO: no se pudieron leer los valores de {url}: {e}")
        raw_values, raw_meta = vals, meta
        if ok:
            src.append(f"{len(vals)} valores CMDB")
    cmdb_rows = []
    for path in args.cmdb or []:
        cmdb_rows += load_cmdb_csv(path)
        src.append(os.path.basename(path))
    return (normalize_alerts(raw_alerts, args.include_pending), normalize_series(raw_series),
            " + ".join(src), raw_alerts, raw_series, normalize_values(raw_values), raw_meta, cmdb_rows)


def page_html(args, cfg, res, topo, meta, refresh=None):
    """Vista principal: drill-down por sede y tecnología si hay modelo CMDB; si no, el listado."""
    lista = render_html(res, topo, meta, refresh=refresh)     # también rellena DS_BACKING
    if args.view == "lista" or not getattr(topo, "cmdb", None):
        return lista
    return render_drill_html(res, topo, topo.cmdb, cfg, meta, refresh=refresh)


def write_pages(args, cfg, res, topo, meta):
    base, ext = os.path.splitext(args.output)
    meta = dict(meta, lista=os.path.basename(f"{base}_lista{ext or '.html'}"))
    with open(args.output, "w", encoding="utf-8") as f:
        f.write(page_html(args, cfg, res, topo, meta))
    if args.view != "lista" and getattr(topo, "cmdb", None):
        with open(f"{base}_lista{ext or '.html'}", "w", encoding="utf-8") as f:
            f.write(render_html(res, topo, meta))


def build(args, cfg, http):
    t0 = time.time()
    alerts, series, src, raw_a, raw_s, values, meta, cmdb_rows = collect(args, cfg, http)
    ex = set(cfg.get("exclude_jobs") or ())
    if ex:                                   # jobs que no forman parte del proyecto (p. ej. acosv2)
        alerts = [a for a in alerts if a["labels"].get("job") not in ex]
        series = [x for x in series if x.get("job") not in ex]
        values = [v for v in values if v.get("job") not in ex]
    if args.relations:
        cfg.setdefault("relations_file", {})
        fs = list(cfg["relations_file"].get("files") or [])
        cfg["relations_file"]["files"] = fs + [f for f in args.relations if f not in fs]
    if args.save_raw:
        os.makedirs(args.save_raw, exist_ok=True)
        with open(os.path.join(args.save_raw, "alerts.json"), "w", encoding="utf-8") as f:
            json.dump(raw_a, f, ensure_ascii=False, indent=1, default=str)
        with open(os.path.join(args.save_raw, "series.json"), "w", encoding="utf-8") as f:
            json.dump(raw_s, f, ensure_ascii=False, default=str)
        if values:
            with open(os.path.join(args.save_raw, "values.json"), "w", encoding="utf-8") as f:
                json.dump(values, f, ensure_ascii=False, default=str)
        if meta:
            with open(os.path.join(args.save_raw, "metadata.json"), "w", encoding="utf-8") as f:
                json.dump(meta, f, ensure_ascii=False, indent=1, default=str)
        log(f"Datos crudos guardados en {args.save_raw}/ ({len(values)} valores, {len(meta or {})} métricas con metadatos)")
    # Las series de los valores y de la CMDB también aportan relaciones
    if values or cmdb_rows:
        seen = {frozenset(x.items()) for x in series}
        for v in values:
            lab = {k: x for k, x in v.items() if k != "__value__"}
            key = frozenset(lab.items())
            if key not in seen:
                seen.add(key)
                series.append(lab)
        series = series + cmdb_rows
    bstats = Counter()
    if cfg.get("datastore_lun_bridge"):
        extra, bstats = build_datastore_lun_bridge(series, cfg["datastore_lun_bridge"], values,
                                                   cfg.get("size_match_tolerance", 0.01))
        series = series + extra
    topo = Topology(series, cfg)
    topo.bridge_stats = bstats
    topo.ds_backing = datastore_backing(series, cfg)
    topo.cmdb = None
    if cfg.get("cmdb_model"):
        for k, a in enumerate(alerts):
            a["_idx"] = k
        topo.cmdb = build_cmdb(series, cfg, [dict(a["labels"]) for a in alerts], values)
        model = cfg["cmdb_model"]
        cnt = Counter(c["type"] for c in topo.cmdb["cis"].values())
        log("Equipos por tecnología: " + ", ".join(
            f"{t} {sum(cnt[x] for x in model.get('device_types', {}).get(t, []))}" for t in model.get("technologies", {})))
        node = topo.map_cmdb(topo.cmdb)
        if not args.no_cmdb_model:
            st = topo.apply_cmdb(topo.cmdb, node)
            log("Modelo CMDB aplicado: " + ", ".join(f"{k}: {v}" for k, v in st.items()))
    res = Correlator(topo, alerts, cfg).run()
    log(f"{len(alerts)} alertas, {topo.n_series} series -> {res['n_elements']} elementos, "
        f"{len(res['groups'])} incidencias, {len(res['summary'])} tipos de aviso ({time.time() - t0:.1f} s)")
    return res, topo, {"source": src}


def main():
    ap = argparse.ArgumentParser(description="Correlación de alertas de Prometheus/Alertmanager -> HTML")
    ap.add_argument("--alertmanager", help="URL de Alertmanager, p. ej. http://alertmanager:9093")
    ap.add_argument("--prometheus", action="append",
                    help="URL de Prometheus, p. ej. http://prometheus:9090. Se puede repetir "
                         "(--prometheus URL1 --prometheus URL2) o separar por comas")
    ap.add_argument("--alerts-file", help="JSON guardado de /api/v2/alerts o /api/v1/alerts")
    ap.add_argument("--series-file", help="JSON guardado de /api/v1/series")
    ap.add_argument("--values", action="store_true",
                    help="Leer también los valores de las métricas CMDB (cmdb_metrics_regex)")
    ap.add_argument("--values-file", help="JSON guardado con los valores CMDB (values.json de --save-raw)")
    ap.add_argument("--relations", action="append", metavar="FICHERO",
                    help="fichero de relaciones manuales (líneas 'Vsys_PA:CEFTA => F5:EMP'); se puede repetir. "
                         "También en la configuración: relations_file.files")
    ap.add_argument("--cmdb", action="append", metavar="CSV",
                    help="CSV de CMDB (columnas = etiquetas, p. ej. vcenter;datastore;array;lun). Repetible")
    ap.add_argument("--series-match", action="append",
                    help='Selector de series a analizar (repetible). Por defecto {job=~".+"}')
    ap.add_argument("--no-series", action="store_true", help="No leer series (solo etiquetas de alertas)")
    ap.add_argument("--include-pending", action="store_true", help="Incluir alertas pending (Prometheus)")
    ap.add_argument("--demo", action="store_true", help="Usar datos de demostración")
    ap.add_argument("--config", help="Fichero JSON con configuración (ver --dump-config)")
    ap.add_argument("--dump-config", action="store_true", help="Mostrar la configuración por defecto y salir")
    ap.add_argument("-o", "--output", default="alertas.html", help="Fichero HTML de salida")
    ap.add_argument("--json", help="Guardar también el resultado en JSON")
    ap.add_argument("--style", choices=["a", "c"], help="estilo inicial de la web: a = consola NOC, c = tabla operativa "
                    "(el usuario puede cambiarlo en la página; por defecto ui_style de la config o 'a')")
    ap.add_argument("--theme", choices=["dark", "light"], help="tema inicial (por defecto el del sistema operativo)")
    ap.add_argument("--view", choices=["drill", "lista"], default="drill",
                    help="drill: sede > tecnología > equipo > elemento (requiere cmdb_model); lista: incidencias y avisos")
    ap.add_argument("--no-cmdb-model", action="store_true",
                    help="No aplicar el modelo CMDB de la config al correlar (solo inferencia)")
    ap.add_argument("--cmdb-export", metavar="DIR",
                    help="Generar la CMDB (CSV + web) a partir de las series y salir. Usa cmdb_model de la config")
    ap.add_argument("--save-raw", metavar="DIR",
                    help="Guardar alertas, series, valores CMDB y metadatos en DIR")
    ap.add_argument("--serve", action="store_true", help="Servir la página por HTTP")
    ap.add_argument("--host", default="0.0.0.0")
    ap.add_argument("--port", type=int, default=8080)
    ap.add_argument("--refresh", type=int, default=60, help="Segundos entre regeneraciones (con --serve)")
    ap.add_argument("--timeout", type=int, default=30)
    ap.add_argument("--insecure", action="store_true", help="No verificar certificados TLS")
    ap.add_argument("--basic-auth", help="usuario:contraseña (o variable CORR_BASIC_AUTH)")
    ap.add_argument("--bearer-token", help="Token (o variable CORR_BEARER_TOKEN)")
    args = ap.parse_args()

    if args.dump_config:
        print(json.dumps(DEFAULT_CONFIG, indent=2, ensure_ascii=False))
        return
    cfg = load_config(args.config)
    if args.style:
        cfg["ui_style"] = args.style
    if args.theme:
        cfg["ui_theme"] = args.theme
    http = Http(args.timeout, args.insecure,
                args.basic_auth or os.environ.get("CORR_BASIC_AUTH"),
                args.bearer_token or os.environ.get("CORR_BEARER_TOKEN"))

    if args.cmdb_export:
        t0 = time.time()
        if args.series_file:
            with open(args.series_file, encoding="utf-8") as f:
                series = normalize_series(json.load(f))
            src = os.path.basename(args.series_file)
        else:
            proms = [u.strip() for p in (args.prometheus or []) for u in p.split(",") if u.strip()]
            if not proms:
                raise SystemExit("--cmdb-export necesita --series-file o --prometheus")
            series, seen = [], set()
            for url in proms:
                for sr in normalize_series(fetch_series(http, url, args.series_match or cfg["series_match"],
                                                        cfg["series_lookback_minutes"])):
                    k = frozenset(sr.items())
                    if k not in seen:
                        seen.add(k)
                        series.append(sr)
            src = ", ".join(proms)
        for path in args.cmdb or []:
            series += load_cmdb_csv(path)
        cmdb = build_cmdb(series, cfg)
        write_cmdb(cmdb, args.cmdb_export, {"source": src})
        n = Counter(c["type"] for c in cmdb["cis"].values())
        log(f"CMDB: {len(cmdb['cis'])} elementos, {len(cmdb['rels'])} relaciones ({time.time() - t0:.1f} s) -> "
            f"{args.cmdb_export}/cmdb.html, cmdb_ci.csv, cmdb_relaciones.csv")
        log("  " + ", ".join(f"{t}: {k}" for t, k in n.most_common()))
        return

    if not args.serve:
        res, topo, meta = build(args, cfg, http)
        write_pages(args, cfg, res, topo, meta)
        if args.json:
            with open(args.json, "w", encoding="utf-8") as f:
                json.dump(result_json(res), f, ensure_ascii=False, indent=2)
        log(f"Página generada: file://{os.path.abspath(args.output)}")
        return

    cache = {"t": 0, "html": "", "json": "{}", "lista": ""}
    lock = threading.Lock()

    def get_page():
        with lock:
            if time.time() - cache["t"] >= args.refresh:
                try:
                    res, topo, meta = build(args, cfg, http)
                    meta = dict(meta, lista="lista")
                    cache["html"] = page_html(args, cfg, res, topo, meta, refresh=args.refresh)
                    cache["lista"] = render_html(res, topo, meta, refresh=args.refresh)
                    cache["json"] = json.dumps(result_json(res), ensure_ascii=False, indent=2)
                except Exception as e:  # se sigue sirviendo la última versión buena
                    log(f"Error al regenerar: {e}")
                    if not cache["html"]:
                        cache["html"] = f"<pre>Error: {esc(e)}</pre>"
                cache["t"] = time.time()
            return cache["html"], cache["json"], cache["lista"] or cache["html"]

    class H(BaseHTTPRequestHandler):
        def do_GET(self):
            page, data, lista = get_page()
            if self.path.startswith("/data.json"):
                body, ctype = data.encode(), "application/json; charset=utf-8"
            elif self.path == "/lista" or self.path.startswith("/lista?"):
                body, ctype = lista.encode(), "text/html; charset=utf-8"
            elif self.path in ("/", "/index.html") or self.path.startswith("/?"):
                body, ctype = page.encode(), "text/html; charset=utf-8"
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *a):
            pass

    get_page()
    srv = ThreadingHTTPServer((args.host, args.port), H)
    shown = "localhost" if args.host in ("0.0.0.0", "") else args.host
    log(f"Sirviendo en http://{shown}:{args.port}/  (JSON en /data.json) — Ctrl+C para parar")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
