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

package composite

import (
	"fmt"
	"sort"
	"strings"
)

// Named request-body checks (Step.BodyChecks).
//
// A body check inspects the final merged request body of a config create, update
// or replace step and rejects the call locally, before any SEMP request, with a
// RequestBodyError (shown to the agent verbatim and carried in the log detail).
//
// Why this lives in the executor and not in a tools-layer wrapper such as
// ownerValidatingHandler (internal/tools/owner_validation.go): a wrapper sees only
// the raw tool params, so it has to re-implement constructRequestBody's rules for
// how those params become body fields — object params are spread, scalar params
// are set under their own name, and an undeclared param is not rejected. That
// re-implementation was bypassed three times in review (see extractOwner's doc
// comment), which also says to revisit the question at the executor level once a
// second sensitive field appears. Checking the merged body is shape-independent:
// whatever route a field took into the body, the check sees it.
//
// Errors must name a field, never echo a value: a rejected value may be a secret,
// and that includes values of attributes that are not themselves secret fields
// (a credential pasted into the wrong field is still a credential).

// bodyMode is the kind of write a step performs, derived from its operation ID.
type bodyMode int

const (
	bodyModeCreate bodyMode = iota
	bodyModeUpdate
	// bodyModeReplace is a PUT: the body states the whole object. An omitted
	// attribute is reset to its default (an omitted enabled means false), but a
	// write-only secret that is omitted is kept, not cleared. The broker accepts
	// every attribute a create does, including the write-only ones, so a replace
	// step needs the same secret guard as a create.
	bodyModeReplace
)

// bodyModeFor maps a SEMPv2 operation ID to its mode. Only create*, update* and
// replace* operations carry a request body worth checking; validateBodyChecks
// rejects the others at load time, so the fallthrough here is never reached from a
// loaded tool.
func bodyModeFor(opID string) (bodyMode, bool) {
	switch {
	case strings.HasPrefix(opID, "create"):
		return bodyModeCreate, true
	case strings.HasPrefix(opID, "update"):
		return bodyModeUpdate, true
	case strings.HasPrefix(opID, "replace"):
		return bodyModeReplace, true
	}
	return 0, false
}

// bodyCheck is one registered check. secretFields lists the write-only attribute
// names the check forbids (nil for checks that are not secret guards); the drift
// test uses it to prove every write-only attribute of a guarded operation is
// covered, and that no entry has gone stale against the embedded spec.
type bodyCheck struct {
	fn           func(mode bodyMode, body map[string]any) error
	secretFields []string
}

// kafkaSecretFields are the write-only attributes of MsgVpnKafkaReceiver and
// MsgVpnKafkaSender. TestBodyChecks_SecretFieldsMatchSpec pins this list to the
// embedded spec's "absent from a GET" attributes.
var kafkaSecretFields = []string{
	"authenticationAwsMskIamSecretAccessKey",
	"authenticationBasicPassword",
	"authenticationClientCertContent",
	"authenticationClientCertPassword",
	"authenticationKerberosKeytabContent",
	"authenticationOauthClientSecret",
	"authenticationScramPassword",
}

// bodyChecks is the registry of check names usable in tools.yaml.
var bodyChecks = map[string]bodyCheck{
	"rejectKafkaSecrets": {
		fn:           rejectSecretFields(kafkaSecretFields, "Then ask me to enable the bridge."),
		secretFields: kafkaSecretFields,
	},
	"createDisabledWhenAuthenticated": {
		fn: checkAuthenticatedBridgeIsDisabled,
	},
}

// rejectSecretFields builds a check that rejects any body field whose name equals
// (case-insensitively) one of fields. The error names the canonical field name,
// never the caller's key or value. afterHint is appended as the next step.
func rejectSecretFields(fields []string, afterHint string) func(bodyMode, map[string]any) error {
	sorted := append([]string(nil), fields...)
	sort.Strings(sorted) // deterministic: with several secrets present, always report the same one
	return func(_ bodyMode, body map[string]any) error {
		for _, secret := range sorted {
			for key := range body {
				if strings.EqualFold(key, secret) {
					return requestBodyErrorf(
						"%q is a secret and cannot be sent through this tool. Set it yourself (Broker Manager, CLI, SEMP or Terraform) and never paste it into this chat. %s If you already shared a secret in this conversation, treat it as exposed and rotate it.",
						secret, afterHint)
				}
			}
		}
		return nil
	}
}

