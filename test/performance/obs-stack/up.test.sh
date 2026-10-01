#!/usr/bin/env bash
# Copyright 2024-2026 Solace Corporation. All rights reserved.
#
# Self-test for up.sh. No containers: COMPOSE is a stub that records the
# commands it was given.
#
# up.sh builds its configs from files it does not own: the collector and
# Tempo configs in deploy/otel-collector/docker/, and the metrics-pipeline
# script and OTLP Prometheus config in test/e2e-dashboard/. Nothing else ties
# those trees to this one, so a move or a reshape over there would surface
# only on the rig, mid-soak. This runs up.sh against the real files and checks
# the shape of what it renders, so that break fails here instead.
#
# It runs in a copy of the three trees under mktemp, at their real relative
# paths, so it never touches the bin/ of a stack running from this checkout.
#
# Usage: ./up.test.sh

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../.." && pwd)"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok() {   printf '  ok    %s\n' "$1"; pass=$((pass + 1)); }
bad() {  printf '  FAIL  %s\n' "$1"; fail=$((fail + 1)); }

# indent <file> [max-lines] — echo a file under the failure that names it.
indent() {
  awk -v max="${2:-12}" 'NR <= max { print "        " $0 }' "$1"
}

report() {
  echo
  echo "$pass passed, $fail failed"
  [[ "$fail" -eq 0 ]]
}

# ---- the borrowed files ------------------------------------------------------

# Everything up.sh reads from outside obs-stack/. If up.sh starts borrowing
# another file, the run below fails on it missing from the copy: add it here.
borrowed=(
  deploy/otel-collector/docker/otelcol.yaml
  deploy/otel-collector/docker/tempo.yaml
  test/e2e-dashboard/uncomment-metrics-pipeline.sh
  test/e2e-dashboard/prometheus-otlp.yml
)
missing=0
for f in "${borrowed[@]}"; do
  if [[ ! -f "$repo/$f" ]]; then
    bad "borrowed file is gone: $f"
    missing=1
  fi
done
if ((missing)); then
  report
  exit 1
fi
ok "all ${#borrowed[@]} borrowed files exist"

for f in "${borrowed[@]}"; do
  mkdir -p "$tmp/$(dirname "$f")"
  cp -p "$repo/$f" "$tmp/$f"
done
stack="$tmp/test/performance/obs-stack"
mkdir -p "$stack"
for f in up.sh docker-compose.yml prometheus-scrape.yml.tmpl tempo-retention.yaml; do
  cp -p "$here/$f" "$stack/"
done

log="$tmp/compose.log"
cat >"$tmp/compose" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$COMPOSE_LOG"
EOF
chmod +x "$tmp/compose"

# up <SCRAPE_TARGET> — run up.sh in the copy, compose log reset first. An
# empty target runs with it unset.
up() {
  : >"$log"
  local -a target=()
  [[ -n "$1" ]] && target=(SCRAPE_TARGET="$1")
  (cd "$stack" && env -u SCRAPE_TARGET COMPOSE="$tmp/compose" COMPOSE_LOG="$log" \
    DATA_DIR="$tmp/data" ${target[@]+"${target[@]}"} ./up.sh) >"$tmp/out" 2>&1
}

# ---- SCRAPE_TARGET is checked before anything is written or started ---------

for t in '' 'x"]; evil' 'host' 'host:' 'host:0' 'host:65536' 'host:99999' '[fd00::1]'; do
  if ! up "$t" && [[ ! -s "$log" ]]; then
    ok "refuses SCRAPE_TARGET='$t' without calling compose"
  else
    bad "accepted SCRAPE_TARGET='$t' (or called compose)"
    indent "$tmp/out"
  fi
done

# ---- a first run renders every config from the real sources -----------------

if up host.docker.internal:9091; then
  ok "first run succeeds"
else
  bad "first run failed"
  indent "$tmp/out" 20
  report
  exit 1
fi
bin="$stack/bin"

if [[ "$(cat "$log")" == "up -d" ]]; then
  ok "first run calls compose up -d and restarts nothing"
else
  bad "first run compose calls were not just 'up -d'"
  indent "$log"
fi

# shellcheck disable=SC2016 # the literal placeholder, not an expansion
if grep -qF 'targets: ["host.docker.internal:9091"]' "$bin/prometheus-scrape.yml" &&
  ! grep -qF '${SCRAPE_TARGET}' "$bin/prometheus-scrape.yml"; then
  ok "scrape config carries the target, placeholder gone"
else
  bad "scrape config does not carry the target"
  indent "$bin/prometheus-scrape.yml"
