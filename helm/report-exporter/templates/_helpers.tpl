{{- define "reportExporter.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "reportExporter.clusterRoleName" -}}
{{- printf "%s-%s" (.Release.Namespace | trunc 30 | trimSuffix "-") (.Release.Name | trunc 32 | trimSuffix "-") -}}
{{- end -}}

{{- define "reportExporter.selectorLabels" -}}
app.kubernetes.io/name: nvcre-report-exporter
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "reportExporter.labels" -}}
{{ include "reportExporter.selectorLabels" . }}
app.kubernetes.io/component: report-exporter
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | quote }}
{{- end -}}
