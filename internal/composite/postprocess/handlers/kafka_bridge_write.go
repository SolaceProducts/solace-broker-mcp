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

package handlers

import (
	"fmt"
	"strings"

	"github.com/SolaceProducts/solace-broker-mcp/internal/composite/postprocess"
)

// kafkaBridgeKind describes one kind of Kafka bridge the write-tool handlers
// serve. Receiver and sender are the same shape, so the handlers are built per
// kind from this table rather than copied.
type kafkaBridgeKind struct {
	object            string // result "object" value, and the handler-name prefix
	label             string // human label used in the text
	nameField         string // SEMP attribute holding the bridge name
	configParam       string // update tool parameter carrying the attributes to change
	createTool        string
	updateTool        string
	statusTool        string
	createStepID      string // YAML step ID of the create tool's SEMP call
	updateStepID      string // YAML step ID of the update tool's SEMP call
	bindingCountField string // status-tool field: configured bindings
	bindingUpField    string // status-tool field: bindings that are up
}

var kafkaBridgeKinds = []kafkaBridgeKind{
	{
		object:            "kafkaReceiver",
		label:             "Kafka Receiver",
		nameField:         "kafkaReceiverName",
		configParam:       "kafkaReceiverConfig",
		createTool:        "create-kafka-receiver",
		updateTool:        "update-kafka-receiver",
		statusTool:        "get-kafka-receiver-status",
		createStepID:      "createKafkaReceiver",
		updateStepID:      "updateKafkaReceiver",
		bindingCountField: "topicBindingCount",
		bindingUpField:    "topicBindingUpCount",
	},
}

// kafkaSchemeSecretFields lists, per authentication scheme, the attributes the
// user must set out of band. Credentials always travel with their identity: the
// broker refuses a password without its username in the same request, a keytab
// without its principal and file name, and so on, so the text says "together".
// A scheme missing from the table falls back to generic wording (new schemes in
// a newer broker must not break the handler).
var kafkaSchemeSecretFields = map[string][]string{
	"basic":              {"authenticationBasicUsername", "authenticationBasicPassword"},
	"scram":              {"authenticationScramUsername", "authenticationScramPassword"},
	"client-certificate": {"authenticationClientCertContent", "authenticationClientCertPassword (only if the key is encrypted)"},
	"kerberos":           {"authenticationKerberosKeytabContent", "authenticationKerberosKeytabFileName", "authenticationKerberosUserPrincipalName"},
	"oauth-client":       {"authenticationOauthClientSecret"},
	"aws-msk-iam":        {"authenticationAwsMskIamSecretAccessKey"},
	"aws-msk-iam-sts":    {"authenticationAwsMskIamSecretAccessKey"},
}

func init() {
	for _, k := range kafkaBridgeKinds {
		postprocess.Register(k.object+"Created", postprocess.Handler{
			Fn:             k.created,
			RequiredSteps:  []string{k.createStepID},
			RequiredFields: []string{"msgVpnName", k.nameField, "authenticationScheme", "enabled"},
		})
		postprocess.Register(k.object+"Updated", postprocess.Handler{
			Fn:             k.updated,
			RequiredSteps:  []string{k.updateStepID},
			RequiredFields: []string{"msgVpnName", k.nameField, "authenticationScheme", "enabled"},
		})
	}
}

// bridgeFacts are the fields the text is built from, read off the SEMP response's data.
type bridgeFacts struct {
	name, vpn, scheme string
	enabled           bool
}

func (k kafkaBridgeKind) facts(stepResults map[string]map[string]any, stepID string) (bridgeFacts, error) {
	step, ok := stepResults[stepID]
	if !ok {
		return bridgeFacts{}, fmt.Errorf("step %q not in results", stepID)
	}
	data, ok := step["data"].(map[string]any)
	if !ok {
		return bridgeFacts{}, fmt.Errorf("%s.data: want object, got %T", stepID, step["data"])
	}
	// authenticationScheme and enabled are what the hand-off turns on: a missing or
	// mistyped scheme would read as "no secret needed" and drop the hand-off exactly
	// when it matters, so they are required like the identifiers.
	name, ok1 := stringField(data, k.nameField)
	vpn, ok2 := stringField(data, "msgVpnName")
	scheme, ok3 := stringField(data, "authenticationScheme")
	enabled, ok4 := boolField(data, "enabled")
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return bridgeFacts{}, fmt.Errorf("%s.data: want %s, msgVpnName, authenticationScheme (string) and enabled (boolean)", stepID, k.nameField)
	}
	return bridgeFacts{name: name, vpn: vpn, scheme: scheme, enabled: enabled}, nil
}