fi

if cmp -s "$bin/prometheus-otlp.yml" "$repo/test/e2e-dashboard/prometheus-otlp.yml"; then
  ok "OTLP Prometheus config is e2e-dashboard's, byte for byte"
else
  bad "OTLP Prometheus config differs from e2e-dashboard's"
fi

# Every ./bin/ file the compose file mounts must be one up.sh wrote, and
# every other relative host path it mounts must exist.
mounts_ok=1
while read -r src; do
  [[ -e "$stack/$src" ]] || { bad "compose mounts $src, which does not exist after up.sh"; mounts_ok=0; }
done < <(sed -nE 's/^[[:space:]]*- (\.\.?\/[^:]+):.*/\1/p' "$stack/docker-compose.yml")
((mounts_ok)) && ok "every relative path the compose file mounts exists"

for d in prometheus-scrape prometheus-otlp tempo; do
  [[ -d "$tmp/data/$d" ]] || bad "data directory $d not created"
done
[[ -d "$tmp/data/tempo" ]] && ok "data directories created under DATA_DIR"

# Shape of the two configs built from deploy/: what the stack relies on.
if command -v python3 >/dev/null && python3 -c 'import yaml' 2>/dev/null; then
  if python3 - "$bin/otelcol.yaml" "$bin/tempo.yaml" <<'PY'
import sys, yaml
col = yaml.safe_load(open(sys.argv[1]))
pipes = col["service"]["pipelines"]
assert pipes["metrics"]["exporters"] == ["otlphttp/prometheus"], pipes
assert "otlp/backend" in pipes["traces"]["exporters"], pipes
assert "otlphttp/prometheus" in col["exporters"], col["exporters"]
assert "health_check" in col["service"]["extensions"]
t = yaml.safe_load(open(sys.argv[2]))
assert t["storage"]["trace"]["local"]["path"] == "/var/tempo", t["storage"]
assert t["compactor"]["compaction"]["block_retention"] == "504h", t["compactor"]
assert "otlp" in t["distributor"]["receivers"], t["distributor"]
PY
  then
    ok "collector has its metrics pipeline; Tempo has its storage, receiver and 504h retention"
  else
    bad "collector or Tempo config is not the shape the stack relies on"
  fi
else
  if grep -q '^    metrics:' "$bin/otelcol.yaml" && grep -q 'block_retention: 504h' "$bin/tempo.yaml" &&
    grep -q 'path: /var/tempo' "$bin/tempo.yaml"; then
    ok "collector has its metrics pipeline; Tempo has its storage and 504h retention (grep only)"
  else
    bad "collector or Tempo config is not the shape the stack relies on"
  fi
  echo "  skip  full YAML shape check (python3 yaml module not installed)"
fi

# ---- re-runs restart exactly the services whose config changed ---------------

up host.docker.internal:9091
if [[ "$(cat "$log")" == "up -d" ]]; then
  ok "re-run with nothing changed restarts nothing"
else
  bad "re-run with nothing changed called more than 'up -d'"
  indent "$log"
fi

up host.docker.internal:9292
if grep -qx 'restart prometheus-scrape' "$log"; then
  ok "a new SCRAPE_TARGET restarts prometheus-scrape only"
else
  bad "a new SCRAPE_TARGET did not restart prometheus-scrape alone"
  indent "$log"
fi

echo "# changed upstream" >>"$tmp/test/e2e-dashboard/prometheus-otlp.yml"
up host.docker.internal:9292
if grep -qx 'restart prometheus-otlp' "$log"; then
  ok "a change to e2e-dashboard's OTLP config restarts prometheus-otlp only"
else
  bad "a change to e2e-dashboard's OTLP config did not restart prometheus-otlp alone"
  indent "$log"
fi

for t in host.docker.internal:65535 '[fd00::12]:9091'; do
  if up "$t"; then ok "accepts SCRAPE_TARGET='$t'"; else bad "refused SCRAPE_TARGET='$t'"; indent "$tmp/out"; fi
done

# ---- the compactor guard -----------------------------------------------------

printf 'compactor:\n  compaction:\n    block_retention: 1h\n' >>"$tmp/deploy/otel-collector/docker/tempo.yaml"
if ! up host.docker.internal:9091 && grep -q 'own compactor: block' "$tmp/out" && [[ ! -s "$log" ]]; then
  ok "refuses to append retention to a Tempo config that has its own compactor block"
else
  bad "did not refuse a Tempo config with its own compactor block"
  indent "$tmp/out"
fi

report
