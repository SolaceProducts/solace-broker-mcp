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
{{- /* The server refuses the same clash at config load, but only with scraping
on; the chart refuses it always, as the Service and the container cannot carry
two ports with one number either way. */}}
{{- $metricsPort := include "solace-broker-mcp.metricsPort" . }}
{{- if eq (toString ($server.port | default 9090)) $metricsPort }}
{{- fail (printf "server.port %s collides with the metrics port (%s, from observability.metrics.bindAddress); choose another port" $metricsPort $metricsPort) }}
{{- end }}
{{- if not (kindIs "invalid" $server.logLevel) }}
{{- if not (has (lower (toString $server.logLevel)) (list "debug" "info" "warn" "error")) }}
{{- fail (printf "server.logLevel %q must be one of debug, info, warn, error" (toString $server.logLevel)) }}
{{- end }}
{{- end }}
{{- if not .Values.brokers }}
{{- fail "brokers: at least one broker is required (see values.yaml or README.md for an example)" }}
{{- end }}
{{- $mca := .Values.mcpClientAuth | default dict }}
{{- $mcaMode := $mca.mode | default "" | toString }}
{{- if not (has $mcaMode (list "static" "oauth")) }}
{{- fail (printf "mcpClientAuth.mode must be static or oauth, got %q" $mcaMode) }}
{{- end }}
{{- $oauth := eq $mcaMode "oauth" }}
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
{{- /* The server appends SEMP paths to the url (sempv1/sempv2 client.go), so
a query string or fragment can never work. Rejecting both outright also keeps
a token passed that way out of the ConfigMap, without guessing parameter names.
A secret in the path cannot be detected; README.md says the url must hold none. */}}
{{- if regexMatch "[?#]" $url }}
{{- fail (printf "brokers.%s.url must not include a query string or fragment: the server appends SEMP paths to it; put credentials in brokers.%s.auth (or auth.existingSecret)" $name $name) }}
{{- end }}
{{- if or (not (regexMatch "^[Hh][Tt][Tt][Pp][Ss]?://[^/?#]+" $url)) (regexMatch "\\s" $url) }}
{{- fail (printf "brokers.%s.url must be http(s)://<host>[:port][/path] with no whitespace" $name) }}
{{- end }}
{{- /* oauth is the server's production mode: it refuses a plain http broker
url and, unless allow_insecure_broker_tls accepts the risk, a disabled
certificate check (config.go validateBrokerURL, validate). */}}
{{- if $oauth }}
{{- if not (regexMatch "^[Hh][Tt][Tt][Pp][Ss]://" $url) }}
{{- fail (printf "brokers.%s.url must be https:// when mcpClientAuth.mode is oauth" $name) }}
{{- end }}
{{- if and $broker.insecureSkipVerify (not $.Values.allowInsecureBrokerTls) }}
{{- fail (printf "brokers.%s.insecureSkipVerify is true while mcpClientAuth.mode is oauth: set allowInsecureBrokerTls: true to accept the risk, or use a trusted certificate" $name) }}
{{- end }}
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
{{- include "solace-broker-mcp.validateClientAuth" . }}
{{- /* Under oauth the dev token is not used: a devToken or existingSecret left
over from a shared base values file is ignored, never rendered. */}}
{{- if not $oauth }}
{{- $mcaExisting := $mca.existingSecret | default "" | toString | trim }}
{{- if not (or ($mca.devToken | default "" | toString | trim) $mcaExisting) }}
{{- fail "mcpClientAuth.devToken or mcpClientAuth.existingSecret is required when mcpClientAuth.mode is static" }}
{{- end }}
{{- $creds = append $creds (dict "name" "DEV_TOKEN" "key" "dev-token" "existingSecret" $mcaExisting "value" ($mca.devToken | default "" | toString)) }}
{{- end }}
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
mcpClientAuth.toolAuthorization.accessLevelGroups without its null entries:
null is how a values overlay drops a group a base file granted, as for
brokers. Output: JSON, read back with fromJson.
*/}}
{{- define "solace-broker-mcp.accessLevelGroups" -}}
{{- $groups := dict }}
{{- range $name, $tools := ((.Values.mcpClientAuth | default dict).toolAuthorization | default dict).accessLevelGroups | default dict }}
{{- if not (kindIs "invalid" $tools) }}{{ $_ := set $groups $name $tools }}{{ end }}
{{- end }}
{{- toJson $groups }}
{{- end }}

