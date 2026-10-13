{{- define "aisa.name" -}}
{{- .Chart.Name }}
{{- end }}

{{- /* The proxy's chart derives aisa's Service name the same way, from the same values
       (proxies/apisix/chart, "proxy.aisaURL"); keep the two in step. */}}
{{- define "aisa.fullname" -}}
{{- if .Values.global.aisa.fullnameOverride }}
{{- .Values.global.aisa.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "aisa.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "aisa.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "aisa.serviceAccountName" -}}
{{- default (include "aisa.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{- define "aisa.selectorLabels" -}}
app.kubernetes.io/name: {{ include "aisa.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: control
{{- end }}

{{- define "aisa.commonLabels" -}}
app.kubernetes.io/part-of: aisa
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "aisa.labels" -}}
{{ include "aisa.selectorLabels" . }}
{{ include "aisa.commonLabels" . }}
{{- end }}

{{- define "aisa.redis.selectorLabels" -}}
app.kubernetes.io/name: redis
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: counters
{{- end }}

{{- define "aisa.redis.labels" -}}
{{ include "aisa.redis.selectorLabels" . }}
{{ include "aisa.commonLabels" . }}
{{- end }}

{{- define "aisa.redisAddr" -}}
{{- if .Values.redis.enabled }}
{{- printf "%s-redis:6379" (include "aisa.fullname" .) }}
{{- else }}
{{- required "redis.addr is required when redis.enabled is false and quotas.enabled is true" .Values.redis.addr }}
{{- end }}
{{- end }}

{{- define "aisa.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{- define "aisa.vaultAddr" -}}
{{- required "global.vault.addr is required: the address of Vault" .Values.global.vault.addr }}
{{- end }}

{{- /* The proxy's URL that applications call, for their Secrets and the Model objects: the
       proxy chart's client Service in this release's namespace (proxies/apisix/chart,
       "proxy.fullname"), or kubernetes.proxyURL, which is required without that chart. */}}
{{- define "aisa.proxyURL" -}}
{{- if .Values.kubernetes.proxyURL }}
{{- .Values.kubernetes.proxyURL }}
{{- else if not .Values.apisix.enabled }}
{{- fail "kubernetes.proxyURL is required with apisix.enabled=false: the URL of the proxy that applications call" }}
{{- else }}
{{- /* The parent sees only the values given here, not the proxy chart's defaults. */}}
{{- $proxy := .Values.apisix | default dict }}
{{- $name := dig "fullnameOverride" "" $proxy | default (printf "%s-%s" .Release.Name (dig "nameOverride" "" $proxy | default "apisix")) | trunc 63 | trimSuffix "-" }}
{{- $port := dig "service" "port" 80 $proxy | int }}
{{- if eq $port 80 }}
{{- printf "http://%s.%s.svc.cluster.local/v1" $name .Release.Namespace }}
{{- else }}
{{- printf "http://%s.%s.svc.cluster.local:%d/v1" $name .Release.Namespace $port }}
{{- end }}
{{- end }}
{{- end }}
