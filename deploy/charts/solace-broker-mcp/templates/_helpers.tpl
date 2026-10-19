{{/*
Chart name, truncated to the 63-character label limit.
*/}}
{{- define "solace-broker-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name: names the Deployment, Service, ConfigMap and
chart-managed Secret. Unquoted, because NOTES.txt uses it in shell commands:
quote it at every YAML call site, or a name such as "true" or "123" renders as
a boolean or number.
*/}}
{{- define "solace-broker-mcp.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Selector labels. FIXED: a Deployment's spec.selector is immutable, so changing
these breaks `helm upgrade`. `instance` keeps two releases in one namespace from
selecting each other's pods. Used by every selector the chart renders.
*/}}
{{- define "solace-broker-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "solace-broker-mcp.name" . | quote }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "solace-broker-mcp.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "solace-broker-mcp.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Env var prefix for one broker's credentials: brokers.<name>.envPrefix, else the
name upper-cased with "-" replaced by "_" (collision-free, since broker names
are [A-Za-z0-9-] and compared case-insensitively). The server substitutes only
${[A-Z0-9_]+} references, so any other prefix would leave the literal
"${...}" text in place as the credential; a leading digit is rejected too, as
it is not a portable env var name.
Call with (dict "name" $name "broker" $broker).
*/}}
{{- define "solace-broker-mcp.envPrefix" -}}
{{- $prefix := .broker.envPrefix | default (.name | upper | replace "-" "_") }}
{{- if not (regexMatch "^[A-Z_][A-Z0-9_]*$" $prefix) }}
{{- fail (printf "brokers.%s.envPrefix %q must match ^[A-Z_][A-Z0-9_]*$ (set envPrefix explicitly)" .name $prefix) }}
{{- end }}
{{- $prefix }}
{{- end }}

