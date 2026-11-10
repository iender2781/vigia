#!/usr/bin/env bash
# Comprueba que el motor en Go produce la misma CMDB que el correlador Python de referencia.
#
#   tools/reference/equivalence.sh                 captura sintética (cubre todas las reglas)
#   tools/reference/equivalence.sh DIR_CAPTURA     captura real (series.json [values.json] [alerts.json])
#
# Requiere python3 y go. Sale con código distinto de 0 si hay diferencias.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
REF="$ROOT/reference"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

CAPTURE=${1:-}
if [[ -z "$CAPTURE" ]]; then
  CAPTURE="$WORK/fixture"
  python3 "$ROOT/tools/reference/gen_fixture.py" "$REF/config_junta.json" "$CAPTURE"
fi

go build -o "$WORK/contexto" "$ROOT/cmd/contexto"
"$WORK/contexto" ontology import-legacy -config "$REF/config_junta.json" -out "$WORK/ont" >/dev/null
REL_ARGS=()
if [[ -f "$CAPTURE/relaciones.txt" ]]; then
  cp "$CAPTURE/relaciones.txt" "$WORK/ont/business/relaciones.txt"
  REL_ARGS=(--relations "$CAPTURE/relaciones.txt")
fi

run_case() {   # nombre, args extra de captura
  local name=$1; shift
  local pyargs=() goargs=()
  for f in "$@"; do
    case $f in
      values) pyargs+=(--values "$CAPTURE/values.json"); goargs+=(-values-file "$CAPTURE/values.json");;
      alerts) pyargs+=(--alerts "$CAPTURE/alerts.json"); goargs+=(-alerts-file "$CAPTURE/alerts.json");;
    esac
  done
  echo "== $name"
  python3 "$ROOT/tools/reference/py_cmdb.py" "$REF/correlador_alertas.py" "$REF/config_junta.json" \
    "$CAPTURE/series.json" ${pyargs[@]+"${pyargs[@]}"} ${REL_ARGS[@]+"${REL_ARGS[@]}"} --out "$WORK/py-$name" 2>&1 | grep -v '^Ficheros' || true
  "$WORK/contexto" replay -ontology "$WORK/ont" -no-exclude -series-file "$CAPTURE/series.json" \
    ${goargs[@]+"${goargs[@]}"} -export "$WORK/go-$name" 2>"$WORK/go-$name.log"
  head -1 "$WORK/go-$name.log"
  # Diferencias conocidas y deliberadas (mejoras del motor en Go):
  #  - Python no aplica value_attr en reglas con etiquetas calculadas (veeam_service_state).
  #  - build_cmdb de Python calcula el puente datastore -> LUN sin capacidades, así que no
  #    desempata LUN con nombres parecidos; el motor en Go usa los valores si los tiene.
  python3 "$ROOT/tools/reference/compare_cmdb.py" "$WORK/py-$name" "$WORK/go-$name" \
    --ignore-attr "Servicio Veeam:Estado" --allow-new-method "nombre del datastore"
}

FAIL=0
run_case series || FAIL=1
if [[ -f "$CAPTURE/values.json" ]]; then run_case series+values values || FAIL=1; fi
if [[ -f "$CAPTURE/alerts.json" ]]; then run_case completo values alerts || FAIL=1; fi
if [[ $FAIL == 0 ]]; then echo "OK: misma CMDB que el correlador Python"; else echo "HAY DIFERENCIAS"; fi
exit $FAIL
