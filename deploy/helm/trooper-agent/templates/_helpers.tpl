{{- define "trooper-agent.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "trooper-agent.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{/* selector labels; call with (dict "ctx" $ "component" "host") */}}
{{- define "trooper-agent.selector" -}}
app.kubernetes.io/name: {{ .ctx.Chart.Name }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "trooper-agent.secretName" -}}
{{- default (include "trooper-agent.fullname" .) .Values.existingSecret -}}
{{- end -}}

{{- define "trooper-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "trooper-agent.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "trooper-agent.image" -}}
{{ .Values.image.repository }}:{{ default .Chart.AppVersion .Values.image.tag }}
{{- end -}}

{{/* OTLP base endpoint for OBI: ebpf.endpoint, else scheme://host of the DSN. */}}
{{- define "trooper-agent.ingestEndpoint" -}}
{{- if .Values.ebpf.endpoint -}}
{{- .Values.ebpf.endpoint | trimSuffix "/" -}}
{{- else if .Values.dsn -}}
{{- $u := urlParse .Values.dsn -}}
{{- printf "%s://%s" $u.scheme $u.host -}}
{{- else -}}
{{- fail "ebpf.enabled with existingSecret needs ebpf.endpoint (e.g. https://ingest.tasktrooper.io)" -}}
{{- end -}}
{{- end -}}

{{/* Env shared by both agent workloads. */}}
{{- define "trooper-agent.commonEnv" -}}
- name: TT_DSN
  valueFrom: { secretKeyRef: { name: {{ include "trooper-agent.secretName" . }}, key: dsn } }
- name: TT_K8S_CLUSTER
  value: {{ .Values.clusterName | quote }}
- name: TT_LOG_LEVEL
  value: {{ .Values.logLevel | quote }}
{{- with .Values.cloud.provider }}
- name: TT_CLOUD_PROVIDER
  value: {{ . | quote }}
{{- end }}
{{- with .Values.cloud.region }}
- name: TT_CLOUD_REGION
  value: {{ . | quote }}
{{- end }}
{{- end -}}

{{- define "trooper-agent.validate" -}}
{{- if not .Values.clusterName -}}
{{- fail "clusterName is required (k8s.cluster.name)" -}}
{{- end -}}
{{- if and (not .Values.dsn) (not .Values.existingSecret) -}}
{{- fail "set dsn (https://<ingest key>@<host>/<project>) or existingSecret" -}}
{{- end -}}
{{- if and .Values.db.enabled (not .Values.db.apiUrl) -}}
{{- fail "db.enabled needs db.apiUrl (+ db.apiKey)" -}}
{{- end -}}
{{- end -}}
