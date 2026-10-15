{{/* Common labels and names for PriGossip-Hazard. */}}
{{- define "prigossip.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prigossip.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "prigossip.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "prigossip.labels" -}}
app.kubernetes.io/name: {{ include "prigossip.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: prigossip-hazard
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "prigossip.selectorLabels" -}}
app.kubernetes.io/name: {{ include "prigossip.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
{{- end -}}

{{- define "prigossip.serviceAccountName" -}}
{{- printf "%s-agent" (include "prigossip.fullname" .) -}}
{{- end -}}

{{- define "prigossip.brokerUrl" -}}
{{- printf "tcp://%s-mqtt.%s.svc.cluster.local:%v" .Release.Name .Release.Namespace .Values.mqtt.servicePort -}}
{{- end -}}
