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

{{/* The image tag: image.tag, by default the chart's appVersion. */}}
{{- define "cadre.tag" -}}
{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "cadre.image" -}}
{{ .root.Values.image.registry }}/{{ .name }}:{{ include "cadre.tag" .root }}
{{- end -}}

{{/* Where runners reach cadre-server: the Service, unless set. */}}
{{- define "cadre.publicURL" -}}
{{- if .Values.server.publicURL -}}
{{ .Values.server.publicURL }}
{{- else -}}
http://{{ include "cadre.server" . }}.{{ .Release.Namespace }}.svc:{{ .Values.server.service.port }}
{{- end -}}
{{- end -}}

{{/* Where the chart's runner reaches it: runner.serverURL, or publicURL. */}}
{{- define "cadre.runnerServerURL" -}}
{{ .Values.runner.serverURL | default (include "cadre.publicURL" .) }}
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

{{/* image.pullPolicy, but IfNotPresent becomes Always for a moving tag
     (main, latest), so that an upgrade pulls it; Never stays Never. */}}
{{- define "cadre.pullPolicy" -}}
{{- if and (eq .Values.image.pullPolicy "IfNotPresent") (has (include "cadre.tag" .) (list "main" "latest")) -}}
Always
{{- else -}}
{{ .Values.image.pullPolicy }}
{{- end -}}
{{- end -}}

{{/* The session key, base64 as in the Secret: the existing one's, else, for
     the chart's own Secret, a new one, made once per render and kept in
     .Values so that the Secret and the server's checksum agree. Empty for
     an existingSecret the chart cannot read (helm template). */}}
{{- define "cadre.sessionKey" -}}
{{- if not (hasKey .Values.auth.sessionKey "_rendered") -}}
{{- $key := "" -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "cadre.sessionKeySecret" .) -}}
{{- if and $existing $existing.data (index $existing.data "key") -}}
{{- $key = index $existing.data "key" -}}
{{- else if not .Values.auth.sessionKey.existingSecret -}}
{{- /* 64 hex characters: a 32-byte key. */ -}}
{{- $key = randAlphaNum 64 | sha256sum | b64enc -}}
{{- end -}}
{{- $_ := set .Values.auth.sessionKey "_rendered" $key -}}
{{- end -}}
{{ .Values.auth.sessionKey._rendered }}
{{- end -}}
