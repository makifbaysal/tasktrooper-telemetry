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

{{- define "trooper-agent.selector" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
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

{{/* Static config: integrations + targets. */}}
{{- define "trooper-agent.config" -}}
{{- $dbs := list -}}
{{- range $engine := list "postgres" "mysql" "redis" "mongodb" -}}
{{- $i := index $.Values.integrations $engine -}}
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
{{- $dbs = concat $dbs (default (list) .Values.targets.databases) -}}
{{- $cfg := dict "interval" (default "15s" .Values.targets.interval) "timeout" (default "10s" .Values.targets.timeout) "databases" $dbs "brokerClusters" (default (list) .Values.targets.brokerClusters) -}}
{{- toYaml $cfg -}}
{{- end -}}

{{- define "trooper-agent.hasStatic" -}}
{{- $n := len (default (list) .Values.targets.databases) | add (len (default (list) .Values.targets.brokerClusters)) -}}
{{- range $engine := list "postgres" "mysql" "redis" "mongodb" -}}{{- if (index $.Values.integrations $engine).enabled -}}{{- $n = add1 $n -}}{{- end -}}{{- end -}}
{{- if gt (int $n) 0 -}}true{{- end -}}
{{- end -}}