{{/*
Validates mcpClientAuth and the switches oauth depends on, mirroring the
server's startup errors (internal/config/config.go validate and
validateToolAuthorization). Called from credentials, so every template runs it.
As for broker urls, no message echoes a url.
*/}}
{{- define "solace-broker-mcp.validateClientAuth" -}}
{{- $mca := .Values.mcpClientAuth | default dict }}
{{- $ta := $mca.toolAuthorization | default dict }}
{{- $groups := include "solace-broker-mcp.accessLevelGroups" . | fromJson }}
{{- /* An empty accessLevelGroups map is the default (Helm deep-merges maps), so
only a non-empty one counts as set. */}}
{{- $taSet := or (not (kindIs "invalid" $ta.enabled)) (not (kindIs "invalid" $ta.groupsClaimName)) (not (kindIs "invalid" $ta.filterToolsList)) (not (empty $groups)) }}
{{- if ne (toString $mca.mode) "oauth" }}
{{- if $taSet }}
{{- fail (printf "mcpClientAuth.toolAuthorization is only supported when mcpClientAuth.mode is oauth (currently %q): set mode to oauth or remove it" (toString $mca.mode)) }}
{{- end }}
{{- else }}
{{- range (list "issuer" "audience" "resourceUrl") }}
{{- if not (get $mca . | default "" | toString | trim) }}
{{- fail (printf "mcpClientAuth.%s is required when mcpClientAuth.mode is oauth" .) }}
{{- end }}
{{- end }}
{{- /* The server parses both with url.Parse, which refuses a host character
outside RFC 3986, a non-numeric port, a malformed %-escape in the path or
fragment (not the query, which it leaves unparsed) and any ASCII control
character. This is slightly stricter: it also refuses whitespace, a < or " or
non-ASCII character in the host, an empty port, and a bracketed host that is
not hex digits, colons and dots (so no IPv6 zone).
The server substitutes ${VAR_NAME} references (config.go envVarPattern) before
it validates, so a value carrying one skips the URL pattern: what the
reference expands to is unknown here. */}}
{{- $urlRe := "^[Hh][Tt][Tt][Pp][Ss]://([A-Za-z0-9._~!$&'()*+,;=-]+|\\[[0-9A-Fa-f:.]+\\])(:[0-9]+)?(/([^?#%]|%[0-9A-Fa-f]{2})*)?(\\?[^#]*)?(#([^%]|%[0-9A-Fa-f]{2})*)?$" }}
{{- range (list "issuer" "resourceUrl") }}
{{- $u := get $mca . | toString }}
{{- $badShape := not (regexMatch $urlRe $u) }}
{{- if regexMatch "\\$\\{[A-Z0-9_]+\\}" $u }}
{{- /* Still https:// unless the reference itself comes first and may supply
the scheme: a scheme written out in the value is known here. */}}
{{- $badShape = not (regexMatch "^(\\$\\{[A-Z0-9_]+\\}|[Hh][Tt][Tt][Pp][Ss]://)" $u) }}
{{- end }}
{{- if or (regexMatch "^([A-Za-z][A-Za-z0-9+.-]*://)?[^/?#]*@" $u) $badShape (regexMatch "[\\s\\x00-\\x1f\\x7f]" $u) }}
{{- fail (printf "mcpClientAuth.%s must be https://<host>[:port][/path] with no credentials or whitespace when mcpClientAuth.mode is oauth" .) }}
{{- end }}
{{- end }}
{{- /* The chart has no in-pod TLS yet (SOL-156417), and oauth refuses a
plaintext listener unless TLS is acknowledged as terminated upstream. */}}
{{- if ne (toString .Values.tlsTerminatedUpstream) "true" }}
{{- fail "tlsTerminatedUpstream: true is required when mcpClientAuth.mode is oauth: terminate TLS at the Ingress or gateway in front of the Service (in-pod TLS is not yet supported by this chart)" }}
{{- end }}
{{- if kindIs "invalid" $ta.enabled }}
{{- fail "mcpClientAuth.toolAuthorization.enabled must be set to true or false when mcpClientAuth.mode is oauth (false lets any authenticated caller invoke any registered tool)" }}
{{- end }}
{{- if and $ta.enabled (empty $groups) }}
{{- fail "mcpClientAuth.toolAuthorization.accessLevelGroups is required when mcpClientAuth.toolAuthorization.enabled is true" }}
{{- end }}
{{- if and (not (kindIs "invalid" $ta.groupsClaimName)) (not ($ta.groupsClaimName | toString | trim)) }}
{{- fail "mcpClientAuth.toolAuthorization.groupsClaimName must not be blank: set it, or null for the server default \"groups\"" }}
{{- end }}
{{- /* The schema's \S patterns are ASCII-only. The server trims with
strings.TrimSpace and splits on unicode.IsSpace, as sprig's trim and this
class do, so a no-break space fails here rather than at startup. */}}
{{- range $group, $tools := $groups }}
{{- if not (trim $group) }}
{{- fail "mcpClientAuth.toolAuthorization.accessLevelGroups: a group name is empty or whitespace-only" }}
{{- end }}
{{- range $tools }}
{{- if or (not .) (ne (toString .) (trim (toString .))) }}
{{- fail (printf "mcpClientAuth.toolAuthorization.accessLevelGroups.%s: a tool name is empty or has leading or trailing whitespace" $group) }}
{{- end }}
{{- end }}
{{- end }}
{{- range $i, $scope := ($mca.scopesSupported | default list) }}
{{- if or (not $scope) (regexMatch "[\\s\\x0B\\x{85}\\p{Z}]" (toString $scope)) }}
{{- fail (printf "mcpClientAuth.scopesSupported[%d] must be non-empty with no whitespace" $i) }}
{{- end }}
{{- end }}
{{- end }}
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
{{- $metrics := (.Values.observability | default dict).metrics | default dict }}
{{- $semp := .Values.semp | default dict }}
{{- $mca := .Values.mcpClientAuth | default dict }}
{{- include "solace-broker-mcp.kv" (list "" "port" $server.port) }}
{{- include "solace-broker-mcp.kv" (list "" "log_level" $server.logLevel) }}
{{- include "solace-broker-mcp.kv" (list "" "listen_address" $server.listenAddress) }}
{{- include "solace-broker-mcp.kv" (list "" "tls_terminated_upstream" .Values.tlsTerminatedUpstream) }}
{{- include "solace-broker-mcp.kv" (list "" "enable_write_tools" .Values.enableWriteTools) }}
{{- include "solace-broker-mcp.kv" (list "" "allow_insecure_broker_tls" .Values.allowInsecureBrokerTls) }}
mcp_client_auth:
{{- include "solace-broker-mcp.kv" (list "  " "mode" $mca.mode) }}
{{- if eq (toString $mca.mode) "oauth" }}
{{- include "solace-broker-mcp.kv" (list "  " "issuer" $mca.issuer) }}
{{- include "solace-broker-mcp.kv" (list "  " "audience" $mca.audience) }}
{{- include "solace-broker-mcp.kv" (list "  " "resource_url" $mca.resourceUrl) }}
{{- include "solace-broker-mcp.kv" (list "  " "scopes_supported" $mca.scopesSupported) }}
{{- $ta := $mca.toolAuthorization | default dict }}
{{- $taLines := "" }}
{{- $taLines = printf "%s%s" $taLines (include "solace-broker-mcp.kv" (list "    " "enabled" $ta.enabled)) }}
{{- $taLines = printf "%s%s" $taLines (include "solace-broker-mcp.kv" (list "    " "groups_claim_name" $ta.groupsClaimName)) }}
{{- $taLines = printf "%s%s" $taLines (include "solace-broker-mcp.kv" (list "    " "filter_tools_list" $ta.filterToolsList)) }}
{{- $groups := include "solace-broker-mcp.accessLevelGroups" . | fromJson }}
{{- if not (empty $groups) }}
{{- $taLines = printf "%s%s" $taLines (include "solace-broker-mcp.kv" (list "    " "access_level_groups" $groups)) }}
{{- end }}
{{- if $taLines }}
  tool_authorization:
{{- $taLines }}
{{- end }}
{{- else }}
  dev_token: "${DEV_TOKEN}"
{{- end }}
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
{{- $obs := .Values.observability | default dict }}
{{- $obsLines := include "solace-broker-mcp.kv" (list "  " "metrics_bind_address" $metrics.bindAddress) }}
{{- $obsKeys := dict "shutdown_drain_delay_s" "shutdownDrainDelayS" "service_name" "serviceName" "service_instance_id" "serviceInstanceId" "deployment_environment" "deploymentEnvironment" "cloud_region" "cloudRegion" "saturation_threshold_ms" "saturationThresholdMs" "otel_self_stats_interval_s" "otelSelfStatsIntervalS" }}
{{- range (list "shutdown_drain_delay_s" "service_name" "service_instance_id" "deployment_environment" "cloud_region" "saturation_threshold_ms" "otel_self_stats_interval_s") }}
{{- $obsLines = printf "%s%s" $obsLines (include "solace-broker-mcp.kv" (list "  " . (index $obs (get $obsKeys .)))) }}
{{- end }}
{{- if $obsLines }}
observability:
{{- $obsLines }}
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

