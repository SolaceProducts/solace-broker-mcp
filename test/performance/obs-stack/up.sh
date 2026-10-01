#!/usr/bin/env bash
# Renders the stack's generated configs into bin/ and starts it.
#
#   SCRAPE_TARGET=<host>:<port> ./up.sh
#
# Environment (see README.md):
#   SCRAPE_TARGET   required. host:port of the server's /metrics listener.
#   OTLP_BIND_ADDR  host address the collector's OTLP port 4317 is published
#                   on (default 127.0.0.1). On the rig: Box C's private IP.
#   DATA_DIR        where Prometheus and Tempo keep their data (default
#                   ./data). On the rig: /srv/obs, created and owned by the
#                   Box C setup step.
#   COMPOSE         compose command (default "docker compose"; e.g.
#                   "podman compose" locally).
#
# Safe to re-run, including with a changed SCRAPE_TARGET: configs are
# re-rendered, and a service whose rendered config changed is restarted to
# pick it up. compose up alone would not, since it compares only this
# directory's compose file and none of the services reloads a mounted file.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/bin"

if [[ -z "${SCRAPE_TARGET:-}" ]]; then
  echo "!! SCRAPE_TARGET is required: host:port of the server metrics listener," >&2
  echo "   e.g. SCRAPE_TARGET=host.docker.internal:9091 $0" >&2
  exit 1
fi
# The value lands inside YAML, so accept only the shape of an address.
if [[ ! "$SCRAPE_TARGET" =~ ^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._-]+):[0-9]+$ ]]; then
  echo "!! SCRAPE_TARGET must be host:port, got: $SCRAPE_TARGET" >&2
  exit 1
fi
read -r -a compose <<<"${COMPOSE:-docker compose}"

mkdir -p "$bin"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# render <service> <file> writes stdin to bin/<file>. When the file already
# existed with other content, the service is running the old version, so it
# is queued for a restart after compose up.
restart=()
render() {
  local svc=$1 dest="$bin/$2" new
  new="$(cat)"
  if [[ -f "$dest" && "$(<"$dest")" != "$new" ]]; then
    restart+=("$svc")
  fi
  printf '%s\n' "$new" >"$dest"
  echo "wrote $dest"
}

# shellcheck disable=SC2016 # the literal placeholder, not an expansion
placeholder='${SCRAPE_TARGET}'
tmpl="$(<"$here/prometheus-scrape.yml.tmpl")"
render prometheus-scrape prometheus-scrape.yml <<<"${tmpl//"$placeholder"/$SCRAPE_TARGET}"

OUT="$tmp/otelcol.yaml" "$here/../../e2e-dashboard/uncomment-metrics-pipeline.sh" >/dev/null
render otel-collector otelcol.yaml <"$tmp/otelcol.yaml"

# The committed Tempo config plus the soak's retention block, joined rather
# than copied so a change to the committed file reaches the soak. Appending
# is only safe while the committed file sets no compactor block of its own;
# fail loudly if it ever does, rather than run with a duplicate key.
tempo_src="$here/../../../deploy/otel-collector/docker/tempo.yaml"
if grep -q '^compactor:' "$tempo_src"; then
  echo "!! $tempo_src now has its own compactor: block; merge tempo-retention.yaml into it by hand" >&2
  exit 1
fi
render tempo tempo.yaml < <(cat "$tempo_src"; echo; cat "$here/tempo-retention.yaml")

# From $here, so a relative DATA_DIR names the same directory it does to
# compose, which resolves it against the compose file.
cd "$here"
data="${DATA_DIR:-./data}"
mkdir -p "$data/prometheus-scrape" "$data/prometheus-otlp" "$data/tempo"

# On a Linux host the containers write their bind mounts as the image users
# (Prometheus: nobody, Tempo: 10001), so a directory owned by anyone else is
# unwritable to them and the service crash-loops behind restart:
# unless-stopped. The Box C setup step creates them with those owners; a
# local run has to do the same. Only a warning: rootless podman maps those
# users to subordinate IDs, where this check cannot say what is right.
if [[ "$(uname -s)" == Linux ]]; then
  for pair in prometheus-scrape:65534 prometheus-otlp:65534 tempo:10001; do
    dir="$data/${pair%%:*}" want="${pair##*:}"
    owner="$(stat -c %u "$dir")" others=$((8#$(stat -c %a "$dir") & 7))
    # Write and search for "others" would also do; anything less fails in
    # the container.
    if [[ "$owner" != "$want" ]] && (((others & 3) != 3)); then
      echo "!! $dir is owned by uid $owner; the container writes it as uid $want." >&2
      echo "   With rootful Docker: sudo chown $want:$want '$dir'" >&2
    fi
  done
fi

"${compose[@]}" up -d
if ((${#restart[@]})); then
  echo "config changed for: ${restart[*]}; restarting to pick it up"
  "${compose[@]}" restart "${restart[@]}"
fi