// checkAuthenticatedBridgeIsDisabled keeps a bridge that needs a secret from
// being enabled by the same call that selects its authentication scheme, since
// that call cannot carry the secret.
//
//   - create and replace: a body with a non-none authenticationScheme must not set
//     enabled:true. (An omitted enabled is false in both: the create default, and
//     the PUT reset.)
//   - update: a body that sets a non-none authenticationScheme must also set
//     enabled:false, so a scheme switch on a live bridge disables it and takes the
//     out-of-band secret path instead of failing at the broker.
//
// An update that does not mention authenticationScheme is never affected, so the
// plain {"enabled": true} step that enables a bridge after its secret is set passes.
//
// The values are read strictly. The broker would reject a wrong-typed enabled or
// authenticationScheme itself, but a free-form config object lets one through to
// this check, where silently treating it as absent would apply the wrong rule (an
// update with "enabled":"false" would be told to "set enabled:false"). A present
// but wrong-typed value is rejected here with a message that says so.
//
// No message repeats a caller-supplied value; see the package note above.
func checkAuthenticatedBridgeIsDisabled(mode bodyMode, body map[string]any) error {
	rawScheme, hasScheme := body["authenticationScheme"]
	if !hasScheme {
		return nil
	}
	scheme, isString := rawScheme.(string)
	if !isString {
		return requestBodyErrorf("authenticationScheme must be a string (for example \"none\" or \"basic\").")
	}
	if scheme == "" || scheme == "none" {
		return nil
	}
	rawEnabled, hasEnabled := body["enabled"]
	enabled := false
	if hasEnabled {
		var isBool bool
		if enabled, isBool = rawEnabled.(bool); !isBool {
			return requestBodyErrorf("enabled must be true or false (a boolean).")
		}
	}
	switch mode {
	case bodyModeCreate:
		if enabled {
			return requestBodyErrorf(
				"A bridge with a non-none authenticationScheme must be created with enabled:false. Set its secret out of band, then enable it with the update tool.")
		}
	case bodyModeReplace:
		if enabled {
			return requestBodyErrorf(
				"A bridge replaced with a non-none authenticationScheme must set enabled:false (an omitted enabled also means false). Set its secret out of band, then enable it with an update.")
		}
	case bodyModeUpdate:
		if !hasEnabled || enabled {
			return requestBodyErrorf(
				"Changing authenticationScheme to a non-none value must also set enabled:false, so the bridge stays down until its new secret is set out of band. Enable it with a separate update afterwards.")
		}
	}
	return nil
}

// runBodyChecks runs the named checks, in order, against the merged body and
// returns the first rejection. The names were validated at load time; an unknown
// name here is a programming error, reported rather than silently skipped (a
// skipped security check would be worse than a failed call).
func runBodyChecks(names []string, opID string, body map[string]any) error {
	if len(names) == 0 {
		return nil
	}
	mode, ok := bodyModeFor(opID)
	if !ok {
		return fmt.Errorf("body checks configured on operation %q, which is not a create, update or replace", opID)
	}
	for _, name := range names {
		check, ok := bodyChecks[name]
		if !ok {
			return fmt.Errorf("unknown body check %q", name)
		}
		if err := check.fn(mode, body); err != nil {
			return err
		}
	}
	return nil
}

// validateBodyChecks rejects, at load time, unknown check names and use on a step
// that is not a config create/update. It is called per step from validateTool.
func validateBodyChecks(step Step) error {
	if len(step.BodyChecks) == 0 {
		return nil
	}
	opID, isConfig := strings.CutPrefix(step.Operation, "config/")
	if _, ok := bodyModeFor(opID); !isConfig || !ok {
		return fmt.Errorf("step %s: bodyChecks require a config/create*, config/update* or config/replace* operation, got %q", step.ID, step.Operation)
	}
	for _, name := range step.BodyChecks {
		if _, ok := bodyChecks[name]; !ok {
			return fmt.Errorf("step %s: unknown bodyCheck %q", step.ID, name)
		}
	}
	return nil
}