{{/*
The metrics port, from observability.metrics.bindAddress (null: the server
default ":9091"). Only an all-interfaces bind is accepted: the Service,
ServiceMonitor and NetworkPolicy reach the listener through the pod IP, so a
loopback or pod-specific host would be a target that never answers.
*/}}
{{- define "solace-broker-mcp.metricsPort" -}}
{{- $metrics := (.Values.observability | default dict).metrics | default dict }}
{{- if kindIs "invalid" $metrics.bindAddress }}
{{- "9091" }}
{{- else }}
{{- $addr := toString $metrics.bindAddress }}
{{- $port := 0 }}
{{- if regexMatch "^(|0\\.0\\.0\\.0|\\[::\\]):[0-9]{1,5}$" $addr }}
{{- $port = regexReplaceAll "^.*:" $addr "" | atoi }}
{{- else if regexMatch "^.+:[0-9]{1,5}$" $addr }}
{{- fail (printf "observability.metrics.bindAddress %q must bind all interfaces (\":N\", \"0.0.0.0:N\" or \"[::]:N\"): the Service and Prometheus reach it through the pod IP" $addr) }}
{{- end }}
{{- if or (lt $port 1) (gt $port 65535) }}
{{- fail (printf "observability.metrics.bindAddress %q must be \":N\", \"0.0.0.0:N\" or \"[::]:N\" with N from 1 to 65535" $addr) }}
{{- end }}
{{- $port }}
{{- end }}
{{- end }}

