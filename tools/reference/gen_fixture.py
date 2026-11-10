#!/usr/bin/env python3
"""Genera una captura sintética (series, valores, alertas y relaciones manuales) que ejercita
todas las reglas de un config_junta.json. Sirve para comprobar que el motor en Go produce la
misma CMDB que build_cmdb del correlador Python.

Uso: gen_fixture.py CONFIG DIR_SALIDA
"""
import json
import re
import sys
from pathlib import Path

POOLS = {
    # vSphere / vmware_exporter
    "vcenter": ["vc04", "vc05"],
    "cluster": ["CL-PROD", "CL-ORACLE"],
    "cluster_name": ["CL-PROD", "CL-ORACLE"],
    "host": ["s00e-hses-48.junta.es", "s01e-hses-12.junta.es", "S00EHSES48"],
    "host_name": ["s00e-hses-48.junta.es", "s01e-hses-12.junta.es"],
    "esxi_host": ["s00e-hses-48.junta.es"],
    "vm": ["ccobuz38", "s00e-hses-48", "vbproxy01"],
    "vm_name": ["ccobuz38", "ccobuz39", "s00e-lsal-vbhard01-adm"],
    "datastore": ["S00CAH01_ORG1_X_VC04-CORE_01", "S01CAS01_ORG2_Y_VC04-DATA_0A", "local-ds"],
    "ds_name": ["S00CAH01_ORG1_X_VC04-CORE_01", "S01CAS01_ORG2_Y_VC04-DATA_0A"],
    "dc_name": ["DC-CICA", "DC-ZOCO"],
    "tags": ["CJDA-CCOR-001, backup", "IDS-BBDD-001"],
    "NB_LAST_BACKUP": ["2026-11-08", "2026-11-09"],
    # Cabinas
    "array": ["unity-1-cica", "unity-2-zoco"],
    "name": ["ORG1_CORE_01", "ORG2_DATA_0A", "ORG1_OTRA_02"],
    "nasserver": ["nas01", "nas02"],
    "sede": ["CICA", "ZOCO"],
    "pool": ["POOL_A", "POOL_B"],
    "vstore": ["vs1", "vs2"],
    "lun": ["LUN_00_REPO-VB-vbhard01_1", "LUN_A", "LUN_B"],
    "filesystem": ["FS_NFS_E_ALFRESCO", "FS_B"],
    "hostgroup": ["HG_TST", "HG_PRO"],
    "lungroup": ["LG1", "LG2"],
    "location": ["/fs/a", "/fs/b"],
    "wwn": ["6008c0e1aabbccdd0011223344556677", "6008c0e1aabbccdd0011223344556678"],
    "volume": ["VOL_1", "VOL_2"],
    "uid": ["600507680C8101234000000000000001", "600507680C8101234000000000000002"],
    "host_cluster": ["HC_PRO", "HG_TST"],
    "partition": ["PART_HA_1", "SANDETEL_PRO", "Common"],
    "system_name": ["S00ACAS21", "S01ACAS21"],
    # F5
    "chassis": ["s00-velos-01", "s01-velos-01"],
    "component": ["blade-1", "blade-2"],
    "tenant": ["s00-r-bac-01-t09-01", "s01-r-bac-01-t09-01"],
    "virtual_server": ["vs_correo_443", "vs_imap"],
    "node": ["10.1.1.10", "ccobuz38"],
    "port": ["443", "993"],
    "organismo": ["EMP_CICA", "EMP_ZOCO", "ADA_CICA"],
    "monitor": ["http_mon", "tcp_mon"],
    "address": ["10.1.1.10", "10.1.1.11"],
    "state": ["available", "offline"],
    "role": ["tenant"],
    # Palo Alto
    "instance": ["10.47.208.11:9100", "10.47.208.131:9100", "vcnppu01.junta.es:443"],
    "exported_instance": ["10.47.208.12", ""],
    "hostname": ["PA-CICA-1", "PA-ZOCO-1"],
    "display_name": ["FW_CEFTA", "FW_OTRO"],
    "vsys": ["vsys1", "vsys2"],
    "interface": ["ethernet1/1", "ethernet1/2"],
    "peer_mgmt_ip": ["10.47.208.12", "10.47.208.132"],
    "mode": ["active-passive"],
    # Veeam
    "site": ["CICA", "ZOCO"],
    "version": ["12.1"],
}

# Valores candidatos para cumplir las expresiones de "where".
WHERE_CANDIDATES = ["chassis", "chassis_partition", "tenant", "flashsystem", "unity", "group",
                    "F5S_TenantDown", "panos_exporter", "OceanStorFilesystemFull",
                    "LUN_00_REPO-VB-vbhard01_1", "LUN_01_REPO-VB-vbhard02_3", "x-1"]


def pick_where(pattern):
    rx = re.compile(pattern)
    for c in WHERE_CANDIDATES:
        if rx.fullmatch(c):
            return c
    raise SystemExit(f"sin candidato para where {pattern!r}")


def metric_for(regex):
    for c in ["f5os_memory_used_bytes", "f5os_cpu_utilization_percent"]:
        if re.fullmatch(regex, c):
            return c
    raise SystemExit(f"sin métrica para {regex!r}")


def rule_labels(rule):
    labs = set()
    for spec in rule["cis"].values():
        labs |= set(spec["key"])
        labs.add(spec.get("name", spec["key"][-1]))
        labs |= set((spec.get("attrs") or {}).values())
    for rel in rule.get("rels", []):
        if len(rel) > 3:
            labs |= set(rel[3].values())
    for k, v in (rule.get("computed") or {}).items():
        if isinstance(v, list):
            labs.add(v[0])
        elif "first" in v:
            labs |= set(v["first"])
        elif "sorted" in v:
            labs |= set(v["sorted"])
        elif "from" in v:
            labs.add(v["from"])
        elif "format" in v:
            labs |= set(re.findall(r"\{([^{}]+)\}", v["format"]))
    computed = set((rule.get("computed") or {}).keys())
    return sorted(labs - computed)


