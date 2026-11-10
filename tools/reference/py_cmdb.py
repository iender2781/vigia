#!/usr/bin/env python3
"""Ejecuta build_cmdb del correlador Python de referencia sobre una captura y escribe los CSV.

Uso: py_cmdb.py CORRELADOR CONFIG SERIES [--values F] [--alerts F] [--relations F] --out DIR
"""
import argparse
import importlib.util
import json
import sys


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("correlador")
    ap.add_argument("config")
    ap.add_argument("series")
    ap.add_argument("--values")
    ap.add_argument("--alerts")
    ap.add_argument("--relations")
    ap.add_argument("--exclude-jobs", action="store_true", help="aplicar exclude_jobs como en build()")
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    spec = importlib.util.spec_from_file_location("correlador", a.correlador)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["correlador"] = mod
    spec.loader.exec_module(mod)
    cfg = mod.load_config(a.config)
    cfg.setdefault("relations_file", {})["files"] = [a.relations] if a.relations else []
    series = mod.normalize_series(json.load(open(a.series, encoding="utf-8")))
    values = mod.normalize_values(json.load(open(a.values, encoding="utf-8"))) if a.values else None
    alerts = None
    if a.alerts:
        alerts = [dict(x["labels"]) for x in mod.normalize_alerts(json.load(open(a.alerts, encoding="utf-8")))]
    if a.exclude_jobs:
        ex = set(cfg.get("exclude_jobs") or ())
        series = [x for x in series if x.get("job") not in ex]
        values = [v for v in values or () if v.get("job") not in ex] or values
        alerts = [x for x in alerts or () if x.get("job") not in ex] or alerts
    cmdb = mod.build_cmdb(series, cfg, alerts, values)
    mod.write_cmdb(cmdb, a.out, {"source": a.series})
    print(f"Python: {len(cmdb['cis'])} elementos, {len(cmdb['rels'])} relaciones", file=sys.stderr)


if __name__ == "__main__":
    main()
