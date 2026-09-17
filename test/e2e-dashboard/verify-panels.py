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

KEEP IN SYNC: METRIC_NAME_RE and the label_values(...) regex in
load_template_vars are hand-mirrored from metricNameRe/labelValuesRe in
cmd/server/grafana_dashboard_test.go — two independent parsers for the same
dashboard-JSON shape, in two languages, deliberately (a shared JSON-fixture
pipeline was considered and is a bigger change than this script's own scope
justifies today; see the PR review this note responds to). A future edit to
either file's parsing rules — a new template-variable form, a new metric-name
family — must be mirrored in the other, or the two suites can silently
disagree about what a "supported" panel or variable looks like.
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
    with open(dashboard_path) as f:
        doc = json.load(f)
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


LABEL_VALUES_RE = re.compile(r"^label_values\(\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*,\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\)$")


def load_template_vars(dashboard_path: str):
    """Returns (checkable, unrecognized): checkable is [(name, metric,
    label), ...] for every label_values(metric, label) template variable
    query in the dashboard; unrecognized is [(name, query), ...] for any
    variable whose query this script does not know how to translate into a
    direct Prometheus API call.

    A variable landing in `unrecognized` must not be silently dropped: an
    earlier version of this function did exactly that (`continue`d past
    anything that didn't match), which means a future variable added in some
    other form would never be checked at all, and this suite would stay
    green while that variable's real-world resolution was never verified —
    the caller decides what to do with `unrecognized`, but "nothing" isn't
    an option."""
    with open(dashboard_path) as f:
        doc = json.load(f)
    checkable, unrecognized = [], []
    for v in doc.get("templating", {}).get("list", []):
        query = v.get("query", "").strip()
        m = LABEL_VALUES_RE.match(query)
        if m:
            checkable.append((v["name"], m.group(1), m.group(2)))
        else:
            unrecognized.append((v["name"], query))
    return checkable, unrecognized


def http_get_json(url: str):
    """Fetches url and returns the parsed JSON body, after checking
    Prometheus's own top-level "status" field. Every Prometheus HTTP API
    response carries status: "success" or "error" — without checking it, a
    malformed query (e.g. a PromQL syntax error from a bad substitution)
    returns {"status":"error",...} with no "data.result", which this script
    would otherwise report as the misleading "expected non-empty data, got
    none" instead of surfacing the real problem: the query itself was
    rejected."""
    with urllib.request.urlopen(url, timeout=15) as resp:
        result = json.loads(resp.read().decode())
    if result.get("status") != "success":
        raise RuntimeError(
            f"Prometheus returned status={result.get('status')!r}: "
            f"{result.get('error', '<no error field>')} (errorType={result.get('errorType')}) — url: {url}"
        )
    return result


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
    """True only when EVERY real metric name in expr matches an empty
    prefix — not when ANY does. A panel is only "structurally empty on this
    path" if it measures nothing but go_*/process_* metrics; a panel that
    mixed one of those with a real mcp_* metric (a future ratio/combination
    panel, say) must still be required to return data for its mcp_* half,
    or a regression there would be silently masked as "correctly empty" —
    exactly the kind of silent-pass this suite exists to prevent.

    target_info is excluded from the check: it's the join partner present on
    nearly every panel via the `* on (...) group_left(...) target_info{...}`
    pattern (see docs/observability.md, Resource Attributes), not a metric
    being measured, so its presence must not affect the classification
    either way."""
    if not empty_prefixes:
        return False
    names = [n for n in METRIC_NAME_RE.findall(expr) if n != "target_info"]
    if not names:
        return False
    return all(any(n.startswith(p) for p in empty_prefixes) for n in names)


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
        checkable_vars, unrecognized_vars = load_template_vars(args.dashboard)
        for name, query in unrecognized_vars:
            failures.append(
                f'variable "${name}": query {query!r} is not a label_values(metric, label) call — '
                f"this script only knows how to check that form; extend load_template_vars rather "
                f"than leaving a new form silently unchecked"
            )
        for name, metric, label in checkable_vars:
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