def value(label, i):
    pool = POOLS.get(label) or [f"{label}-a", f"{label}-b"]
    return pool[i % len(pool)]


def main():
    cfg = json.load(open(sys.argv[1], encoding="utf-8"))
    out = Path(sys.argv[2])
    out.mkdir(parents=True, exist_ok=True)
    series, values, alerts = [], [], []
    for n, rule in enumerate(cfg["cmdb_model"]["rules"]):
        labs = rule_labels(rule)
        where = {k: pick_where(v) for k, v in (rule.get("where") or {}).items()}
        for i in range(4):
            x = {lab: value(lab, i + n) for lab in labs}
            x.update(where)
            if i == 3 and labs:                      # una fila sin una etiqueta clave
                x.pop(next(iter(rule["cis"].values()))["key"][-1], None)
            x = {k: v for k, v in x.items() if v != ""}
            if rule.get("source") == "alerts":
                x.setdefault("alertname", where.get("alertname", f"Alerta{n}"))
                alerts.append({"labels": x, "annotations": {"summary": "prueba"},
                               "fingerprint": f"{n:04d}{i}", "startsAt": "2026-11-09T10:00:00Z",
                               "status": {"state": "active", "silencedBy": [], "inhibitedBy": []}})
                continue
            x["__name__"] = rule.get("metric") or metric_for(rule["metric_regex"])
            x.setdefault("job", "job-" + x["__name__"].split("_")[0])
            series.append(x)
            values.append({**x, "__value__": float(i % 2)})
    # Casos dirigidos: puente datastore -> LUN (por nombre y con desempate por capacidad)
    # y relaciones manuales con expansión por organismo y filtro de sede.
    gib = 2 ** 30
    extra = [
        ({"__name__": "unity_pool_size_total_bytes", "job": "unity", "instance": "s00-a-cah01.junta.es",
          "array": "UNITY-1-CICA", "name": "POOL_X"}, 500 * gib),
        ({"__name__": "unity_lun_size_total_bytes", "job": "unity", "instance": "s00-a-cah01.junta.es",
          "array": "UNITY-1-CICA", "name": "ORG1_CORE_01"}, 100 * gib),
        ({"__name__": "unity_lun_size_total_bytes", "job": "unity", "instance": "s00-a-cah01.junta.es",
          "array": "UNITY-1-CICA", "name": "ORG7_DATA_0A"}, 200 * gib),
        ({"__name__": "unity_lun_size_total_bytes", "job": "unity", "instance": "s00-a-cah01.junta.es",
          "array": "UNITY-1-CICA", "name": "ORG7_X_DATA_0A"}, 300 * gib),
        ({"__name__": "vsphere_datastore_capacity_bytes", "job": "vsphere", "vcenter": "vc04",
          "datastore": "S00CAH01_ORG1_X_VC04-CORE_01"}, 100 * gib),
        ({"__name__": "vsphere_datastore_capacity_bytes", "job": "vsphere", "vcenter": "vc04",
          "datastore": "S00CAH01_ORG7_Y_VC04-DATA_0A"}, 300 * gib),
        ({"__name__": "vsphere_datastore_capacity_bytes", "job": "vsphere", "vcenter": "vc04",
          "datastore": "S09CAH09_ORG1_X_VC04-CORE_01"}, 1 * gib),
        ({"__name__": "panos_vsys_info", "job": "panos_exporter", "instance": "10.47.208.11:9100",
          "display_name": "FW_CEFTA", "vsys": "vsys9"}, 1),
        ({"__name__": "f5_bigip_virtual_server_enabled_state", "job": "f5", "name": "s00-r-bac-01-t09-01",
          "partition": "SANDETEL_PRO", "virtual_server": "vs_emp", "organismo": "EMP_CICA", "site": "CICA"}, 1),
        ({"__name__": "f5_bigip_virtual_server_enabled_state", "job": "f5", "name": "s01-r-bac-01-t09-01",
          "partition": "SANDETEL_PRO", "virtual_server": "vs_emp", "organismo": "EMP_ZOCO", "site": "ZOCO"}, 1),
    ]
    for x, v in extra:
        series.append(x)
        values.append({**x, "__value__": float(v)})
    # Series de un job excluido y de métricas que ninguna regla usa
    series.append({"__name__": "vsphere_vm_power_state", "job": "acosv2", "vcenter": "vcx", "vm": "excluida", "host": "h"})
    series.append({"__name__": "node_cpu_seconds_total", "job": "node", "instance": "x:9100"})
    json.dump({"status": "success", "data": series}, open(out / "series.json", "w"), ensure_ascii=False)
    json.dump(values, open(out / "values.json", "w"), ensure_ascii=False)
    json.dump(alerts, open(out / "alerts.json", "w"), ensure_ascii=False)
    (out / "relaciones.txt").write_text(
        "# Relaciones manuales de prueba\n"
        "Vsys_PA:CEFTA => F5:EMP\n"
        "Vsys_PA:OTRO@CICA => F5:ADA\n"
        "Vsys_PA:NOEXISTE => F5:EMP\n"
        "línea mal formada\n", encoding="utf-8")
    print(f"{len(series)} series, {len(values)} valores, {len(alerts)} alertas -> {out}")


if __name__ == "__main__":
    main()
