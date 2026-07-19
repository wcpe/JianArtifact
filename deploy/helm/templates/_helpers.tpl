{{/* 通用命名与标签辅助模板 */}}
{{- define "jianartifact.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "jianartifact.fullname" -}}
{{- printf "%s" (include "jianartifact.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "jianartifact.labels" -}}
app.kubernetes.io/name: {{ include "jianartifact.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "jianartifact.selectorLabels" -}}
app.kubernetes.io/name: {{ include "jianartifact.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
