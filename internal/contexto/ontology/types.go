// Package ontology carga y valida la ontología de la plataforma: clases, tipos de
// relación, sedes, ajustes y las reglas que convierten series de Prometheus en CI.
//
// La ontología vive en ficheros YAML versionados en git (directorio ontology/):
//
//	core/settings.yaml      ajustes de descubrimiento (jobs excluidos, mismo equipo, puente datastore-LUN)
//	core/classes.yaml       clases de CI agrupadas por tecnología
//	core/relations.yaml     tipos de relación y si propagan impacto
//	core/sites.yaml         sedes y cómo deducirlas
//	core/correlation.yaml   parámetros de correlación (se usan a partir de la fase 3)
//	packs/*.yaml            reglas de mapeo por tecnología
//	business/*.yaml         relaciones manuales y datos de negocio
package ontology

// Ontology es la ontología completa ya cargada y validada.
type Ontology struct {
	Dir         string
	Settings    Settings
	Classes     []Class
	Relations   []RelationType
	Sites       Sites
	Correlation map[string]any
	Packs       []Pack
	Manual      ManualRelations

	// Derivados (los rellena Index).
	Rules      []*Rule
	classByName map[string]*Class
	relByName   map[string]*RelationType
}

// Settings son los ajustes globales de descubrimiento (core/settings.yaml).
type Settings struct {
	// Jobs de Prometheus que no forman parte del proyecto: se ignoran sus series y alertas.
	ExcludeJobs []string `yaml:"exclude_jobs"`
	// Clases cuyos CI con el mismo nombre normalizado son el mismo equipo visto por varios exporters.
	SameHostClasses []string `yaml:"same_host_classes"`
	// Reescrituras del nombre de host antes de compararlo (p. ej. quitar prefijos de entorno).
	HostRewrite []Rewrite `yaml:"host_rewrite"`
	// Puente datastore (vSphere) -> LUN (cabina) por convención de nombres.
	DatastoreLunBridge *Bridge `yaml:"datastore_lun_bridge"`
	DatastoreClass     string  `yaml:"datastore_class"`
	LunClass           string  `yaml:"lun_class"`
	SizeMatchTolerance float64 `yaml:"size_match_tolerance"`
	// Atributo que identifica el Sistema de Información de un CI.
	SSIIAttr string `yaml:"ssii_attr"`
	// Atributos por los que también se busca un CI (además del nombre).
	SearchAttrs []string `yaml:"search_attrs"`
}

// Rewrite es una sustitución por expresión regular (sintaxis RE2 de Go).
type Rewrite struct {
	Pattern string `yaml:"pattern"`
	Replace string `yaml:"replace"`
}

// Bridge configura el enlace datastore -> LUN por nombre (y capacidad).
type Bridge struct {
	DatastoreMetric     string         `yaml:"datastore_metric"`
	DatastoreRegex      string         `yaml:"datastore_regex"`
	DatastoreSizeMetric string         `yaml:"datastore_size_metric"`
	LunSources          []BridgeSource `yaml:"lun_sources"`
}

// BridgeSource es una fuente de LUN para el puente.
type BridgeSource struct {
	Metric     string `yaml:"metric"`
	Label      string `yaml:"label"`
	ArrayLabel string `yaml:"array_label"`
	CodeMetric string `yaml:"code_metric"`
	CodeFrom   string `yaml:"code_from"`
	CodeRegex  string `yaml:"code_regex"`
	SizeMetric string `yaml:"size_metric"`
}

