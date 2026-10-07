{{- define "proxy.name" -}}
{{- default "apisix" .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "proxy.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "proxy.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "proxy.serviceAccountName" -}}
{{- default (include "proxy.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{- define "proxy.selectorLabels" -}}
app.kubernetes.io/name: {{ include "proxy.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "proxy.labels" -}}
{{ include "proxy.selectorLabels" . }}
app.kubernetes.io/part-of: aisa
app.kubernetes.io/component: proxy
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- /* aisa's URL: the value, or the Service of aisa's chart in the same release, named as that
       chart names it (deploy/helm/aisa, "aisa.fullname"). */}}
{{- define "proxy.aisaURL" -}}
{{- if .Values.aisa.url }}
{{- .Values.aisa.url | trimSuffix "/" }}
{{- else if .Values.global.aisa.fullnameOverride }}
{{- printf "http://%s:8080" (.Values.global.aisa.fullnameOverride | trunc 63 | trimSuffix "-") }}
{{- else }}
{{- $name := .Release.Name }}
{{- if not (contains "aisa" .Release.Name) }}{{ $name = printf "%s-aisa" .Release.Name }}{{ end }}
{{- printf "http://%s:8080" ($name | trunc 63 | trimSuffix "-") }}
{{- end }}
{{- end }}

{{- define "proxy.env" -}}
- name: VAULT_ADDR
  value: {{ required "global.vault.addr is required: the address of Vault" .Values.global.vault.addr | quote }}
- name: CONSUL_HTTP_ADDR
  value: {{ required "global.consul.addr is required: the address of Consul" .Values.global.consul.addr | quote }}
- name: AISA_DECIDE_URI
  value: {{ printf "%s/v1/decide" (include "proxy.aisaURL" .) | quote }}
- name: AISA_USAGE_URI
  value: {{ printf "%s/v1/usage" (include "proxy.aisaURL" .) | quote }}
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "proxy.renderArgs" -}}
- -config=/etc/apisix/aisa/consul-template.hcl
- -vault-agent-token-file=/vault/token
{{- if .Values.vaultAgent.consulToken.enabled }}
- -consul-token-file=/vault/consul-token
{{- end }}
{{- end }}

{{- define "proxy.renderMounts" -}}
- {name: render, mountPath: /etc/apisix/aisa, readOnly: true}
- {name: tokens, mountPath: /vault, readOnly: true}
- {name: rendered, mountPath: /rendered}
{{- with .Values.extraVolumeMounts }}
{{ toYaml . }}
{{- end }}
{{- end }}
