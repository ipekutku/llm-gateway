{{/* The chart name, truncated to the 63 characters a label value allows. */}}
{{- define "llm-gateway.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* The name of the release's resources. */}}
{{- define "llm-gateway.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "llm-gateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "llm-gateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "llm-gateway.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "llm-gateway.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Labels of the migration Job and its pod. Its name differs from the gateway's,
so the Deployment's and Service's selectors never match the migration pod.
*/}}
{{- define "llm-gateway.migrationLabels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "llm-gateway.name" . | trunc 55 | trimSuffix "-" }}-migrate
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: migrate
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "llm-gateway.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "llm-gateway.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Checks the values that the schema cannot: required references, provider
combinations, the shutdown budget, and variables that env must not set. The
gateway checks its configuration again at startup.
*/}}
{{- define "llm-gateway.validate" -}}
{{- $v := .Values }}
{{- if not $v.database.urlSecret.name }}
{{- fail "database.urlSecret.name is required: the Secret holding GATEWAY_DATABASE_URL" }}
{{- end }}
{{- if not $v.clients.secretName }}
{{- fail "clients.secretName is required: the Secret holding the clients file" }}
{{- end }}
{{- $openai := $v.providers.openai }}
{{- $anthropic := $v.providers.anthropic }}
{{- if not (or $openai.model $anthropic.model) }}
{{- fail "no provider configured: set providers.openai.model or providers.anthropic.model" }}
{{- end }}
{{- range $name, $p := $v.providers }}
{{- if and $p.model (not $p.apiKeySecret.name) }}
{{- fail (printf "providers.%s.apiKeySecret.name is required when providers.%s.model is set" $name $name) }}
{{- end }}
{{- end }}
{{- if and $openai.fallback (not (and $openai.model $anthropic.model)) }}
{{- fail "providers.openai.fallback needs both providers configured" }}
{{- end }}
{{- if and $anthropic.fallback (not (and $openai.model $anthropic.model)) }}
{{- fail "providers.anthropic.fallback needs both providers configured" }}
{{- end }}
{{- with $v.autoscaling }}
{{- if .enabled }}
{{- if gt (int .minReplicas) (int .maxReplicas) }}
{{- fail "autoscaling.minReplicas must not be greater than autoscaling.maxReplicas" }}
{{- end }}
{{- if not (and $v.resources $v.resources.requests $v.resources.requests.cpu) }}
{{- fail "autoscaling needs resources.requests.cpu: utilization is measured against it" }}
{{- end }}
{{- end }}
{{- end }}
{{- $names := list "gateway" }}
{{- range $v.sidecars }}
{{- if has .name $names }}
{{- fail (printf "sidecars: the container name %s is already used" .name) }}
{{- end }}
{{- $names = append $names .name }}
{{- end }}
{{- $s := $v.shutdown }}
{{- if lt (int $s.terminationGracePeriodSeconds) (add $s.delaySeconds $s.timeoutSeconds 10 | int) }}
{{- fail (printf "shutdown.terminationGracePeriodSeconds must be at least delaySeconds + timeoutSeconds + 10 (%d), or the final usage records and spans can be lost" (add $s.delaySeconds $s.timeoutSeconds 10 | int)) }}
{{- end }}
{{- $reserved := list "GATEWAY_ADDR" "GATEWAY_METRICS_ADDR" "GATEWAY_LOG_FORMAT" "GATEWAY_CLIENTS_FILE" "GATEWAY_PRICING_FILE" "GATEWAY_DATABASE_URL" "GATEWAY_REDIS_URL" "GATEWAY_SHUTDOWN_DELAY" "GATEWAY_SHUTDOWN_TIMEOUT" }}
{{- range $prefix := list "OPENAI" "ANTHROPIC" }}
{{- range $suffix := list "MODEL" "API_KEY" "BASE_URL" "FALLBACK" }}
{{- $reserved = append $reserved (printf "%s_%s" $prefix $suffix) }}
{{- end }}
{{- end }}
{{- range $v.env }}
{{- if has .name $reserved }}
{{- fail (printf "env must not set %s: the chart sets it from its own values" .name) }}
{{- end }}
{{- end }}
{{- end }}
