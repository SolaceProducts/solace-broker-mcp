#!/usr/bin/env bash
#
# Single source of truth for "production surface" — the paths whose change
# requires a CHANGELOG [Unreleased] entry. Meant to be *sourced*, not executed.
#
# Consumers (keep this the only copy of the pattern):
#   - .github/scripts/changelog-check.sh    (CI advisory gate)
#   - .claude/hooks/changelog-reminder.sh   (PreToolUse reminder)
#   - .claude/skills/cut-release/SKILL.md    (Phase A2 gap detection — sources this file)
#
# Go test files are excluded via SURFACE_TEST_EXCLUDE — a test-only change is not
# user- or operator-visible. Usage: grep -E "$SURFACE_RE" | grep -v "$SURFACE_TEST_EXCLUDE"
SURFACE_RE='^(internal/config/|internal/tools/|internal/composite/definitions/tools\.yaml)'
SURFACE_TEST_EXCLUDE='_test\.go$'
