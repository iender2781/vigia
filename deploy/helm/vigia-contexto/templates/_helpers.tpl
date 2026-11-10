{{- define "contexto.name" -}}vigia-contexto{{- end -}}

{{- define "contexto.fullname" -}}
{{- if eq .Release.Name "vigia-contexto" -}}vigia-contexto{{- else -}}{{ .Release.Name }}-vigia-contexto{{- end -}}
{{- end -}}

{{- define "contexto.labels" -}}
app.kubernetes.io/name: {{ include "contexto.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: vigia
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{- define "contexto.selector" -}}
app.kubernetes.io/name: {{ include "contexto.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "contexto.image" -}}
{{ .Values.image.repository }}:{{ default .Chart.AppVersion .Values.image.tag }}
{{- end -}}

{{- define "contexto.serviceAccount" -}}
{{- if .Values.serviceAccount.create -}}{{ default (include "contexto.fullname" .) .Values.serviceAccount.name }}{{- else -}}{{ default "default" .Values.serviceAccount.name }}{{- end -}}
{{- end -}}

{{/* Variables de entorno comunes a todos los roles. */}}
{{- define "contexto.env" -}}
- name: CONTEXTO_DATABASE_URL
  valueFrom: {secretKeyRef: {name: {{ .Values.existingSecret }}, key: database-url}}
- name: CONTEXTO_API_TOKENS
  valueFrom: {secretKeyRef: {name: {{ .Values.existingSecret }}, key: api-tokens, optional: true}}
- name: CONTEXTO_PROMETHEUS_BEARER_TOKEN
  valueFrom: {secretKeyRef: {name: {{ .Values.existingSecret }}, key: prometheus-bearer-token, optional: true}}
- name: CONTEXTO_PROMETHEUS_BASIC_AUTH
  valueFrom: {secretKeyRef: {name: {{ .Values.existingSecret }}, key: prometheus-basic-auth, optional: true}}
{{- end -}}

{{- define "contexto.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities: {drop: [ALL]}
seccompProfile: {type: RuntimeDefault}
{{- end -}}

{{- define "contexto.ontologyVolumes" -}}
{{- range $dir, $cm := .Values.ontology.overrideConfigMaps }}
- name: ontology-{{ $dir }}
  configMap: {name: {{ $cm }}}
{{- end }}
{{- end -}}

{{- define "contexto.ontologyMounts" -}}
{{- range $dir, $cm := .Values.ontology.overrideConfigMaps }}
- name: ontology-{{ $dir }}
  mountPath: /etc/contexto/ontology/{{ $dir }}
  readOnly: true
{{- end }}
{{- end -}}
