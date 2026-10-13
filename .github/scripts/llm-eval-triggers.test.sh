#!/usr/bin/env bash
#
# Guards the trigger and concurrency wiring of llm-eval.yml (SOL-155075).
#
# The nightly run and the release-tag run must never share a concurrency group:
# GitHub keeps one running and one pending run per group, and a third arrival
# cancels the pending one, which could be the release gate. This test reads the
# real workflow file, evaluates its `group:` and job `if:` for each event, and
# asserts the release path is unchanged and the nightly path is separate.
#
# It evaluates the real expression text, not a copy. It understands only the
# `github.event_name`, `github.sha` and `github.repository` names with `==`,
# `!=`, `&&` and `||`. Any other expression fails loudly rather than passing.
#
# An optional first argument points at another workflow file. That is how the
# mutation check feeds it a deliberately broken copy.
#
# Run manually:  .github/scripts/llm-eval-triggers.test.sh [workflow-file]
# Runs in CI as the `llm_eval_triggers` job in .github/workflows/ci-pr.yaml.
#
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd "${SCRIPT_DIR}/../.." && pwd)
WORKFLOW="${1:-${REPO_ROOT}/.github/workflows/llm-eval.yml}"

python3 -I -c 'import yaml' 2>/dev/null || {
  echo "llm-eval-triggers.test.sh: python3 with PyYAML is required" >&2
  exit 2
}

exec python3 -I - "$WORKFLOW" "$REPO_ROOT" <<'PYEOF'
import pathlib
import re
import sys

import yaml

wf_path = sys.argv[1]
root = pathlib.Path(sys.argv[2])

SHA = "0123456789abcdef0123456789abcdef01234567"
REPO = "SolaceProducts/solace-broker-mcp"
FORK = "someone-else/solace-broker-mcp"

passed = 0
failed = 0


def ok(msg):
    global passed
    passed += 1
    print(f"ok   - {msg}")


def bad(msg, why=""):
    global failed
    failed += 1
    print(f"FAIL - {msg}")
    if why:
        print(f"       {why}")


class Unsupported(Exception):
    pass


TOKEN = re.compile(r"\s*(?:('[^']*')|(github\.[a-z_]+)|(==|!=|&&|\|\|))")


def evaluate(expr, ctx):
    names = {
        "github.event_name": ctx["event_name"],
        "github.sha": ctx["sha"],
        "github.repository": ctx["repository"],
    }
    pos = 0
    out = []
    while expr[pos:].strip():
        m = TOKEN.match(expr, pos)
        if not m:
            raise Unsupported(f"unsupported expression text: {expr[pos:].strip()!r}")
        pos = m.end()
        if m.group(1):
            out.append(repr(m.group(1)[1:-1]))
        elif m.group(2):
            if m.group(2) not in names:
                raise Unsupported(f"unsupported name: {m.group(2)}")
            out.append(repr(names[m.group(2)]))
        else:
            out.append({"&&": "and", "||": "or"}.get(m.group(3), m.group(3)))
    return eval(" ".join(out), {"__builtins__": {}}, {})


def resolve(value, ctx):
    parts = re.split(r"\$\{\{(.*?)\}\}", str(value))
    return "".join(
        p if i % 2 == 0 else str(evaluate(p.strip(), ctx)) for i, p in enumerate(parts)
    )


def truthy(value, ctx):
    text = str(value).strip()
    if text.startswith("${{") and text.endswith("}}"):
        text = text[3:-2]
    return bool(evaluate(text, ctx))


def ctx_for(event, repository=REPO):
    return {"event_name": event, "sha": SHA, "repository": repository}


doc = yaml.safe_load(pathlib.Path(wf_path).read_text())
# PyYAML (YAML 1.1) reads the key `on` as the boolean True.
triggers = doc.get("on", doc.get(True)) or {}
conc = doc.get("concurrency") or {}
group = conc.get("group")
job = (doc.get("jobs") or {}).get("llm-eval") or {}
job_if = job.get("if")

# --- Proof 1: the nightly and the release resolve to different groups ---------
if group is None:
    bad("workflow has a concurrency group")