{{/*
Broker credential env vars as a list of {name, key, existingSecret, value},
where key is the existing-Secret key and value the chart-managed credential.
Also validates every required input, so every template that renders calls it
(directly or through another helper) and an invalid release fails rendering
instead of crash-looping. Output: JSON, read back with fromJson.
*/}}
{{- define "solace-broker-mcp.credentials" -}}
{{- $creds := list }}
{{- $server := .Values.server | default dict }}
{{- /* In a pod only a wildcard bind works: the pod IP is not known at render
time, and under mcp_client_auth.mode static the server's own default is
127.0.0.1, which the Service and the kubelet probes cannot reach. */}}
{{- $listen := $server.listenAddress | default "" | toString | trim }}
{{- if not (has $listen (list "0.0.0.0" "::")) }}
{{- fail (printf "server.listenAddress must be \"0.0.0.0\" or \"::\" (a wildcard bind; the pod IP is not known at render time), got %q" ($server.listenAddress | default "" | toString)) }}
{{- end }}
{{- if eq (toString ($server.port | default 9090)) "9091" }}
{{- fail "server.port 9091 collides with the metrics port (9091); choose another port" }}
{{- end }}
{{- if not (kindIs "invalid" $server.logLevel) }}
{{- if not (has (lower (toString $server.logLevel)) (list "debug" "info" "warn" "error")) }}
{{- fail (printf "server.logLevel %q must be one of debug, info, warn, error" (toString $server.logLevel)) }}
{{- end }}
{{- end }}
{{- if not .Values.brokers }}
{{- fail "brokers: at least one broker is required (see values.yaml or README.md for an example)" }}
{{- end }}
{{- $lowerNames := dict }}
{{- range $name, $broker := .Values.brokers }}
{{- if kindIs "invalid" $broker }}{{ continue }}{{ end }}
{{- if hasKey $lowerNames (lower $name) }}
{{- fail (printf "broker names %q and %q collide: names are compared case-insensitively; rename one" (get $lowerNames (lower $name)) $name) }}
{{- end }}
{{- $_ := set $lowerNames (lower $name) $name }}
{{- if not ($broker.url | default "" | toString | trim) }}
{{- fail (printf "brokers.%s.url is required" $name) }}
{{- end }}
{{- /* No message here echoes the url: it may carry credentials, which would
then land in the terminal and CI logs. The same reason keeps these checks out
of values.schema.json, whose errors print the value. Userinfo is rejected by
the server too, but by then it would already sit in the ConfigMap. */}}
{{- $url := toString $broker.url }}
{{- if regexMatch "^([A-Za-z][A-Za-z0-9+.-]*://)?[^/?#]*@" $url }}
{{- fail (printf "brokers.%s.url must not include credentials: put them in brokers.%s.auth (or auth.existingSecret)" $name $name) }}
{{- end }}
{{- if or (not (regexMatch "^[Hh][Tt][Tt][Pp][Ss]?://[^/?#]+" $url)) (regexMatch "\\s" $url) }}
{{- fail (printf "brokers.%s.url must be http(s)://<host>[:port][/path] with no whitespace" $name) }}
{{- end }}
{{- $auth := $broker.auth | default dict }}
{{- $existing := $auth.existingSecret | default "" | toString | trim }}
{{- $prefix := include "solace-broker-mcp.envPrefix" (dict "name" $name "broker" $broker) }}
{{- $fields := list }}
{{- $mode := $auth.mode | default "" | toString }}
{{- if eq $mode "basic" }}
{{- $fields = list "username" "password" }}
{{- else if eq $mode "bearer" }}
{{- $fields = list "token" }}
{{- else }}
{{- fail (printf "brokers.%s.auth.mode must be basic or bearer, got %q" $name $mode) }}
{{- end }}
{{- range $fields }}
{{- if and (not $existing) (not (get $auth . | default "" | toString | trim)) }}
{{- fail (printf "brokers.%s.auth.%s is required (or set brokers.%s.auth.existingSecret)" $name . $name) }}
{{- end }}
{{- $creds = append $creds (dict "name" (printf "%s_%s" $prefix (upper .)) "key" . "existingSecret" $existing "value" (get $auth . | default "" | toString)) }}
{{- end }}
{{- end }}
{{- if not $creds }}
{{- fail "brokers: at least one broker is required (see values.yaml or README.md for an example)" }}
{{- end }}
{{- $mca := .Values.mcpClientAuth | default dict }}
{{- $mcaMode := $mca.mode | default "" | toString }}
{{- if ne $mcaMode "static" }}
{{- fail (printf "mcpClientAuth.mode %q is not supported by this chart version; only static (oauth arrives in a later chart version)" $mcaMode) }}
{{- end }}
{{- $mcaExisting := $mca.existingSecret | default "" | toString | trim }}
{{- if not (or ($mca.devToken | default "" | toString | trim) $mcaExisting) }}
{{- fail "mcpClientAuth.devToken or mcpClientAuth.existingSecret is required when mcpClientAuth.mode is static" }}
{{- end }}
{{- $creds = append $creds (dict "name" "DEV_TOKEN" "key" "dev-token" "existingSecret" $mcaExisting "value" ($mca.devToken | default "" | toString)) }}
{{- $seen := dict }}
{{- range $creds }}
{{- if hasKey $seen .name }}
{{- fail (printf "env var %s is produced twice; give one of the brokers a distinct envPrefix" .name) }}
{{- end }}
{{- $_ := set $seen .name true }}
{{- end }}
{{- toJson $creds }}
{{- end }}

{{/*
"true" when at least one credential is chart-managed, so the chart Secret
exists; empty otherwise.
*/}}
{{- define "solace-broker-mcp.hasManagedSecret" -}}
{{- range (include "solace-broker-mcp.credentials" . | fromJsonArray) }}
{{- if not .existingSecret }}true{{ break }}{{ end }}
{{- end }}
{{- end }}

{{/*
One "key: value" config line, written only when the value is non-null (the
config rendering rule). false and 0 are values, not absence. Scalars go
through toJson so strings are double-quoted: an unquoted ${VAR} reference is
re-typed after substitution (config.go), so a credential such as "null" would
otherwise decode as empty.
Call with (list indent key value).
*/}}
{{- define "solace-broker-mcp.kv" -}}
{{- $value := index . 2 }}
{{- if not (kindIs "invalid" $value) }}
{{ printf "%s%s: %s" (index . 0) (index . 1) (toJson $value) }}
{{- end }}
{{- end }}

