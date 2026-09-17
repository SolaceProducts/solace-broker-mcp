#!/usr/bin/env python3
"""Drives every panel query (and, optionally, every template variable and an
exemplar check) from the real deploy/grafana/solace-broker-mcp-overview.json
straight at a Prometheus instance's HTTP API, and asserts each returns data
or not, per --expect-empty-prefixes.

Deliberately reads the dashboard JSON at run time rather than hardcoding a
panel list (SOL-154545's own requirement): Stories 17/18/28/29 landing later
and adding new panels are covered for free, with zero changes here.

Not a PromQL parser — a regex over each target's `expr` string, exactly like
cmd/server/grafana_dashboard_test.go's approach, proportionate to what this
script needs (does the panel return data?), not full query analysis.
"""
import argparse
import json
import re
import sys
import time
import urllib.parse
import urllib.request

# Grafana template variables as they appear literally inside a panel's expr
# string — substituted with a wildcard regex match so a raw PromQL query
# against Prometheus (which has no concept of a Grafana variable) still
# selects real data instead of matching nothing or erroring.
VARIABLE_SUBSTITUTIONS = {
    "$service_name": ".*",
    "$cloud_region": ".*",
    "$broker": ".*",
}

METRIC_NAME_RE = re.compile(
    r"\b(?:mcp_[a-zA-Z0-9_]*|go_[a-zA-Z0-9_]*|process_[a-zA-Z0-9_]*|target_info)\b"
)


def substitute_variables(expr: str) -> str:
    for var, replacement in VARIABLE_SUBSTITUTIONS.items():
        expr = expr.replace(var, replacement)
    return expr


def load_targets(dashboard_path: str):
    """Returns [(panel_title, expr), ...], recursing into row-nested panels —
    the same shape cmd/server/grafana_dashboard_test.go's loadDashboardTargets
    walks, for the same reason: a collapsed row nests its panels under its
    own "panels" array."""
    doc = json.load(open(dashboard_path))
    out = []

    def walk(panels):
        for p in panels:
            for tgt in p.get("targets", []):
                if tgt.get("expr"):
                    out.append((p.get("title", "<untitled>"), tgt["expr"]))
            if p.get("panels"):
                walk(p["panels"])

    walk(doc.get("panels", []))
    return out


def load_template_vars(dashboard_path: str):
    """Returns [(name, metric, label), ...] for every label_values(metric,
    label) template variable query in the dashboard."""
    doc = json.load(open(dashboard_path))
    label_values_re = re.compile(r"^label_values\(\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$")
    out = []
    for v in doc.get("templating", {}).get("list", []):
        m = label_values_re.match(v.get("query", "").strip())
        if not m:
            continue
        out.append((v["name"], m.group(1), m.group(2)))
    return out


def http_get_json(url: str):
    with urllib.request.urlopen(url, timeout=15) as resp:
        return json.loads(resp.read().decode())


def prometheus_query(base_url: str, query: str):
    url = f"{base_url}/api/v1/query?" + urllib.parse.urlencode({"query": query})
    return http_get_json(url)


def prometheus_label_values(base_url: str, label: str, match_metric: str):
    url = f"{base_url}/api/v1/label/{urllib.parse.quote(label)}/values?" + urllib.parse.urlencode(
        {"match[]": match_metric}
    )
    return http_get_json(url)


def prometheus_query_exemplars(base_url: str, query: str, lookback_seconds: int = 600):
    now = time.time()
    params = {"query": query, "start": now - lookback_seconds, "end": now}
    url = f"{base_url}/api/v1/query_exemplars?" + urllib.parse.urlencode(params)
    return http_get_json(url)