else:
    try:
        push = resolve(group, ctx_for("push"))
        sched = resolve(group, ctx_for("schedule"))
        disp = resolve(group, ctx_for("workflow_dispatch"))
    except Unsupported as e:
        bad("group expression has a shape this test understands", str(e))
    else:
        if push == f"llm-eval-{SHA}":
            ok("release call path (event push) resolves to llm-eval-<sha>")
        else:
            bad("release call path (event push) resolves to llm-eval-<sha>", f"got {push!r}")
        if disp == f"llm-eval-{SHA}":
            ok("manual dispatch resolves to llm-eval-<sha>")
        else:
            bad("manual dispatch resolves to llm-eval-<sha>", f"got {disp!r}")
        if sched != push:
            ok("nightly group differs from the release group for the same sha")
        else:
            bad(
                "nightly group differs from the release group for the same sha",
                f"both resolve to {push!r}",
            )
        if not re.fullmatch(r"llm-eval-[0-9a-f]{40}", sched):
            ok("nightly group can never equal an llm-eval-<sha> group")
        else:
            bad("nightly group can never equal an llm-eval-<sha> group", f"got {sched!r}")
        # Pinned, not just "different": a nightly put into another workflow's
        # group (for example Release) would pass the checks above. Renaming it
        # should be a deliberate edit here.
        if sched == "llm-eval-nightly":
            ok("nightly group is exactly llm-eval-nightly")
        else:
            bad("nightly group is exactly llm-eval-nightly", f"got {sched!r}")

# --- Proof 2 (guard): the release gate is unchanged ---------------------------
if conc.get("cancel-in-progress") is False:
    ok("cancel-in-progress is false")
else:
    bad("cancel-in-progress is false", f"got {conc.get('cancel-in-progress')!r}")
try:
    release_runs = True if job_if is None else truthy(job_if, ctx_for("push"))
except Unsupported as e:
    bad("job if has a shape this test understands", str(e))
else:
    if release_runs:
        ok("the llm-eval job runs on the release call path (event push)")
    else:
        bad("the llm-eval job runs on the release call path (event push)")
# The group only keeps the release out of the nightly queue while the caller's
# event is never `schedule`. A schedule on release.yml would put every release
# run in llm-eval-nightly.
release_doc = yaml.safe_load((root / ".github/workflows/release.yml").read_text())
release_on = release_doc.get("on", release_doc.get(True)) or {}
release_events = set(release_on) if isinstance(release_on, (dict, list)) else {release_on}
if "schedule" not in release_events:
    ok("release.yml has no schedule trigger")
else:
    bad(
        "release.yml has no schedule trigger",
        "a scheduled release.yml would resolve llm-eval to the nightly group",
    )

# --- Proof 3: a single daily schedule, not run in forks -----------------------
sched_cfg = triggers.get("schedule") if isinstance(triggers, dict) else None
if isinstance(sched_cfg, list) and len(sched_cfg) == 1 and sched_cfg[0].get("cron") == "0 6 * * *":
    ok("exactly one schedule entry, cron 0 6 * * *")
else:
    bad("exactly one schedule entry, cron 0 6 * * *", f"got {sched_cfg!r}")
if job_if is None:
    bad("the llm-eval job has a fork guard")
else:
    try:
        nightly_here = truthy(job_if, ctx_for("schedule", REPO))
        nightly_fork = truthy(job_if, ctx_for("schedule", FORK))
        dispatch_fork = truthy(job_if, ctx_for("workflow_dispatch", FORK))
    except Unsupported as e:
        bad("job if has a shape this test understands", str(e))
    else:
        if nightly_here and not nightly_fork and dispatch_fork:
            ok("nightly runs in this repo, not in forks; dispatch still works in forks")
        else:
            bad(
                "nightly runs in this repo, not in forks; dispatch still works in forks",
                f"here={nightly_here} fork={nightly_fork} dispatch_in_fork={dispatch_fork}",
            )

# --- Proof 4 (guard): manual dispatch defaults are unchanged ------------------
inputs = ((triggers.get("workflow_dispatch") or {}).get("inputs") or {}) if isinstance(triggers, dict) else {}
want = {"target_branch": "main", "broker_target": "local-docker", "scenario_filter": ""}
got = {k: (inputs.get(k) or {}).get("default") for k in want}
if got == want:
    ok("workflow_dispatch defaults are main, local-docker, empty filter")
else:
    bad("workflow_dispatch defaults are main, local-docker, empty filter", f"got {got!r}")

# --- Proof 5: the old tag-only story is gone from the operator surfaces -------
def flat(rel):
    text = (root / rel).read_text()
    text = re.sub(r"\n\s*(#\s*)?", " ", text).lower()
    return re.sub(r"\s+", " ", text)


for rel, phrase in (
    (".github/workflows/llm-eval.yml", "no schedule"),
    ("test/e2e-llm/README.md", "no daily run"),
    ("test/e2e-monitoring/README.md", "no daily run"),
):
    try:
        found = phrase in flat(rel)
    except FileNotFoundError:
        bad(f"{rel} exists", "file not found")
        continue
    if found:
        bad(f'{rel} no longer says "{phrase}"')
    else:
        ok(f'{rel} no longer says "{phrase}"')

print(f"{passed} passed, {failed} failed")
sys.exit(1 if failed else 0)
PYEOF
