{{- define "ant.name" -}}
{{- printf "%s-ant" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "ant.labels" -}}
app.kubernetes.io/name: kuvryn-ant
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- end -}}
{{- define "ant.selector" -}}
app.kubernetes.io/name: kuvryn-ant
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}
{{- define "ant.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
{{- end -}}
{{- define "ant.validate" -}}
{{- if ne (int .Values.replicaCount) 1 }}{{ fail "replicaCount must be 1: this release requires a single writer" }}{{ end -}}
{{- if not (has .Values.storage.backend (list "disk" "s3")) }}{{ fail "storage.backend must be disk or s3" }}{{ end -}}
{{- if not (regexMatch "^https://[^/?#@]+$" .Values.publicURL) }}{{ fail "publicURL must be a trusted HTTPS origin without a path" }}{{ end -}}
{{- if or (lt (int .Values.shutdownSeconds) 1) (gt (int .Values.shutdownSeconds) 60) }}{{ fail "shutdownSeconds must be 1..60" }}{{ end -}}
{{- if lt (int .Values.terminationGracePeriodSeconds) (add (int .Values.shutdownSeconds) 10) }}{{ fail "terminationGracePeriodSeconds must allow shutdownSeconds plus 10 seconds" }}{{ end -}}
{{- if .Values.ingress.enabled -}}
{{- if ne .Values.publicURL (printf "https://%s" .Values.ingress.host) }}{{ fail "ingress.host must exactly match publicURL" }}{{ end -}}
{{- if not .Values.ingress.tlsSecret }}{{ fail "ingress.tlsSecret is required for HTTPS ingress" }}{{ end -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- if not .Values.httpRoute.parentRefs }}{{ fail "httpRoute.parentRefs is required" }}{{ end -}}
{{- if not (has (trimPrefix "https://" .Values.publicURL) .Values.httpRoute.hostnames) }}{{ fail "httpRoute.hostnames must include the publicURL host" }}{{ end -}}
{{- end -}}
{{- if eq .Values.storage.backend "s3" -}}
{{- if or (not .Values.storage.s3.region) (not .Values.storage.s3.bucket) (not .Values.storage.s3.credentialsExistingSecret) }}{{ fail "S3 requires region, bucket and credentialsExistingSecret" }}{{ end -}}
{{- if and .Values.storage.s3.endpoint (not (hasPrefix "https://" .Values.storage.s3.endpoint)) (not (and .Values.storage.s3.allowInsecureHTTP (hasPrefix "http://" .Values.storage.s3.endpoint))) }}{{ fail "S3 endpoint must use HTTPS unless storage.s3.allowInsecureHTTP is true for an http:// endpoint" }}{{ end -}}
{{- end -}}
{{- end -}}