// Class es una clase de CI.
type Class struct {
	Name       string `yaml:"name"`
	Technology string `yaml:"technology"`
	// Device: es el "equipo" principal de su tecnología (cabina, chasis, vCenter...).
	Device bool `yaml:"device,omitempty"`
	// SiteFromName: la sede se deduce de su nombre y atributos.
	SiteFromName bool `yaml:"site_from_name,omitempty"`
	// NavParent: clase del padre lógico que distingue nombres repetidos en las vistas.
	NavParent   string `yaml:"nav_parent,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// RelationType describe un tipo de relación.
type RelationType struct {
	Name string `yaml:"name"`
	// Causal: si el destino falla, el origen se ve afectado (propaga impacto).
	Causal bool `yaml:"causal"`
	// Informative: se muestra pero no se usa para correlar ni recorrer dependencias.
	Informative bool `yaml:"informative,omitempty"`
	// Identity: une dos CI que son el mismo equipo visto desde tecnologías distintas.
	Identity    bool   `yaml:"identity,omitempty"`
	Description string `yaml:"description,omitempty"`
}

// Sites define las sedes y cómo deducirlas.
type Sites struct {
	// Código corto -> nombre de sede (S00 -> CICA).
	Names map[string]string `yaml:"names"`
	// Sede -> expresión regular que la identifica en nombres y atributos.
	Patterns []SitePattern `yaml:"patterns"`
	// Atributos que no se miran para deducir la sede (p. ej. "Réplica con").
	IgnoreAttrs []string `yaml:"ignore_attrs"`
}

// SitePattern asocia una sede a una expresión regular.
type SitePattern struct {
	Site    string `yaml:"site"`
	Pattern string `yaml:"pattern"`
}

// Pack es un fichero de reglas de una tecnología (packs/*.yaml).
type Pack struct {
	File        string  `yaml:"-"`
	Name        string  `yaml:"pack"`
	Technology  string  `yaml:"technology"`
	Description string  `yaml:"description,omitempty"`
	Rules       []*Rule `yaml:"rules"`
}

// Rule convierte series (o alertas) en CI y relaciones.
type Rule struct {
	ID string `yaml:"id"`
	// Order fija el orden global de aplicación (las reglas posteriores pueden completar o
	// renombrar CI creados por las anteriores). Sin Order, van al final en orden de fichero.
	Order int `yaml:"order,omitempty"`
	// Una de: Metric, MetricRegex o Source: alerts.
	Metric      string            `yaml:"metric,omitempty"`
	MetricRegex string            `yaml:"metric_regex,omitempty"`
	Source      string            `yaml:"source,omitempty"`
	Where       map[string]string `yaml:"where,omitempty"`
	Computed    []Computed        `yaml:"computed,omitempty"`
	CIs         []CISpec          `yaml:"cis"`
	Rels        []RelSpec         `yaml:"rels,omitempty"`
	// Attach: alias del CI al que se cuelga cada alerta (por defecto, el primero).
	Attach string `yaml:"attach,omitempty"`
	// Method: cómo se obtuvo la relación (se muestra en la web). Por defecto "métricas".
	Method      string `yaml:"method,omitempty"`
	Description string `yaml:"description,omitempty"`

	Pack string `yaml:"-"`
}

// Computed calcula una etiqueta nueva a partir de otras. Se evalúan en orden.
// Exactamente una de las formas: Regex, Format, First, Map o Sorted.
type Computed struct {
	Name string `yaml:"name"`
	// regex: grupo 1 de Pattern aplicado (anclado al inicio) a la etiqueta From.
	Regex *RegexFrom `yaml:"regex,omitempty"`
	// format: "{node}:{port}" (si falta alguna etiqueta, no se calcula).
	Format *string `yaml:"format,omitempty"`
	// first: primera etiqueta no vacía de la lista; con Pattern, su grupo 1.
	First   []string `yaml:"first,omitempty"`
	Pattern string   `yaml:"pattern,omitempty"`
	// map: valor de From traducido por tabla o por expresiones (la primera que encaje).
	Map *MapFrom `yaml:"map,omitempty"`
	// sorted: valores de las etiquetas ordenados y unidos con Sep.
	Sorted []string `yaml:"sorted,omitempty"`
	Sep    string   `yaml:"sep,omitempty"`
}

// RegexFrom extrae el grupo 1 de una expresión aplicada a una etiqueta.
type RegexFrom struct {
	From    string `yaml:"from"`
	Pattern string `yaml:"pattern"`
}

// MapFrom traduce el valor de una etiqueta.
type MapFrom struct {
	From     string            `yaml:"from"`
	Values   map[string]string `yaml:"values,omitempty"`
	Patterns []PatternValue    `yaml:"patterns,omitempty"`
	Default  string            `yaml:"default,omitempty"`
}

// PatternValue: si Pattern encaja (búsqueda no anclada), el resultado es Value.
type PatternValue struct {
	Pattern string `yaml:"pattern"`
	Value   string `yaml:"value"`
}

// CISpec define un CI que crea o completa una regla.
type CISpec struct {
	Alias string   `yaml:"alias"`
	Class string   `yaml:"class"`
	Key   []string `yaml:"key"`
	// Name: etiqueta con el nombre visible (por defecto, la última de Key).
	Name string `yaml:"name,omitempty"`
	// SetName: esta regla fija el nombre aunque el CI ya exista.
	SetName bool `yaml:"set_name,omitempty"`
	// Attrs: atributo visible -> etiqueta.
	Attrs map[string]string `yaml:"attrs,omitempty"`
	// ValueAttr: atributo que toma el valor actual de la serie (traducido con ValueMap).
	ValueAttr string            `yaml:"value_attr,omitempty"`
	ValueMap  map[string]string `yaml:"value_map,omitempty"`
}

// NameLabel devuelve la etiqueta que da nombre al CI.
func (c CISpec) NameLabel() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Key[len(c.Key)-1]
}

// RelSpec es una relación From -> To ("From depende de To").
type RelSpec struct {
	From  string            `yaml:"from"`
	Rel   string            `yaml:"rel"`
	To    string            `yaml:"to"`
	Attrs map[string]string `yaml:"attrs,omitempty"`
}

// ManualRelations son las relaciones que Prometheus no ve (business/manual_relations.yaml).
type ManualRelations struct {
	Relation string                    `yaml:"relation"`
	Prefixes map[string]ManualPrefix   `yaml:"prefixes"`
	Files    []string                  `yaml:"files,omitempty"`
	Entries  []string                  `yaml:"entries,omitempty"`
	File     string                    `yaml:"-"`
}

// ManualPrefix traduce un prefijo (Vsys_PA, F5...) a clases de CI.
type ManualPrefix struct {
	Classes      []string `yaml:"classes"`
	NamePrefixes []string `yaml:"name_prefixes,omitempty"`
	Expand       []string `yaml:"expand,omitempty"`
}
