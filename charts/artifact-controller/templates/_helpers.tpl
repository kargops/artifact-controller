{{/* Chart name, overridable by nameOverride. */}}
{{- define "artifact-controller.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Fully qualified release name. */}}
{{- define "artifact-controller.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains (include "artifact-controller.name" .) .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name (include "artifact-controller.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "artifact-controller.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "artifact-controller.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: devops
{{- end -}}

{{- define "artifact-controller.selectorLabels" -}}
app.kubernetes.io/name: {{ include "artifact-controller.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
ServiceAccount name. Kept stable rather than release-prefixed by default,
because cloud identity (EKS Pod Identity / IRSA) binds to this exact name.
*/}}
{{- define "artifact-controller.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "artifact-controller.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
ServiceAccount that generator runs use. Class templates reference it by name,
so it is stable rather than release-prefixed, for the same reason as the
controller's: cloud identity binds to the exact name.
*/}}
{{- define "artifact-controller.generatorServiceAccountName" -}}
{{- default (printf "%s-generator" (include "artifact-controller.fullname" .)) .Values.generator.serviceAccount.name -}}
{{- end -}}

{{- define "artifact-controller.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
