{{/* ----- names ----- */}}

{{- define "netcat.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "netcat.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "netcat.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "netcat.agentName"      -}}{{ include "netcat.fullname" . }}-agent{{- end -}}
{{- define "netcat.controllerName" -}}{{ include "netcat.fullname" . }}-controller{{- end -}}

{{- define "netcat.namespace" -}}
{{- .Release.Namespace -}}
{{- end -}}

{{/* ----- labels ----- */}}

{{- define "netcat.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "netcat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{- define "netcat.agentSelectorLabels" -}}
app.kubernetes.io/name: {{ include "netcat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
app: {{ include "netcat.agentName" . }}
{{- end -}}

{{- define "netcat.controllerSelectorLabels" -}}
app.kubernetes.io/name: {{ include "netcat.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: controller
app: {{ include "netcat.controllerName" . }}
{{- end -}}

{{/* ----- images ----- */}}

{{- define "netcat.agentImage" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.agentRepository $tag -}}
{{- end -}}

{{- define "netcat.controllerImage" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.controllerRepository $tag -}}
{{- end -}}

