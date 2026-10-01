# obs-stack — observability backends for the all-flags-on soaks

The receiving end for a soak run with every observability flag on
(SOL-154041, SOL-155021). Without it nothing receives OTLP or scrapes
`/metrics`, and "all flags on" only exercises failing exports. It runs on its
own box (Box C) so the load generator's measurements and disk guard on Box A
stay clean. The rig scripts that provision Box C and start this stack live in
`SolaceDev/dax` (`broker-mcp-perf-cloud-env`, SOL-155016).

| service | image | host port | role |
|---|---|---|---|
| `prometheus-scrape` | `prom/prometheus:v3.0.1` | `127.0.0.1:9092` | scrapes the server's `/metrics` every 5s |
| `prometheus-otlp` | `prom/prometheus:v3.0.1` | `127.0.0.1:9093` | receives OTLP metrics through the collector |
| `otel-collector` | `otel/opentelemetry-collector-contrib:0.111.0` | `${OTLP_BIND_ADDR}:4317`, `127.0.0.1:13133` (health), `127.0.0.1:8888` (self-metrics) | receives the server's OTLP metrics and traces |
| `tempo` | `grafana/tempo:2.6.0` | `127.0.0.1:3200` | stores traces |

The two Prometheus instances and the collector are wired as in
`test/e2e-dashboard/`, without Grafana; Tempo, which that suite does not run,
comes from `deploy/otel-collector/docker/`. For dashboards, run Grafana
locally and tunnel to the two Prometheus ports.

Nothing here copies a committed config. The stack runs from a repo checkout
and uses the originals:

| config | source |
|---|---|
| collector | `deploy/otel-collector/docker/otelcol.yaml`, metrics pipeline uncommented by `../../e2e-dashboard/uncomment-metrics-pipeline.sh` |
| Tempo | `deploy/otel-collector/docker/tempo.yaml` plus `tempo-retention.yaml` |
| OTLP Prometheus | `../../e2e-dashboard/prometheus-otlp.yml`, mounted as is |
| scrape Prometheus | `prometheus-scrape.yml.tmpl`, with `SCRAPE_TARGET` filled in |

## Starting it

```
SCRAPE_TARGET=<server host>:9091 ./up.sh
```

| variable | required | default | on the rig |
|---|---|---|---|
| `SCRAPE_TARGET` | yes | none | `<Box B private IP>:9091` |
| `OTLP_BIND_ADDR` | no | `127.0.0.1` | Box C's private IP |
| `DATA_DIR` | no | `./data` | `/srv/obs` |
| `COMPOSE` | no | `docker compose` | (default) |

`up.sh` is the only entry point. It writes the three generated configs into
`bin/`: the scrape config (Prometheus does not expand environment variables in
`static_configs`), the collector config and the Tempo config. It then runs
`compose up -d`. It refuses to start if the committed Tempo config ever grows
its own `compactor:` block, since appending the retention block would then
duplicate the key. Re-running it is safe: a service whose rendered config
changed (a new `SCRAPE_TARGET`, say) is restarted to pick it up.

No rig address is committed here: the addresses come in through the
environment.

**Data directories.** `${DATA_DIR}/prometheus-scrape`, `prometheus-otlp` and
`tempo`. Prometheus runs as `nobody` (65534) and Tempo as `10001`, so on a
Linux host the directories must be owned by those users, or the services
crash-loop behind `restart: unless-stopped`. The Box C setup step sets the
owners. `up.sh` creates missing directories and, on Linux, warns with the
`chown` to run for any it cannot see the right owner on. Docker Desktop and
podman on a Mac do not enforce this.

**Collector self-metrics.** The committed collector config publishes none, and
the image has no shell to query it from inside. The compose file adds
`--set=service.telemetry.metrics.address=0.0.0.0:8888`. The collector logs a
deprecation warning for `address` (in favour of `readers`); it is honoured on
the pinned 0.111.0. The counters the soak reads:

```
curl -s 127.0.0.1:8888/metrics | grep -E '^otelcol_(receiver_(accepted|refused)|exporter_send_failed)_(spans|metric_points)'
```

## Retention and disk

Sized for Box C's 200 GB volume, with about 20 GB left for images, container
logs and the OS:

| store | bound | budget |
|---|---|---|
| each Prometheus | `retention.time=30d`, `retention.size=25GB` | 25 GB each |
| Tempo | `block_retention: 504h` (21 days) | about 130 GB |

Both Prometheus bounds are set because a size limit on its own removes the
15-day time default. Tempo's default `block_retention` is 336h, exactly the
14-day run, so day-1 traces would expire around collect time.

Tempo has no size limit, so **the trace sampling ratio is what bounds its
disk**. The server's default sampler is `parentbased_always_on`. Measured
locally (SOL-155015) with the default tool mix: about 8 spans per call and
about 110 bytes per span on Tempo's disk, so about 0.9 KB per call. At
2,500 calls/s that is about 190 GB/day unsampled, the whole volume in a day.

To stay inside about 6 GB/day (130 GB over 21 days):

```
ratio ≈ 6 GB/day ÷ (calls/s × spans per call × bytes per span × 86,400)
```

At 2,500 calls/s this gives about 0.03. The soak starts at
`OTEL_TRACES_SAMPLER=parentbased_traceidratio`, `OTEL_TRACES_SAMPLER_ARG=0.01`,
which is about 2 GB/day. The 3-day run confirms the value from Tempo's
measured daily growth, and the soak's Box C disk warning is the backstop.
Prometheus is small by comparison: a few hundred MB/day per instance at a 5s
interval (a rough local reading).

## Local run

Start a server with the observability flags on. Its metrics listener
defaults to `:9091`.

```
OBS_METRICS_SCRAPE_ENABLED=true OBS_METRICS_OTLP_ENABLED=true OBS_TRACING_ENABLED=true \
OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:4317 OTEL_EXPORTER_OTLP_INSECURE=true \
OTEL_METRIC_EXPORT_INTERVAL=5000 <start the server>

SCRAPE_TARGET=host.docker.internal:9091 ./up.sh        # or COMPOSE="podman compose"
```

`prometheus-scrape` maps `host.docker.internal` to the host on Linux as well
as Mac. On Linux with rootful Docker, `chown` the data directories as `up.sh`
suggests on the first run (see Data directories). Readiness: `127.0.0.1:9092/-/ready`, `127.0.0.1:9093/-/ready`,
`127.0.0.1:3200/ready` (Tempo takes about 15s after start), and
`127.0.0.1:13133/` for the collector.

Stop with `docker compose down` from this directory. `./data` survives
that; delete it to start clean.