// created builds the summary for a create call. A bridge that needs a secret gets
// the hand-off text; the tool never sees the secret (see the bodyChecks).
func (k kafkaBridgeKind) created(stepResults map[string]map[string]any) (map[string]any, error) {
	f, err := k.facts(stepResults, k.createStepID)
	if err != nil {
		return nil, err
	}
	out := k.base(f)
	needsSecret := needsSecret(f.scheme)
	out["secretRequired"] = needsSecret

	switch {
	case needsSecret:
		k.addHandoff(out, f, "was created disabled with")
	case f.enabled:
		out["nextStep"] = k.pollText()
	default:
		out["nextStep"] = fmt.Sprintf("%s %q was created disabled. Enable it with %s when it should start.", k.label, f.name, k.updateTool)
	}
	return out, nil
}

// updated builds the summary for an update call.
func (k kafkaBridgeKind) updated(stepResults map[string]map[string]any) (map[string]any, error) {
	f, err := k.facts(stepResults, k.updateStepID)
	if err != nil {
		return nil, err
	}
	out := k.base(f)
	if f.enabled {
		out["nextStep"] = k.pollText()
		return out, nil
	}
	// An update that switches to an authenticated scheme leaves the bridge disabled
	// until the new secret is set out of band (the body check requires enabled:false
	// with it), so it needs the same per-scheme hand-off a create gets: a Kerberos
	// switch needs three attributes set together, not a generic "set its secret".
	if needsSecret(f.scheme) {
		out["secretRequired"] = true
		k.addHandoff(out, f, "now uses")
		return out, nil
	}
	out["nextStep"] = fmt.Sprintf(
		"%s %q is disabled. Enable it with %s when it should start; if its authenticationScheme needs a secret, the user must set it out of band first.",
		k.label, f.name, k.updateTool)
	return out, nil
}

func (k kafkaBridgeKind) base(f bridgeFacts) map[string]any {
	return map[string]any{
		"object":               k.object,
		"name":                 f.name,
		"msgVpnName":           f.vpn,
		"authenticationScheme": f.scheme,
		"enabled":              f.enabled,
	}
}

func (k kafkaBridgeKind) secretFields(scheme string) []string {
	if fields, ok := kafkaSchemeSecretFields[scheme]; ok {
		return append([]string(nil), fields...)
	}
	return []string{"the authentication secret for scheme " + fmt.Sprintf("%q", scheme)}
}

// needsSecret reports whether a bridge with this scheme needs a credential set out
// of band before it can connect.
func needsSecret(scheme string) bool { return scheme != "" && scheme != "none" }

// addHandoff adds the secret hand-off to a summary: which attributes the user must
// set, the text to show them, and what to do once they confirm. intro completes
// "<label> <name> ... authenticationScheme X": "was created disabled with" for a
// create, "now uses" for an update that switched scheme (the bridge is disabled).
func (k kafkaBridgeKind) addHandoff(out map[string]any, f bridgeFacts, intro string) {
	fields := k.secretFields(f.scheme)
	out["secretFields"] = fields
	out["handoff"] = k.handoff(f, fields, intro)
	out["nextStep"] = fmt.Sprintf(
		"When the user confirms the secret is set, call %s for %s %q with %s {\"enabled\": true}, then %s",
		k.updateTool, k.label, f.name, k.configParam, k.pollText())
}

func (k kafkaBridgeKind) handoff(f bridgeFacts, fields []string, intro string) string {
	disabled := ""
	if intro != "was created disabled with" {
		disabled = " and is disabled"
	}
	return fmt.Sprintf(
		"%s %q %s authenticationScheme %q%s. It cannot connect until its secret is set, and I must not handle that secret. "+
			"Please set %s together on %s %q (Message VPN %q) yourself, using Broker Manager, the CLI, SEMP or Terraform. "+
			"Do not paste the secret into this chat. Tell me when it is done and I will enable the bridge and check its status.",
		k.label, f.name, intro, f.scheme, disabled, strings.Join(fields, ", "), k.label, f.name, f.vpn)
}

// pollText tells the agent how to confirm a bridge came up. Enabling is
// asynchronous: right after the call the bridge reads up=false with a transient
// "No remote-broker connection", so one immediate check would report a false failure.
func (k kafkaBridgeKind) pollText() string {
	return fmt.Sprintf(
		"Enabling can take 10 to 40 seconds. Call %s; if up is false with failureReason \"No remote-broker connection\" or empty, wait about 10 seconds and retry for up to a minute before concluding it failed. "+
			"failureReason \"Username or password missing\" means the secret is not set; \"Authentication failed\" in lastNotice means it is wrong, so ask the user to set it again themselves. "+
			"If up is true but %s is below %s, a binding is still starting or is down.",
		k.statusTool, k.bindingUpField, k.bindingCountField)
}
