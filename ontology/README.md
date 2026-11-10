# Ontología

Define qué existe (clases), cómo se relaciona (tipos de relación) y cómo se obtiene de Prometheus
(reglas). La plataforma la carga al arrancar y la imagen la incluye en `/etc/contexto/ontology`.
Se importó de `reference/config_junta.json` con `contexto ontology import-legacy` y se mantiene aquí.

```
core/settings.yaml      jobs excluidos, clases "mismo equipo", reescrituras de host, puente datastore→LUN,
                        atributo de Sistema de Información (ssii_attr) y atributos de búsqueda
core/classes.yaml       clases por tecnología (device, site_from_name, nav_parent)
core/relations.yaml     tipos de relación: causal (propaga impacto), informative, identity
core/sites.yaml         sedes y expresiones que las identifican
core/correlation.yaml   parámetros heredados para la correlación (fase 3)
packs/*.yaml            reglas por tecnología
business/               relaciones manuales (manual_relations.yaml y relaciones.txt)
```

## Una regla

```yaml
- id: vmware-vm-power-state          # único en toda la ontología
  order: 650                         # orden global (las reglas posteriores completan a las anteriores)
  metric: vmware_vm_power_state      # o metric_regex, o source: alerts
  where: {job: "vmware_.*"}          # filtros (regex completa) sobre etiquetas
  computed:                          # etiquetas calculadas, en orden
    - name: vcenter
      regex: {from: instance, pattern: '^([^.:]+)'}      # grupo 1, anclada al inicio
    - name: member
      format: '{node}:{port}'                            # si falta una etiqueta, no se calcula
    - name: fw_ip
      first: [exported_instance, instance]               # la primera no vacía (con pattern: su grupo 1)
      pattern: '^\[?([^\]:/]+)'
    - name: fw_site
      map: {from: fw_ip, values: {...}, patterns: [{pattern: '^10\.47\.208\.1[12]$', value: CICA}], default: Sin sede}
    - name: pair
      sorted: [fw_ip, peer_mgmt_ip]
      sep: ' / '
  cis:
    - alias: vm
      class: VM                      # debe existir en core/classes.yaml
      key: [vcenter, vm_name]        # clave natural: identidad estable del CI
      name: vm_name                  # por defecto, la última de key
      set_name: false                # true: esta regla fija el nombre aunque el CI ya exista
      attrs: {COD_SSII: cod_ssii}    # atributo visible -> etiqueta
      value_attr: Estado             # valor actual de la serie como atributo
      value_map: {"1": encendida, "0": apagada}
  rels:
    - {from: vm, rel: se ejecuta en, to: esx}   # "vm depende de esx"; el tipo debe existir en relations.yaml
```

Las expresiones regulares son **RE2** (Go): no admiten lookahead (`(?=...)`) ni referencias hacia atrás.

## Cómo cambiarla

1. Edita el YAML en una rama y valida: `go run ./cmd/contexto ontology summary -dir ontology`.
2. Prueba el efecto con una captura real: `go run ./cmd/contexto replay -ontology ontology -series-file CAPTURA/series.json -values-file CAPTURA/values.json -export /tmp/cmdb` y revisa `cmdb_ci.csv` y `cmdb_relaciones.csv`.
3. Abre un pull request: el CI valida la ontología y ejecuta los tests.
4. Para probar en un entorno sin publicar imagen: `ontology.overrideConfigMaps` del chart.

## Extensiones respecto a config_junta.json

- `vmware-vm-power-state`: COD_SSII (código texto-texto-número dentro de `tags`), Etiquetas y Estado de la VM.
