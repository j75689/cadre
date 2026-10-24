{{/* The release's name, the prefix of every object the chart makes. */}}
{{- define "cadre.fullname" -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}

{{- define "cadre.server" -}}{{ include "cadre.fullname" . }}-server{{- end -}}
{{- define "cadre.runner" -}}{{ include "cadre.fullname" . }}-runner{{- end -}}

{{- define "cadre.labels" -}}
app.kubernetes.io/part-of: cadre
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{- define "cadre.image" -}}
{{ .root.Values.image.registry }}/{{ .name }}:{{ .root.Values.image.tag | default .root.Chart.AppVersion }}
{{- end -}}

{{/* Where runners reach cadre-server: the Service, unless set. */}}
{{- define "cadre.publicURL" -}}
{{- if .Values.server.publicURL -}}
{{ .Values.server.publicURL }}
{{- else -}}
http://{{ include "cadre.server" . }}.{{ .Release.Namespace }}.svc:{{ .Values.server.service.port }}
{{- end -}}
{{- end -}}

{{/* Where browsers reach it: set, or the Ingress host, or nothing. */}}
{{- define "cadre.externalURL" -}}
{{- if .Values.server.externalURL -}}
{{ .Values.server.externalURL }}
{{- else if and .Values.ingress.enabled .Values.ingress.host -}}
{{ ternary "https" "http" (ne .Values.ingress.tlsSecret "") }}://{{ .Values.ingress.host }}
{{- end -}}
{{- end -}}

{{- define "cadre.sessionKeySecret" -}}
{{ .Values.auth.sessionKey.existingSecret | default (printf "%s-session-key" (include "cadre.fullname" .)) }}
{{- end -}}

{{/* Rules of the first document of kind ClusterRole in a file. */}}
{{- define "cadre.rules" -}}
{{- range splitList "\n---\n" (.root.Files.Get .file) -}}
{{- $doc := fromYaml . -}}
{{- if eq (get $doc "kind") "ClusterRole" -}}
{{ toYaml (get $doc "rules") }}
{{- end -}}
{{- end -}}
{{- end -}}
