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
{{ if .Values.ingress.tlsSecret }}https{{ else }}http{{ end }}://{{ .Values.ingress.host }}
{{- end -}}
{{- end -}}

{{- define "cadre.sessionKeySecret" -}}
{{ .Values.auth.sessionKey.existingSecret | default (printf "%s-session-key" (include "cadre.fullname" .)) }}
{{- end -}}

{{/* Rules of the one document of kind ClusterRole in a file; a second
     fails, so that a file gaining a role cannot widen a grant unseen. */}}
{{- define "cadre.rules" -}}
{{- $roles := list -}}
{{- range splitList "\n---\n" (.root.Files.Get .file) -}}
{{- $doc := fromYaml . -}}
{{- if eq (get $doc "kind") "ClusterRole" -}}
{{- $roles = append $roles $doc -}}
{{- end -}}
{{- end -}}
{{- if ne (len $roles) 1 -}}
{{- fail (printf "%s has %d ClusterRoles, not one" .file (len $roles)) -}}
{{- end -}}
{{ toYaml (get (first $roles) "rules") }}
{{- end -}}

{{/* Always for a moving tag (main, latest), else image.pullPolicy. */}}
{{- define "cadre.pullPolicy" -}}
{{- $tag := .Values.image.tag | default .Chart.AppVersion -}}
{{- if has $tag (list "main" "latest") -}}Always{{- else -}}{{ .Values.image.pullPolicy }}{{- end -}}
{{- end -}}
