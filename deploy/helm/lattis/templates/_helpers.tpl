{{- define "lattis.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "lattis.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "lattis.selector" -}}
app.kubernetes.io/name: {{ .root.Chart.Name }}
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "lattis.required" -}}
{{- $_ := required "publicURL est obligatoire" .Values.publicURL -}}
{{- $_ := required "secrets.existingSecret est obligatoire" .Values.secrets.existingSecret -}}
{{- $_ := required "secrets.signingKeySecret est obligatoire" .Values.secrets.signingKeySecret -}}
{{- $_ := required "secrets.postgresTLSSecret est obligatoire" .Values.secrets.postgresTLSSecret -}}
{{- $_ := required "lattisd.bootstrapOwner est obligatoire" .Values.lattisd.bootstrapOwner -}}
{{- if and .Values.replicas.publicEndpoints (ne (len .Values.replicas.publicEndpoints) (int .Values.replicas.count)) -}}
{{- fail "replicas.publicEndpoints : une adresse par réplica" -}}
{{- end -}}
{{- end -}}

{{- define "lattis.primaryHost" -}}
{{ include "lattis.fullname" . }}-postgres.{{ .Release.Namespace }}.svc
{{- end -}}

{{/* Nom DNS du pod n d'un réplica (service sans adresse). */}}
{{- define "lattis.replicaHost" -}}
{{ include "lattis.fullname" .root }}-replica-{{ .i }}.{{ include "lattis.fullname" .root }}-replica-hl.{{ .root.Release.Namespace }}.svc
{{- end -}}

{{- define "lattis.publicRW" -}}
{{- default (printf "%s:5432" (include "lattis.primaryHost" .)) .Values.postgres.publicReadWrite -}}
{{- end -}}

{{- define "lattis.publicRO" -}}
{{- $out := list -}}
{{- range $i, $_ := until (int .Values.replicas.count) -}}
{{- if $.Values.replicas.publicEndpoints -}}
{{- $out = append $out (index $.Values.replicas.publicEndpoints $i) -}}
{{- else -}}
{{- $out = append $out (printf "%s:5432" (include "lattis.replicaHost" (dict "root" $ "i" $i))) -}}
{{- end -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}

{{- define "lattis.roURLs" -}}
{{- $out := list -}}
{{- range $i, $_ := until (int .Values.replicas.count) -}}
{{- $out = append $out (printf "postgres://lattis_service:$(LATTIS_SERVICE_PASSWORD)@%s:5432/%s?sslmode=require&connect_timeout=3" (include "lattis.replicaHost" (dict "root" $ "i" $i)) $.Values.postgres.database) -}}
{{- end -}}
{{- join "," $out -}}
{{- end -}}

{{/* Volumes communs aux pods PostgreSQL : TLS et clé publique des jetons. */}}
{{- define "lattis.pgVolumes" -}}
- name: tls
  secret:
    secretName: {{ .Values.secrets.postgresTLSSecret }}
    items:
      - { key: tls.crt, path: server.crt }
      - { key: tls.key, path: server.key }
    defaultMode: 0440
- name: signing
  secret:
    secretName: {{ .Values.secrets.signingKeySecret }}
    defaultMode: 0440
- name: keys
  emptyDir: {}
{{- end -}}

{{/* Conteneur d'initialisation : clé publique tirée de la clé de signature,
     pour lattis_oauth.public_keys_file. La clé privée reste dans ce conteneur. */}}
{{- define "lattis.publicKeysInit" -}}
- name: public-keys
  image: "{{ .Values.image.lattisd.repository }}:{{ .Values.image.lattisd.tag | default .Chart.AppVersion }}"
  imagePullPolicy: {{ .Values.image.pullPolicy }}
  args: ["public-keys", "/keys/public.pem"]
  env:
    - { name: LATTIS_SIGNING_KEY_FILE, value: /signing/signing.pem }
  volumeMounts:
    - { name: signing, mountPath: /signing, readOnly: true }
    - { name: keys, mountPath: /keys }
{{- end -}}
