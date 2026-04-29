// The structs in this file model the wire format of:
//
//	<rpc><show><redundancy/></show></rpc>
//
// captured from a live Solace PubSub+ Standard 10.25.0.208 broker.
// XML tags reflect the broker's element names (kebab-case); JSON tags
// reflect the MCP-facing output (camelCase per project convention).
//
// All types are unexported — they are internal parsing artifacts of
// the redundancy handler. The handler converts the decoded struct into
// a map[string]any envelope before returning to the ToolManager.
package redundancy

import "encoding/xml"

// redundancyResponse models the <redundancy>...</redundancy> element of
// a SEMPv1 show-redundancy reply.
type redundancyResponse struct {
	XMLName                       xml.Name        `xml:"redundancy" json:"-"`
	ConfigStatus                  string          `xml:"config-status" json:"configStatus"`
	RedundancyStatus              string          `xml:"redundancy-status" json:"redundancyStatus"`
	OperatingMode                 string          `xml:"operating-mode" json:"operatingMode"`
	SwitchoverMechanism           string          `xml:"switchover-mechanism" json:"switchoverMechanism"`
	AutoRevert                    bool            `xml:"auto-revert" json:"autoRevert"`
	RedundancyMode                string          `xml:"redundancy-mode" json:"redundancyMode"`
	ActiveStandbyRole             string          `xml:"active-standby-role" json:"activeStandbyRole"`
	GroupManagementServerIdentity string          `xml:"group-management-server-identity" json:"groupManagementServerIdentity"`
	MateRouterName                string          `xml:"mate-router-name" json:"mateRouterName"`
	OperStatus                    operStatus      `xml:"oper-status" json:"operStatus"`
	FailoverCriteria              string          `xml:"failover-criteria" json:"failoverCriteria"`
	VRRPInterfaces                []vrrpInterface `xml:"vrrp-interfaces>interface" json:"vrrpInterfaces"`
	VirtualRouters                virtualRouters  `xml:"virtual-routers" json:"virtualRouters"`
}

// operStatus is the <oper-status> sub-element with mate-link health flags.
type operStatus struct {
	ADBLinkUp  bool `xml:"adb-link-up" json:"adbLinkUp"`
	ADBHelloUp bool `xml:"adb-hello-up" json:"adbHelloUp"`
}

// vrrpInterface is one entry in the top-level <vrrp-interfaces> list.
type vrrpInterface struct {
	Name          string `xml:"name" json:"name"`
	StaticAddress string `xml:"static-address" json:"staticAddress"`
	StaticStatus  string `xml:"static-status" json:"staticStatus"`
}

// virtualRouters wraps the <primary> and <backup> virtual-router subtrees.
type virtualRouters struct {
	Primary virtualRouter `xml:"primary" json:"primary"`
	Backup  virtualRouter `xml:"backup" json:"backup"`
}

// virtualRouter is the per-role (<primary> or <backup>) container.
type virtualRouter struct {
	Config virtualRouterConfig `xml:"config" json:"config"`
	Status virtualRouterStatus `xml:"status" json:"status"`
}

// virtualRouterConfig holds the configured routing-interface and VRID.
type virtualRouterConfig struct {
	RoutingInterface string `xml:"routing-interface" json:"routingInterface"`
	VRRPVRID         string `xml:"vrrp-vrid" json:"vrrpVrid"`
}

// virtualRouterStatus holds the operational status for one virtual router,
// including its mate-priority subtree.
//
// Optional fields use pointer types + json `omitempty` so the JSON output
// faithfully mirrors the wire response: when the broker omits an element
// (e.g., backup-role status often has only <activity>), encoding/xml
// leaves the pointer nil and json.Marshal drops the key entirely. Without
// this, the omitted XML would still appear in JSON as Go's zero value,
// inventing data that the broker never sent.
type virtualRouterStatus struct {
	Activity               string                  `xml:"activity" json:"activity"`
	VRRP                   *string                 `xml:"vrrp" json:"vrrp,omitempty"`
	VRRPInterfaces         []vrrpStatusInterface   `xml:"vrrp-interfaces>interface" json:"vrrpInterfaces,omitempty"`
	RoutingInterface       *string                 `xml:"routing-interface" json:"routingInterface,omitempty"`
	VRRPPriority           *int                    `xml:"vrrp-priority" json:"vrrpPriority,omitempty"`
	PriorityReportedByMate *priorityReportedByMate `xml:"priority-reported-by-mate" json:"priorityReportedByMate,omitempty"`
}

// vrrpStatusInterface is a per-interface VRRP state entry inside
// <virtual-routers>/<role>/<status>/<vrrp-interfaces>.
// Note: distinct from the top-level vrrpInterface — different fields.
type vrrpStatusInterface struct {
	Name   string `xml:"name" json:"name"`
	Status string `xml:"status" json:"status"`
}

// priorityReportedByMate models the <priority-reported-by-mate> subtree
// inside a <status> element. All inner fields use pointer + omitempty so
// the JSON output mirrors the wire — see the rationale comment above
// virtualRouterStatus.
type priorityReportedByMate struct {
	Summary        *string                       `xml:"summary" json:"summary,omitempty"`
	CSPF           *string                       `xml:"cspf" json:"cspf,omitempty"`
	ADBHello       *string                       `xml:"adb-hello" json:"adbHello,omitempty"`
	VRRP           *string                       `xml:"vrrp" json:"vrrp,omitempty"`
	VRRPInterfaces []vrrpStatusPriorityInterface `xml:"vrrp-interfaces>interface" json:"vrrpInterfaces,omitempty"`
}

// vrrpStatusPriorityInterface is a per-interface entry inside the
// priority-reported-by-mate's <vrrp-interfaces> list.
// Distinct shape from vrrpStatusInterface (priority instead of status).
type vrrpStatusPriorityInterface struct {
	Name     string `xml:"name" json:"name"`
	Priority string `xml:"priority" json:"priority"`
}
