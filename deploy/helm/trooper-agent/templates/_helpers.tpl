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
{{- if and (include "trooper-agent.needsCluster" .) (not .Values.clusterName) -}}
{{- fail "clusterName is required (k8s.cluster.name) when host / cluster / windows / ebpf is enabled" -}}
{{- end -}}
{{- if and (not .Values.dsn) (not .Values.existingSecret) -}}
{{- fail "set dsn (https://<ingest key>@<host>/<project>) or existingSecret" -}}
{{- end -}}
{{- if .Values.db.enabled -}}
{{- if and (not (include "trooper-agent.hasStatic" .)) (not .Values.db.fromApi.apiUrl) -}}
{{- fail "db.enabled: nothing to collect — set db.targets, enable a db.integrations.*, or set db.fromApi.apiUrl (+ apiKey)" -}}
{{- end -}}
{{- if and .Values.db.fromApi.apiUrl (not .Values.db.fromApi.apiKey) (not .Values.existingSecret) -}}
{{- fail "db.fromApi.apiUrl needs db.fromApi.apiKey (or existingSecret with key apiKey)" -}}
{{- end -}}
{{- end -}}
{{- if not (or .Values.host.enabled .Values.cluster.enabled .Values.db.enabled .Values.windows.enabled .Values.ebpf.enabled) -}}
{{- fail "nothing enabled: enable host, cluster, db, windows or ebpf" -}}
{{- end -}}
{{- end -}}

{{/* Windows image: windows.image.repository (default image.repository) : windows.image.tag (default <appVersion>-windows). */}}
{{- define "trooper-agent.windowsImage" -}}
{{ default .Values.image.repository .Values.windows.image.repository }}:{{ default (printf "%s-windows" .Chart.AppVersion) .Values.windows.image.tag }}
{{- end -}}

{{/* nodeSelector: user values merged over the OS default (user wins); call with (dict "user" .Values.host.nodeSelector "os" "linux"). */}}
{{- define "trooper-agent.nodeSelector" -}}
{{- toYaml (merge (deepCopy (default (dict) .user)) (dict "kubernetes.io/os" .os)) -}}
{{- end -}}

{{/* TT_AGENT_MODES of the cluster Deployment. */}}
{{- define "trooper-agent.clusterModes" -}}
{{- $modes := list -}}
{{- if .Values.cluster.enabled }}{{ $modes = append $modes "cluster" }}{{ end -}}
{{- if .Values.db.enabled }}{{ $modes = append $modes "db" }}{{ end -}}
{{- join "," $modes -}}
{{- end -}}

{{/* Static db config (same format as services/collector/examples/agent.yaml): integrations + targets. */}}
{{- define "trooper-agent.config" -}}
{{- $dbs := list -}}
{{- range $engine := list "postgres" "mysql" "redis" "mongodb" -}}
{{- $i := index $.Values.db.integrations $engine -}}
{{- if $i.enabled -}}
{{- $hp := splitList ":" $i.address -}}
{{- $db := dict "id" (default $engine $i.name) "engine" $engine "host" (first $hp) "collector" "agent" -}}
{{- if gt (len $hp) 1 -}}{{- $_ := set $db "port" (last $hp | int) -}}{{- end -}}
{{- if $i.user -}}{{- $_ := set $db "user" $i.user -}}{{- end -}}
{{- if $i.database -}}{{- $_ := set $db "database" $i.database -}}{{- end -}}
{{- if $i.tls -}}{{- $_ := set $db "tls" $i.tls -}}{{- end -}}
{{- if $i.topQueriesSource -}}{{- $_ := set $db "topQueriesSource" $i.topQueriesSource -}}{{- end -}}
{{- if $i.passwordSecret.name -}}{{- $_ := set $db "credentialsRef" (printf "env:TT_%s_PASSWORD" (upper $engine)) -}}{{- end -}}
{{- $dbs = append $dbs $db -}}
{{- end -}}
{{- end -}}
{{- $dbs = concat $dbs (default (list) .Values.db.targets.databases) -}}
{{- $cfg := dict "interval" (default "15s" .Values.db.targets.interval) "timeout" (default "10s" .Values.db.targets.timeout) "databases" $dbs "brokerClusters" (default (list) .Values.db.targets.brokerClusters) -}}
{{- toYaml $cfg -}}
{{- end -}}

{{/* "true" when db.enabled and there is at least one static target / integration. */}}
{{- define "trooper-agent.hasStatic" -}}
{{- if .Values.db.enabled -}}
{{- $n := len (default (list) .Values.db.targets.databases) | add (len (default (list) .Values.db.targets.brokerClusters)) -}}
{{- range $engine := list "postgres" "mysql" "redis" "mongodb" -}}{{- if (index $.Values.db.integrations $engine).enabled -}}{{- $n = add1 $n -}}{{- end -}}{{- end -}}
{{- if gt (int $n) 0 -}}true{{- end -}}
{{- end -}}
{{- end -}}

{{- define "trooper-agent.needsCluster" -}}
{{- if or .Values.host.enabled .Values.cluster.enabled .Values.windows.enabled .Values.ebpf.enabled -}}true{{- end -}}
{{- end -}}