{{/*
"true" when the text between an IPv6 literal's brackets is an address
netip.ParseAddr accepts (zone IDs aside): 1-4 hex digits per group, at most one
"::" standing for at least one group, and an optional dotted IPv4 tail that
counts as two groups. Eight groups without "::", seven at most with it.
*/}}
{{- define "solace-broker-mcp.isIPv6" -}}
{{- $ok := not (or (contains ":::" .) (gt (len (regexFindAll "::" . -1)) 1)) }}
{{- $compressed := contains "::" . }}
{{- $head := list }}
{{- $tail := list }}
{{- if $compressed }}
{{- $halves := splitList "::" . }}
{{- if index $halves 0 }}{{ $head = splitList ":" (index $halves 0) }}{{ end }}
{{- if index $halves 1 }}{{ $tail = splitList ":" (index $halves 1) }}{{ end }}
{{- else }}
{{- $tail = splitList ":" . }}
{{- end }}
{{- $octet := "(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])" }}
{{- $ipv4 := printf "^%s(\\.%s){3}$" $octet $octet }}
{{- $groups := 0 }}
{{- range $head }}
{{- if regexMatch "^[0-9A-Fa-f]{1,4}$" . }}{{ $groups = add1 $groups }}{{ else }}{{ $ok = false }}{{ end }}
{{- end }}
{{- $last := sub (len $tail) 1 }}
{{- range $i, $g := $tail }}
{{- if regexMatch "^[0-9A-Fa-f]{1,4}$" $g }}{{ $groups = add1 $groups }}
{{- else if and (eq $i $last) (regexMatch $ipv4 $g) }}{{ $groups = add $groups 2 }}
{{- else }}{{ $ok = false }}
{{- end }}
{{- end }}
{{- if or (and $compressed (gt $groups 7)) (and (not $compressed) (ne $groups 8)) }}{{ $ok = false }}{{ end }}
{{- if $ok }}true{{ end }}
{{- end }}

