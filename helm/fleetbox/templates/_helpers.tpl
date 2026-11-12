{{- define "fleetbox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "fleetbox.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "fleetbox.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "fleetbox.labels" -}}
helm.sh/chart: {{ include "fleetbox.chart" . }}
{{ include "fleetbox.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "fleetbox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fleetbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "fleetbox.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "fleetbox.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The configuration file: the user's `config` with `store.path` fixed to the
mounted volume and `server.listen` defaulted to the container port.
*/}}
{{- define "fleetbox.config" -}}
{{- $cfg := deepCopy .Values.config }}
{{- if hasKey $cfg "store" }}
{{- fail "config.store is set by the chart; use persistence.* instead" }}
{{- end }}
{{- if not (and $cfg.auth $cfg.auth.oidc) }}
{{- fail "config.auth.oidc is required: fleetbox refuses to serve beyond loopback without authentication" }}
{{- end }}
{{- $server := default dict $cfg.server }}
{{- if not $server.publicURL }}
{{- fail "config.server.publicURL is required: the OIDC redirect URL is derived from it" }}
{{- end }}
{{- if not $server.listen }}
{{- $_ := set $server "listen" (printf ":%d" (int .Values.containerPort)) }}
{{- end }}
{{- $_ := set $cfg "server" $server }}
{{- $_ := set $cfg "store" (dict "path" "/var/lib/fleetbox/fleetbox.db") }}
{{- toYaml $cfg }}
{{- end }}
