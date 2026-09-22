#!/usr/bin/env python3
"""Unit tests for verify-panels.py's pure parsing/classification logic —
load_targets, load_template_vars, substitute_variables, expects_empty.

No live Prometheus/Grafana needed: these are exactly the functions that
decide pass/fail for the whole nightly suite, and until this file existed
they were only ever exercised indirectly by a live nightly run against real
infra — slow feedback, and a bug here (like the mixed-metric misclassification
this file specifically guards against) could only ever surface as a red
run against real infra, easy to misattribute to the environment rather than
the checker.

Run: python3 test_verify_panels.py -v
"""
import importlib.util
import json
import os
import tempfile
import unittest

_SPEC = importlib.util.spec_from_file_location(
    "verify_panels", os.path.join(os.path.dirname(__file__), "verify-panels.py")
)
vp = importlib.util.module_from_spec(_SPEC)
_SPEC.loader.exec_module(vp)


def write_dashboard(test_case, panels=None, templating=None):
    """Writes a synthetic dashboard JSON to a temp file and returns its path.
    Takes the calling TestCase so cleanup can be registered via
    addCleanup — NamedTemporaryFile with delete=False needs an explicit
    unlink or every test run leaves its file behind in the system temp
    directory permanently, which piles up fast on a long-lived checkout or
    a persistent/self-hosted CI runner."""
    doc = {"panels": panels or [], "templating": {"list": templating or []}}
    f = tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False)
    json.dump(doc, f)
    f.close()
    test_case.addCleanup(os.unlink, f.name)
    return f.name


class TestLoadTargets(unittest.TestCase):
    def test_flat_panels(self):
        path = write_dashboard(
            self,
            panels=[
                {"title": "A", "targets": [{"expr": "mcp_build_info"}]},
                {"title": "B", "targets": [{"expr": "go_goroutines"}, {"expr": "process_cpu_seconds_total"}]},
            ]
        )
        targets = vp.load_targets(path)
        self.assertEqual(
            targets,
            [("A", "mcp_build_info"), ("B", "go_goroutines"), ("B", "process_cpu_seconds_total")],
        )

    def test_row_nested_panels(self):
        """A collapsed row nests its panels under its own "panels" array —
        the exact shape a real Grafana row uses, and the reason load_targets
        recurses instead of only reading the top-level list."""
        path = write_dashboard(
            self,
            panels=[
                {"title": "row 1", "type": "row"},
                {
                    "title": "collapsed row",
                    "type": "row",
                    "panels": [{"title": "nested panel", "targets": [{"expr": "mcp_tool_invocation_total"}]}],
                },
            ]
        )
        targets = vp.load_targets(path)
        self.assertEqual(targets, [("nested panel", "mcp_tool_invocation_total")])

    def test_panel_with_no_targets_or_empty_expr_contributes_nothing(self):
        path = write_dashboard(
            self,
            panels=[
                {"title": "row header, no targets key at all"},
                {"title": "empty expr", "targets": [{"expr": ""}]},
            ]
        )
        self.assertEqual(vp.load_targets(path), [])


class TestLoadTemplateVars(unittest.TestCase):
    def test_recognized_label_values_query(self):
        path = write_dashboard(
            self,
            templating=[{"name": "service_name", "query": "label_values(target_info, job)"}]
        )
        checkable, unrecognized = vp.load_template_vars(path)
        self.assertEqual(checkable, [("service_name", "target_info", "job")])
        self.assertEqual(unrecognized, [])

    def test_unrecognized_query_form_is_reported_not_silently_dropped(self):
        """A variable whose query isn't label_values(...) must come back in
        `unrecognized`, not vanish — an earlier version of this function
        silently `continue`d past anything that didn't match, which means a
        future variable in some other form would never be checked at all."""
        path = write_dashboard(
            self,
            templating=[
                {"name": "weird_var", "query": "query_result(some_other_function())"},
                {"name": "good_var", "query": "label_values(mcp_tool_invocation_total, broker)"},
            ]
        )
        checkable, unrecognized = vp.load_template_vars(path)
        self.assertEqual(checkable, [("good_var", "mcp_tool_invocation_total", "broker")])
        self.assertEqual(len(unrecognized), 1)
        self.assertEqual(unrecognized[0][0], "weird_var")


class TestSubstituteVariables(unittest.TestCase):
    def test_all_three_variables_substituted(self):
        expr = 'mcp_tool_invocation_total{broker=~"$broker"} * on (instance, job) target_info{job=~"$service_name", cloud_region=~"$cloud_region"}'
        result = vp.substitute_variables(expr)
        self.assertNotIn("$broker", result)
        self.assertNotIn("$service_name", result)
        self.assertNotIn("$cloud_region", result)
        self.assertIn('broker=~".*"', result)


class TestExpectsEmpty(unittest.TestCase):
    """The mixed-metric misclassification this class exists to guard
    against: a panel combining a go_*/process_* metric with a real mcp_*
    metric must NOT be classified as want-empty, or a regression in the
    mcp_* half would be silently masked as "correctly empty" instead of
    failing — exactly what this suite exists to prevent."""

    def test_no_empty_prefixes_configured_means_nothing_expects_empty(self):
        self.assertFalse(vp.expects_empty("go_goroutines", []))

    def test_pure_go_metric_expects_empty(self):
        self.assertTrue(vp.expects_empty("go_goroutines", ["go_", "process_"]))

    def test_pure_go_metric_joined_to_target_info_still_expects_empty(self):
        """target_info is the join partner present on nearly every real
        panel (the `* on (...) group_left(...) target_info{...}` pattern) —
        it must not itself count as a "real" metric for this check, or
        every actual Go-runtime panel (which all carry this join) would be
        wrongly reclassified as NOT want-empty."""
        expr = 'go_goroutines * on (instance, job) group_left(cloud_region) target_info{job=~".*", cloud_region=~".*"}'
        self.assertTrue(vp.expects_empty(expr, ["go_", "process_"]))

    def test_real_mcp_metric_never_expects_empty(self):
        self.assertFalse(vp.expects_empty("mcp_build_info", ["go_", "process_"]))

    def test_mixed_go_and_mcp_metric_does_not_expect_empty(self):
        """The regression case: a hypothetical future panel referencing
        both a Go-runtime metric and a real mcp_* metric in one expression
        must require real data, not be silently exempted."""
        expr = "mcp_tool_invocation_total + go_goroutines"
        self.assertFalse(vp.expects_empty(expr, ["go_", "process_"]))


if __name__ == "__main__":
    unittest.main()