{{/*
The server's config.yaml. Each key is written only when its value is non-null,
and a block is written only when it has at least one key. Credentials appear
only as ${<ENV VAR>} references; the values live in a Secret.
*/}}
{{- define "solace-broker-mcp.serverConfig" -}}
{{- $_ := include "solace-broker-mcp.credentials" . }}
{{- $server := .Values.server | default dict }}
{{- $semp := .Values.semp | default dict }}
{{- $mca := .Values.mcpClientAuth | default dict }}
{{- include "solace-broker-mcp.kv" (list "" "port" $server.port) }}
{{- include "solace-broker-mcp.kv" (list "" "log_level" $server.logLevel) }}
{{- include "solace-broker-mcp.kv" (list "" "listen_address" $server.listenAddress) }}
mcp_client_auth:
{{- include "solace-broker-mcp.kv" (list "  " "mode" $mca.mode) }}
  dev_token: "${DEV_TOKEN}"
{{- $sempLines := "" }}
{{- $sempKeys := dict "max_concurrent_per_broker" "maxConcurrentPerBroker" "request_timeout_duration" "requestTimeoutDuration" "request_min_interval" "requestMinInterval" "max_queue_wait" "maxQueueWait" "fair_scheduling" "fairScheduling" "retries" "retries" "retry_min_interval" "retryMinInterval" "retry_max_interval" "retryMaxInterval" }}
{{- range (list "max_concurrent_per_broker" "request_timeout_duration" "request_min_interval" "max_queue_wait" "fair_scheduling" "retries" "retry_min_interval" "retry_max_interval") }}
{{- $sempLines = printf "%s%s" $sempLines (include "solace-broker-mcp.kv" (list "  " . (index $semp (get $sempKeys .)))) }}
{{- end }}
{{- if $sempLines }}
semp:
{{- $sempLines }}
{{- end }}
brokers:
{{- range $name, $broker := .Values.brokers }}
{{- if kindIs "invalid" $broker }}{{ continue }}{{ end }}
{{- $prefix := include "solace-broker-mcp.envPrefix" (dict "name" $name "broker" $broker) }}
  {{ toJson $name }}:
{{- include "solace-broker-mcp.kv" (list "    " "url" $broker.url) }}
{{- include "solace-broker-mcp.kv" (list "    " "insecure_skip_verify" $broker.insecureSkipVerify) }}
    auth:
{{- include "solace-broker-mcp.kv" (list "      " "mode" $broker.auth.mode) }}
{{- if eq $broker.auth.mode "basic" }}
      username: {{ printf "${%s_USERNAME}" $prefix | toJson }}
      password: {{ printf "${%s_PASSWORD}" $prefix | toJson }}
{{- else }}
      token: {{ printf "${%s_TOKEN}" $prefix | toJson }}
{{- end }}
{{- end }}
{{- end }}

{{/*
GOMEMLIMIT: goMemLimit verbatim when set, else 75% of resources.limits.memory
floored to whole MiB, in Go's form (MiB, not Kubernetes' Mi). Only integer Mi
and Gi limits are supported: an unparseable GOMEMLIMIT is fatal at startup, so
anything else fails rendering instead.
*/}}
{{- define "solace-broker-mcp.goMemLimit" -}}
{{- if .Values.goMemLimit }}
{{- if not (regexMatch "^([0-9]+(B|KiB|MiB|GiB|TiB)?|off)$" .Values.goMemLimit) }}
{{- fail (printf "goMemLimit %q is not a value Go accepts: use bytes with an optional B, KiB, MiB, GiB or TiB suffix (Go's MiB, not Kubernetes' Mi), or off" .Values.goMemLimit) }}
{{- end }}
{{- .Values.goMemLimit }}
{{- else }}
{{- $limit := "" }}
{{- with .Values.resources }}{{ with .limits }}{{ $limit = toString (.memory | default "") }}{{ end }}{{ end }}
{{- $mib := 0 }}
{{- if regexMatch "^[0-9]+Mi$" $limit }}
{{- $mib = div (mul (trimSuffix "Mi" $limit | atoi) 3) 4 }}
{{- else if regexMatch "^[0-9]+Gi$" $limit }}
{{- $mib = mul (trimSuffix "Gi" $limit | atoi) 768 }}
{{- else }}
{{- fail (printf "cannot derive GOMEMLIMIT from resources.limits.memory %q: use an integer Mi or Gi value, or set goMemLimit explicitly (Go's form, e.g. 384MiB)" $limit) }}
{{- end }}
{{- if lt $mib 1 }}
{{- fail (printf "cannot derive GOMEMLIMIT from resources.limits.memory %q: 75%% is under 1MiB" $limit) }}
{{- end }}
{{- printf "%dMiB" $mib }}
{{- end }}
{{- end }}
