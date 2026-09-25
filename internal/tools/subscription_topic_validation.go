// Copyright 2024-2026 Solace Corporation. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tools

import (
	"fmt"
	"html"
	"regexp"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// htmlEntityValidatedTools names every composite tool whose value-bearing
// parameter is a raw Solace topic string an LLM caller sometimes HTML-escapes
// when constructing the tool call (SOL-154049, reported against SOL-153868's
// create-queue-subscription e2e-llm scenario: claude-sonnet-5 occasionally
// sent "ABC/&gt;" instead of the literal "ABC/>" the user actually asked
// for). The tool does exactly what it's told, so the broker ends up with a
// real subscription on the wrong, escaped topic — no error, no signal, just
// a silently wrong result.
//
// delete-queue-subscription is deliberately NOT in this map, even though its
// subscriptionTopic has the identical shape and risk on paper. Filed in
// review (PR #446): guarding delete removes the only non-destructive way to
// clean up the exact wrong subscriptions this bug creates — list-queue-
// subscriptions can show one, but delete-queue-subscription would refuse to
// remove it, and delete-queue (destroying the whole queue and its spooled
// messages) is the only other MCP-level lever. Worse, the guard's own advice
// ("pass the literal character and call this tool again") is actively wrong
// for delete: retrying with the literal topic either hits SEMP's NOT_FOUND
// for a subscription that was never created — which classifyDesiredStateOutcome
// (errors.go) reports as outcome:"already_absent", i.e. success, while the
// real, wrongly-escaped subscription is still there — or, if the literal
// subscription does exist, deletes that correct one instead of the wrong one
// the caller actually meant to remove. "Over-reject is safer" holds for
// create (nothing exists yet to protect); it does not hold for delete, where
// the reject prevents no harm and only removes the cleanup path.
//
// A manually-curated map, not derived from the YAML definitions, matching
// writeToolIdentifierFields' (composite_handler.go) and ownerValidatedTools'
// (owner_validation.go, SOL-153080) precedent: "this parameter holds a raw
// topic string" isn't a fact the composite tool definition exposes
// structurally.
var htmlEntityValidatedTools = map[string]string{
	"create-queue-subscription": "subscriptionTopic",
}

// htmlEntityNamedPattern matches the named-entity form of an HTML/XML entity
// encoding one of four characters ('&', '<', '>', '"') — the ones a generic
// escaper converts and HTML5's legacy SGML-compatibility table still decodes
// without a trailing ';', even mid-word (html.UnescapeString("&ampersand")
// == "&ersand", confirmed live). Matched in exact lowercase AND exact
// all-uppercase only ("gt"/"GT", never "Gt" or "gT"), because only those two
// casings decode to the intended single character: any other casing either
// decodes to something else entirely (html.UnescapeString("&Gt;") == "≫",
// U+226B, not '>') or doesn't decode at all (html.UnescapeString("&Quot;")
// == "&Quot;", unchanged) — matching those via a blanket case-insensitive
// flag was tried and found wrong: it made htmlEntityTopicError.literal
// actively incorrect for exactly the mixed-case forms above.
//
// apos is deliberately absent from this pattern: unlike the SGML-era four,
// it never decodes without a ';' and has no uppercase form at all (confirmed:
// "&apos" and even "&APOS;" both fail to decode) — apos is HTML5-only, never
// an SGML legacy entity — so it is matched by its own literal alternative,
// lowercase and ';'-mandatory, with no shared leniency.
var htmlEntityNamedPattern = regexp.MustCompile(`&(?:(?:amp|lt|gt|quot|AMP|LT|GT|QUOT);?|apos;)`)

// htmlEntityNumericPattern matches a decimal or hex numeric character
// reference for one of the five target characters ('&','<','>','"',
// apostrophe: codepoints 38,60,62,34,39 / 0x26,0x3c,0x3e,0x22,0x27), with
// capturing group 1 holding the decimal form and group 2 the hex form.
//
// Unlike htmlEntityNamedPattern, the trailing ';' here is NOT simply made
// optional — a numeric reference greedily extends over further digits into a
// different codepoint entirely (html.UnescapeString("&#621;") == "ɭ", a real
// single Unicode character, not "&#62;" ('>') plus a stray "1"; the same
// holds for hex: "&#x3e5;" == "ϥ", not '>' plus "5"). RE2 (this package's
// regexp engine) has no lookahead to assert that directly, so each
// alternative instead matches its own terminator — a ';', a character that
// cannot continue the reference (a non-digit for decimal, a non-hex-digit
// for hex), or end-of-string — and the terminator is asserted, not consumed,
// by using two capturing groups and reading `matched` from whichever one is
// non-empty, rather than from the whole match FindString would return: a
// literal terminator character would otherwise end up inside `matched`,
// corrupting the html.UnescapeString(matched) computation of
// htmlEntityTopicError.literal (e.g. "&#62X" would report literal ">X"
// instead of ">"). Verified against html.UnescapeString for every shape this
// produces, including the no-semicolon and truncated-prefix cases, in
// TestFindHTMLEntity_DetectsEveryRealEscapingShape.
//
// One deliberate, accepted imprecision from this: `matched` for a numeric
// reference never includes a real trailing ';' either, even when the input
// actually had one (the terminator is only asserted, never captured, in
// either case, to keep this one pattern's logic uniform instead of adding a
// third alternative purely to special-case "consume the ';' when present").
// This only affects the cosmetic "found in your input" text in
// htmlEntityTopicError's message, e.g. reporting "&#62" rather than "&#62;"
// for input containing "&#62;" — html.UnescapeString(matched) still decodes
// to the correct literal either way, which is the property that actually
// matters (the wrong-literal bug this whole file exists to avoid, fixed in
// PR #446 review).
var htmlEntityNumericPattern = regexp.MustCompile(
	`(&#0*(?:38|60|62|34|39))(?:;|[^0-9]|$)` +
		`|(&#[xX]0*(?:26|3[cC]|3[eE]|22|27))(?:;|[^0-9a-fA-F]|$)`,
)

// findHTMLEntity returns the leftmost HTML-entity-shaped substring of value
// across both patterns above, or "" if neither matches. Named and numeric
// entities are matched with two separate patterns (and two different
// extraction strategies — see htmlEntityNumericPattern's doc comment for
// why) rather than one combined pattern, so each stays simple enough to
// verify by inspection; findHTMLEntity is what recombines them into a single
// answer.
//
// Between them, the two patterns cover exactly five characters — '&', '<',
// '>', '"', apostrophe — the exact set every common escaper (Go's
// html.EscapeString, Python's html.escape, browser/JS escaping) produces.
// Matching exactly these five, not an arbitrary named-entity list (there are
// hundreds, e.g. &nbsp; &copy;, that no escaper would ever produce from a
// literal '>' or '*'), keeps the false-positive surface as small as this
// heuristic can make it while still catching every real escaping shape.
//
// Known, accepted trade-off: subscriptionTopic has no character restriction
// at the broker (see htmlEntityValidatedTools' doc comment), so a topic that
// legitimately contains a literal substring shaped like "&amp;" or "&#62;"
// — vanishingly unlikely in practice, but not impossible — would also be
// rejected. There is no way to tell that case apart from the escaping bug
// from the string alone; this is the one lever this repo has over model/CLI-
// side argument construction (SOL-154049), and over-rejecting a rare literal
// is safer than silently creating the wrong subscription.
func findHTMLEntity(value string) string {
	namedLoc := htmlEntityNamedPattern.FindStringIndex(value)
	numLoc := htmlEntityNumericPattern.FindStringSubmatchIndex(value)

	var numMatched string
	numStart := -1
	if numLoc != nil {
		numStart = numLoc[0]
		switch {
		case numLoc[2] != -1: // group 1 (decimal) participated
			numMatched = value[numLoc[2]:numLoc[3]]
		case numLoc[4] != -1: // group 2 (hex) participated
			numMatched = value[numLoc[4]:numLoc[5]]
		}
	}

	switch {
	case namedLoc == nil && numStart == -1:
		return ""
	case namedLoc == nil:
		return numMatched
	case numStart == -1:
		return value[namedLoc[0]:namedLoc[1]]
	case namedLoc[0] <= numStart:
		return value[namedLoc[0]:namedLoc[1]]
	default:
		return numMatched
	}
}

// htmlEntityTopicError reports that a tool call's topic-bearing parameter
// contains what looks like an HTML/XML entity rather than the literal
// character it was meant to send. Constructed only by
// validateHTMLEntityTopic, and — like the errors buildLocalErrorResult
// handles elsewhere in this package — always this package's own text, safe
// to echo back to the caller verbatim (SOL-152980): every field it renders
// is either a constant this package owns (tool, param) or derived from the
// caller's own argument by this package's own regex/html.UnescapeString
// call (value, matched, literal), never broker- or intermediary-originated.
type htmlEntityTopicError struct {
	tool    string
	param   string
	value   string
	matched string
	// literal is html.UnescapeString(matched) — the single character matched
	// actually decodes to (e.g. "&gt;" -> ">"), computed once at construction
	// so both this error's own message and the caller-facing correction can
	// say exactly what to send instead, rather than just what was wrong.
	literal string
}

func (e *htmlEntityTopicError) Error() string {
	return fmt.Sprintf(
		"%s %q was rejected: it contains %q, which looks like an HTML-escaped "+
			"%q rather than the literal character. This call was not sent to the "+
			"broker. Pass the literal character %q — do not HTML-escape it — and "+
			"call %s again.",
		e.param, e.value, e.matched, e.literal, e.literal, e.tool)
}

// validateHTMLEntityTopic checks toolName's topic-bearing parameter (per
// htmlEntityValidatedTools) for an HTML-entity-encoded character, returning
// a non-nil *htmlEntityTopicError if one is found. Returns nil for a tool
// not in that map, or when the parameter is absent or not a string — the
// caller (CallTool) has already run schema validation by the time this is
// called, so an absent/non-string value here is unreachable through the real
// schema, not a case this function needs to error on itself.
//
// Called directly from CallTool's local-validation stage, right after schema
// validation and before Handle ever runs (manager.go) — not implemented as a
// ToolHandler-wrapping decorator. This check has no broker round-trip and
// needs none of Handle's machinery; routing it through Handle would have
// meant threading a purely local, deterministic input error through the
// broker-outcome classification in buildErrorMessage/buildErrorResult and
// logToolResult's audited-broker-error-type gate (three switches meant for
// broker failures, not caller mistakes) just to reach the exact code path —
// buildLocalErrorResult — that already exists for this. Filed in review (PR
// #446).
func validateHTMLEntityTopic(toolName string, params map[string]any) *htmlEntityTopicError {
	param, ok := htmlEntityValidatedTools[toolName]
	if !ok {
		return nil
	}
	value, ok := params[param].(string)
	if !ok {
		return nil
	}
	matched := findHTMLEntity(value)
	if matched == "" {
		return nil
	}
	return &htmlEntityTopicError{
		tool:    toolName,
		param:   param,
		value:   value,
		matched: matched,
		literal: html.UnescapeString(matched),
	}
}

// buildHTMLEntityTopicResult builds the CallToolResult for a rejected
// htmlEntityTopicError. Like buildLocalErrorResult, it echoes the error
// verbatim (safe per htmlEntityTopicError's own doc comment) and additionally
// carries structured fields a programmatic caller can key off without
// parsing the message text.
func buildHTMLEntityTopicResult(err *htmlEntityTopicError) *mcp.CallToolResult {
	result := buildLocalErrorResult(err)
	structured := result.StructuredContent.(map[string]any)
	structured["error_source"] = "input_validation"
	structured["parameter"] = err.param
	structured["value"] = err.value
	structured["matchedEntity"] = err.matched
	return result
}