def expects_empty(expr: str, empty_prefixes) -> bool:
    if not empty_prefixes:
        return False
    names = METRIC_NAME_RE.findall(expr)
    return bool(names) and any(any(n.startswith(p) for p in empty_prefixes) for n in names)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--prometheus-url", required=True, help="e.g. http://localhost:9092")
    ap.add_argument(
        "--dashboard",
        default="../../deploy/grafana/solace-broker-mcp-overview.json",
        help="path to the dashboard JSON, relative to this script",
    )
    ap.add_argument(
        "--expect-empty-prefixes",
        default="",
        help="comma-separated metric name prefixes expected to return NO data on this "
        "Prometheus (e.g. go_,process_ on the OTLP path)",
    )
    ap.add_argument(
        "--check-variables",
        action="store_true",
        help="also assert every template variable resolves to a non-empty set",
    )
    ap.add_argument(
        "--check-exemplar",
        action="store_true",
        help="also assert at least one exemplar with a trace_id is present in the scrape",
    )
    args = ap.parse_args()

    empty_prefixes = [p for p in args.expect_empty_prefixes.split(",") if p]

    failures = []

    targets = load_targets(args.dashboard)
    if not targets:
        print("FAIL: no panel targets found in the dashboard JSON — path or shape assumption is wrong", file=sys.stderr)
        return 1

    for title, expr in targets:
        query = substitute_variables(expr)
        want_empty = expects_empty(expr, empty_prefixes)
        try:
            result = prometheus_query(args.prometheus_url, query)
        except Exception as exc:  # noqa: BLE001 - report and continue, one failure per panel
            failures.append(f'panel "{title}": query failed: {exc} (query: {query!r})')
            continue

        series = result.get("data", {}).get("result", [])
        if want_empty:
            if series:
                failures.append(
                    f'panel "{title}": expected NO data on this path (go_*/process_* are scrape-only, '
                    f"structurally absent from the OTLP egress — see docs/observability.md, "
                    f'"Go Runtime and Process Metrics"), but got {len(series)} series (query: {query!r})'
                )
            else:
                print(f'PASS: panel "{title}" correctly empty on this path')
        else:
            if not series:
                failures.append(f'panel "{title}": expected non-empty data, got none (query: {query!r})')
            else:
                print(f'PASS: panel "{title}" returned {len(series)} series')

    if args.check_variables:
        for name, metric, label in load_template_vars(args.dashboard):
            try:
                result = prometheus_label_values(args.prometheus_url, label, metric)
            except Exception as exc:  # noqa: BLE001
                failures.append(f'variable "${name}": label_values({metric}, {label}) lookup failed: {exc}')
                continue
            values = result.get("data", [])
            if not values:
                failures.append(
                    f'variable "${name}": label_values({metric}, {label}) resolved to an empty set — '
                    f"its Grafana dropdown would be empty too"
                )
            else:
                print(f'PASS: variable "${name}" resolved to {len(values)} value(s): {values}')

    if args.check_exemplar:
        found = False
        checked = []
        for metric in ("mcp_tool_invocation_duration_seconds_bucket", "mcp_semp_request_duration_seconds_bucket"):
            checked.append(metric)
            try:
                result = prometheus_query_exemplars(args.prometheus_url, metric)
            except Exception as exc:  # noqa: BLE001
                failures.append(f"exemplar check: query_exemplars({metric}) failed: {exc}")
                continue
            for series in result.get("data", []):
                for ex in series.get("exemplars", []):
                    if ex.get("labels", {}).get("trace_id"):
                        found = True
                        break
                if found:
                    break
            if found:
                break
        if found:
            print(f"PASS: at least one exemplar with a trace_id is present ({checked})")
        else:
            failures.append(
                f"exemplar check: no exemplar with a trace_id found across {checked} — "
                f"confirm OBS_TRACING_ENABLED, the sampler, and "
                f"--enable-feature=exemplar-storage are all actually on"
            )

    if failures:
        print("", file=sys.stderr)
        print(f"=== {len(failures)} FAILURE(S) against {args.prometheus_url} ===", file=sys.stderr)
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1

    print("")
    print(f"=== all checks passed against {args.prometheus_url} ===")
    return 0


if __name__ == "__main__":
    sys.exit(main())
