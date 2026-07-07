{{- define "runlore.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "runlore.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "runlore.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "runlore.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "runlore.selectorLabels" -}}
app.kubernetes.io/name: {{ include "runlore.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "runlore.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "runlore.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
The pod template (metadata + spec), shared verbatim between deployment.yaml and
statefulset.yaml so the two workload kinds never drift. Takes the root context (`.`)
directly. When workloadKind is StatefulSet with persistence enabled, the "catalog"
volume is provided by volumeClaimTemplates instead of a `persistentVolumeClaim` volume
entry here — the mount by name is identical either way.
*/}}
{{- define "runlore.podTemplate" -}}
{{- $usesVolumeClaimTemplates := and (eq .Values.workloadKind "StatefulSet") .Values.persistence.enabled -}}
metadata:
  annotations:
    checksum/config: {{ include (print .Template.BasePath "/configmap.yaml") . | sha256sum }}
    {{- with .Values.podAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
  labels:
    {{- include "runlore.selectorLabels" . | nindent 4 }}
spec:
  # Give the leader time to gracefully drain its in-flight investigation on
  # SIGTERM (record the outcome + deliver) before SIGKILL — must exceed the
  # agent's internal drain grace (25s). Raise both for longer investigations.
  terminationGracePeriodSeconds: 40
  serviceAccountName: {{ include "runlore.serviceAccountName" . }}
  {{- with .Values.imagePullSecrets }}
  imagePullSecrets:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  securityContext:
    {{- toYaml .Values.podSecurityContext | nindent 4 }}
  containers:
    - name: runlore
      image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      args:
        - serve
        - --config
        - /etc/runlore/runlore.yaml
        - --addr
        - ":{{ .Values.service.port }}"
      ports:
        - name: http
          containerPort: {{ .Values.service.port }}
      {{- with .Values.envFrom }}
      envFrom:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        {{- with .Values.env }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- /* nil-safe: tolerate a values override that nulls `probes` or a sub-map */}}
      {{- $probes := .Values.probes | default dict }}
      {{- $startup := $probes.startup | default dict }}
      {{- $liveness := $probes.liveness | default dict }}
      {{- $readiness := $probes.readiness | default dict }}
      {{- if $startup.enabled }}
      # startupProbe owns the cold-start window (catalog warm-up + leader-lease
      # gating). It targets /healthz (process-alive), NOT /readyz: a standby
      # replica is never the leader and so never /readyz-ready, which would
      # deadlock a readiness-gated startupProbe. While it runs, liveness +
      # readiness are suppressed, so warm-up emits no premature Unhealthy events.
      startupProbe:
        httpGet:
          path: /healthz
          port: http
        periodSeconds: {{ $startup.periodSeconds | default 2 }}
        failureThreshold: {{ $startup.failureThreshold | default 30 }}
        timeoutSeconds: {{ $startup.timeoutSeconds | default 2 }}
      {{- end }}
      livenessProbe:
        # Liveness is NOT loosened — a genuine process hang still trips it.
        httpGet:
          path: /healthz
          port: http
        periodSeconds: {{ $liveness.periodSeconds | default 10 }}
        failureThreshold: {{ $liveness.failureThreshold | default 3 }}
        timeoutSeconds: {{ $liveness.timeoutSeconds | default 2 }}
      readinessProbe:
        # /readyz is gated by leadership + catalog warmth — only the warm leader
        # serves webhook traffic. The startupProbe covers the cold window, so no
        # initialDelaySeconds here; the tight cadence makes leader handoff reflect
        # in the Service endpoints within a few seconds.
        httpGet:
          path: /readyz
          port: http
        periodSeconds: {{ $readiness.periodSeconds | default 5 }}
        failureThreshold: {{ $readiness.failureThreshold | default 3 }}
        timeoutSeconds: {{ $readiness.timeoutSeconds | default 2 }}
      resources:
        {{- toYaml .Values.resources | nindent 8 }}
      securityContext:
        {{- toYaml .Values.securityContext | nindent 8 }}
      volumeMounts:
        - name: config
          mountPath: /etc/runlore
          readOnly: true
        - name: tmp
          mountPath: /tmp
        {{- if .Values.catalog.configMap }}
        - name: catalog
          mountPath: {{ .Values.catalog.mountPath }}
          readOnly: true
        {{- else if or .Values.catalog.gitSync .Values.persistence.enabled }}
        - name: catalog
          mountPath: {{ .Values.catalog.mountPath }}
        {{- end }}
  volumes:
    - name: config
      configMap:
        name: {{ include "runlore.fullname" . }}-config
    - name: tmp
      emptyDir: {}
    {{- if .Values.catalog.configMap }}
    - name: catalog
      configMap:
        name: {{ .Values.catalog.configMap }}
    {{- else if $usesVolumeClaimTemplates }}
    {{- /* volumeClaimTemplates (statefulset.yaml) provides the "catalog" volume — no entry here. */}}
    {{- else if or .Values.catalog.gitSync .Values.persistence.enabled }}
    - name: catalog
      {{- if .Values.persistence.enabled }}
      persistentVolumeClaim:
        claimName: {{ .Values.persistence.existingClaim | default (printf "%s-data" (include "runlore.fullname" .)) }}
      {{- else }}
      emptyDir: {}
      {{- end }}
    {{- end }}
  {{- with .Values.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}