{{/*
The OBS_* switches and the OTLP endpoint as a list of {name, value}. A var is
rendered only when it differs from the server's default
(internal/config/observability.go), so the defaults render none. Also enforces
the cross-field rules. Output: JSON, read back with fromJsonArray.
*/}}
{{- define "solace-broker-mcp.obsEnv" -}}
{{- $obs := .Values.observability | default dict }}
{{- $env := list }}
{{- $endpoint := $obs.otlpEndpoint | default "" | toString | trim }}
{{- $tracing := dig "tracing" "enabled" false $obs }}
{{- $metricsOTLP := dig "metrics" "otlp" "enabled" false $obs }}
{{- if and $endpoint (not (or $tracing $metricsOTLP)) }}
{{- fail "observability.otlpEndpoint is set but neither observability.tracing.enabled nor observability.metrics.otlp.enabled is true: nothing would use it" }}
{{- end }}
{{- /* Go's url.Parse reads "host:4317" as scheme "host", so the SDK dials it
over TLS, and rejects a bare "10.0.0.5:4317", so the SDK stays on
localhost:4317 with no error. As for broker urls, no message echoes the value:
it may carry credentials. */}}
{{- if $endpoint }}
{{- if regexMatch "^([A-Za-z][A-Za-z0-9+.-]*://)?[^/?#]*@" $endpoint }}
{{- fail "observability.otlpEndpoint must not include credentials: they would sit in plain text in the Deployment spec" }}
{{- end }}
{{- /* The host is DNS labels or a bracketed IPv6 literal. url.Parse rejects a
malformed literal, which again leaves the exporter on localhost, so the
literal is checked group by group in isIPv6. */}}
{{- $label := "[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?" }}
{{- $endpointRe := printf "^[Hh][Tt][Tt][Pp][Ss]?://(%s(\\.%s)*|\\[(?P<v6>[0-9A-Fa-f.]*:[0-9A-Fa-f:.]*)\\])(?P<port>:[0-9]{1,5})?/?$" $label $label }}
{{- $badEndpoint := "observability.otlpEndpoint must be http(s)://<host>[:port] with no path, query or whitespace" }}
{{- if not (regexMatch $endpointRe $endpoint) }}
{{- fail $badEndpoint }}
{{- end }}
{{- $v6 := regexReplaceAll $endpointRe $endpoint "${v6}" }}
{{- if and $v6 (not (include "solace-broker-mcp.isIPv6" $v6)) }}
{{- fail $badEndpoint }}
{{- end }}
{{- $endpointPort := regexReplaceAll $endpointRe $endpoint "${port}" | trimPrefix ":" }}
{{- if and $endpointPort (or (lt (atoi $endpointPort) 1) (gt (atoi $endpointPort) 65535)) }}
{{- fail "observability.otlpEndpoint port must be from 1 to 65535" }}
{{- end }}
{{- $env = append $env (dict "name" "OTEL_EXPORTER_OTLP_ENDPOINT" "value" $endpoint) }}
{{- end }}
{{- if not (dig "correlationId" "enabled" true $obs) }}
{{- $env = append $env (dict "name" "OBS_CORRELATION_ID_ENABLED" "value" "false") }}
{{- end }}
{{- /* A ServiceMonitor with no listener is a target that never appears. */}}
{{- if or (dig "metrics" "scrape" "enabled" false $obs) (dig "serviceMonitor" "enabled" false $obs) }}
{{- $env = append $env (dict "name" "OBS_METRICS_SCRAPE_ENABLED" "value" "true") }}
{{- end }}
{{- if dig "auditLog" "enabled" false $obs }}
{{- $env = append $env (dict "name" "OBS_AUDIT_LOG_ENABLED" "value" "true") }}
{{- end }}
{{- if $tracing }}
{{- $env = append $env (dict "name" "OBS_TRACING_ENABLED" "value" "true") }}
{{- end }}
{{- if dig "saturationEvents" "enabled" false $obs }}
{{- $env = append $env (dict "name" "OBS_SATURATION_EVENTS_ENABLED" "value" "true") }}
{{- end }}
{{- /* null leaves it unset, following the metrics switches; an explicit value,
true or false, pins it. */}}
{{- $afc := dig "authFailureCounter" "enabled" nil $obs }}
{{- if not (kindIs "invalid" $afc) }}
{{- $env = append $env (dict "name" "OBS_AUTH_FAILURE_COUNTER_ENABLED" "value" (toString $afc)) }}
{{- end }}
{{- if $metricsOTLP }}
{{- $env = append $env (dict "name" "OBS_METRICS_OTLP_ENABLED" "value" "true") }}
{{- end }}
{{- toJson $env }}
{{- end }}

