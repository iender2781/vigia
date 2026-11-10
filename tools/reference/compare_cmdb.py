#!/usr/bin/env python3
"""Compara dos exportaciones de CMDB (cmdb_ci.csv y cmdb_relaciones.csv) sin depender del
orden de filas ni de atributos. Sale con código 1 si hay diferencias.

Uso: compare_cmdb.py DIR_REFERENCIA DIR_NUEVO [--ignore-attr Clase:Atributo ...]
"""
import argparse
import csv
import sys
from pathlib import Path


def load_cis(d, ignore):
    out = {}
    with open(Path(d) / "cmdb_ci.csv", encoding="utf-8", newline="") as f:
        for r in csv.DictReader(f, delimiter=";"):
            attrs = sorted(a for a in r["atributos"].split(" | ") if a
                           and f"{r['tipo']}:{a.split('=', 1)[0]}" not in ignore)
            out[r["id"]] = (r["tipo"], r["nombre"], tuple(attrs), r["metricas"])
    return out


def load_rels(d):
    out = set()
    with open(Path(d) / "cmdb_relaciones.csv", encoding="utf-8", newline="") as f:
        for r in csv.DictReader(f, delimiter=";"):
            attrs = " | ".join(sorted(a for a in r["atributos"].split(" | ") if a))
            out.add((r["origen_tipo"], r["origen"], r["relacion"], r["destino_tipo"], r["destino"], r["metodo"], attrs))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("ref")
    ap.add_argument("new")
    ap.add_argument("--ignore-attr", action="append", default=[])
    ap.add_argument("--allow-new-method", action="append", default=[],
                    help="relaciones con este método que solo están en el nuevo: mejora conocida, no cuenta")
    a = ap.parse_args()
    ignore = set(a.ignore_attr)
    c1, c2 = load_cis(a.ref, ignore), load_cis(a.new, ignore)
    r1, r2 = load_rels(a.ref), load_rels(a.new)
    diffs = 0
    for k in sorted(set(c1) | set(c2)):
        if c1.get(k) != c2.get(k):
            diffs += 1
            if diffs <= 40:
                print(f"CI {k}\n  referencia: {c1.get(k)}\n  nuevo:      {c2.get(k)}")
    allowed = {r for r in r2 - r1 if r[5] in a.allow_new_method}
    for r in sorted(allowed):
        print("mejora conocida (solo en nuevo):", r)
    r2 = r2 - allowed
    for r in sorted(r1 - r2)[:40]:
        print("solo en referencia:", r)
    for r in sorted(r2 - r1)[:40]:
        print("solo en nuevo:     ", r)
    diffs += len(r1 ^ r2)
    print(f"CI: {len(c1)} / {len(c2)}   relaciones: {len(r1)} / {len(r2)}   diferencias: {diffs}")
    sys.exit(1 if diffs else 0)


if __name__ == "__main__":
    main()
