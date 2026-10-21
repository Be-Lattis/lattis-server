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

{{/* Conteneur d'initialisation : clés publiques tirées de la clé de signature
     (clés privées converties par openssl, clés publiques recopiées), pour
     lattis_oauth.public_keys_file. La clé privée reste dans ce conteneur.
     Image de PostgreSQL, pas celle de lattisd : une nouvelle version de
     lattisd ne relance pas PostgreSQL. */}}
{{- define "lattis.publicKeysInit" -}}
- name: public-keys
  image: "{{ .Values.image.server.repository }}:{{ .Values.image.server.tag }}"
  imagePullPolicy: {{ .Values.image.pullPolicy }}
  command:
    - sh
    - -c
    - |
      set -eu
      : > /keys/public.pem
      awk -v out=/keys/public.pem '
      /-----BEGIN PRIVATE KEY-----/ { mode = 1; cmd = "openssl pkey -pubout >> " out }
      /-----BEGIN PUBLIC KEY-----/ { mode = 2 }
      mode == 1 { print | cmd }
      mode == 2 { print >> out }
      /-----END PRIVATE KEY-----/ { close(cmd); mode = 0 }
      /-----END PUBLIC KEY-----/ { close(out); mode = 0 }
      ' /signing/signing.pem
      grep -q "BEGIN PUBLIC KEY" /keys/public.pem
  volumeMounts:
    - { name: signing, mountPath: /signing, readOnly: true }
    - { name: keys, mountPath: /keys }
{{- end -}}