{{/*
Validates extraEnv. It cannot set an OBS_* switch or MCP_SERVER_PORT (each has
a named value), nor any name the chart already renders: Kubernetes keeps the
last of two same-named entries, and an env entry overrides envFrom, so a
duplicate would silently replace the chart's value. No message echoes a value,
which may be a secret.
Call with (dict "extraEnv" .Values.extraEnv "reserved" <list of names>).
*/}}
{{- define "solace-broker-mcp.validateExtraEnv" -}}
{{- $seen := dict }}
{{- range $i, $e := .extraEnv }}
{{- $name := (get ($e | default dict) "name") | default "" | toString }}
{{- if not $name }}
{{- fail (printf "extraEnv[%d] has no name" $i) }}
{{- end }}
{{- if hasPrefix "OBS_" $name }}
{{- fail (printf "extraEnv: %s is set by the chart's observability values; use observability.* instead" $name) }}
{{- end }}
{{- if eq $name "MCP_SERVER_PORT" }}
{{- fail "extraEnv: MCP_SERVER_PORT overrides server.port behind the Service and probes; set server.port instead" }}
{{- end }}
{{- if has $name $.reserved }}
{{- fail (printf "extraEnv: %s is already set by the chart; set it through the chart's values instead" $name) }}
{{- end }}
{{- if hasKey $seen $name }}
{{- fail (printf "extraEnv: %s appears more than once" $name) }}
{{- end }}
{{- $_ := set $seen $name true }}
{{- end }}
{{- end }}
