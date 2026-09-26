// Package actionurl describes the Grandstream Action URL feature: the phone
// events that can trigger an HTTP request, the dynamic variables the phone
// substitutes into that request, and the configuration keys (legacy P-codes and
// the newer "v2" alias names) used to program each event onto a phone.
//
// References:
//   - Grandstream ActionURL Module User Guide (events + dynamic variables)
//   - GXP16xx / GXP21xx / GRP261x Administration Guides, "Action URL
//     Parameters P-values" table (P-codes)
//   - GXP21xx firmware 1.0.11.x config template (v2 alias names)
package actionurl

import (
	"fmt"
	"strings"
)

// Event is one phone event that can be bound to an Action URL.
type Event struct {
	// Slug is the stable identifier used in URLs and in the database.
	Slug string `json:"slug"`
	// Name is the event name as written in the ActionURL user guide.
	Name string `json:"name"`
	// WebUIName is the label shown on the phone's web UI
	// (Settings -> Outbound Notification -> Action URL).
	WebUIName string `json:"web_ui_name"`
	// PCode is the legacy numeric config parameter, e.g. "P8310".
	PCode string `json:"pcode"`
	// Alias is the v2 (config version="2") parameter name.
	Alias string `json:"alias"`
	// AliasVerified is false when the alias name was not found in a published
	// Grandstream config template and was derived from the naming pattern of
	// its siblings. Check your model's config template before relying on it.
	AliasVerified bool `json:"alias_verified"`
}

// PCodeNumber returns the P-code without its leading "P" (the SSH CLI's
// "set" command takes the bare number).
func (e Event) PCodeNumber() string { return strings.TrimPrefix(e.PCode, "P") }

// Provisionable reports whether the event has a config key, i.e. whether it
// can be set by XML provisioning or the SSH CLI. Events without one can only
// be entered in the phone's web UI.
func (e Event) Provisionable() bool { return e.PCode != "" }

// Events lists every event the backend accepts: first the 20 events of the
// ActionURL module, in the order the user guide lists them, then events that
// only some models offer.
var Events = []Event{
	{"incoming_call", "Incoming Call", "Incoming Call", "P8310", "ons.actionUrl.incomingCall", true},
	{"outgoing_call", "Outgoing Call", "Outgoing Call", "P8311", "ons.actionUrl.outgoingCall", true},
	{"established_call", "Establish Call", "Established Call", "P8313", "ons.actionUrl.establishedCall", true},
	{"terminated_call", "Terminate Call", "Terminated Call", "P8314", "ons.actionUrl.terminatedCall", true},
	{"off_hook", "Off Hook", "Off Hook", "P8308", "ons.actionUrl.offHook", true},
	{"on_hook", "On Hook", "On Hook", "P8309", "ons.actionUrl.onHook", true},
	{"missed_call", "Missed Call", "Missed Call", "P8312", "ons.actionUrl.missedCall", true},
	{"dnd_on", "DND On", "Open DND", "P8316", "ons.actionUrl.openDnd", true},
	{"dnd_off", "DND Off", "Close DND", "P8317", "ons.actionUrl.closedDnd", true},
	{"forward_on", "Call Forwarding On", "Open Forward", "P8318", "ons.actionUrl.openForward", true},
	{"forward_off", "Call Forwarding Off", "Close Forward", "P8319", "ons.actionUrl.closedForward", true},
	{"hold_call", "Hold Call", "Hold Call", "P8324", "ons.actionUrl.holdCall", true},
	{"resume_call", "Resume Call", "UnHold Call", "P8325", "ons.actionUrl.unholdCall", true},
	{"syslog_on", "Syslog On", "Open Syslog", "P8330", "ons.actionUrl.openSyslog", false},
	{"syslog_off", "Syslog Off", "Close Syslog", "P8331", "ons.actionUrl.closedSyslog", false},
	{"boot_completed", "Booting Completed", "Setup Completed", "P8304", "ons.actionUrl.setupCompleted", true},
	{"blind_transfer", "Blind Transferring", "Blind Transfer", "P8320", "ons.actionUrl.blindTransfer", true},
	{"attended_transfer", "Attended Transferring", "Attended Transfer", "P8321", "ons.actionUrl.attendedTransfer", true},
	{"registered", "Registration", "Registered", "P8305", "ons.actionUrl.registered", true},
	{"unregistered", "Sign Off", "Unregistered", "P8306", "ons.actionUrl.unregistered", true},

	// WP8xx-only events. The WP820 keeps its Event Notification URLs outside
	// the P-value store, so these have no config keys.
	{"log_on", "Log On", "Log On", "", "", false},
	{"log_off", "Log Off", "Log Off", "", "", false},
	{"panic_call", "SAFE/Panic Call", "SAFE/Panic Call", "", "", false},
}

var eventsBySlug = func() map[string]Event {
	m := make(map[string]Event, len(Events))
	for _, e := range Events {
		m[e.Slug] = e
	}
	return m
}()

// LookupEvent returns the event with the given slug.
func LookupEvent(slug string) (Event, bool) {
	e, ok := eventsBySlug[slug]
	return e, ok
}

// Variable is one dynamic variable the phone substitutes into an Action URL.
type Variable struct {
	// Token is the placeholder as written in the URL, including the "$".
	Token string `json:"token"`
	// Param is the query-string key this tool uses for the variable.
	Param string `json:"param"`
	// Description is taken from the ActionURL user guide.
	Description string `json:"description"`
}

// Variables lists every dynamic variable supported by the ActionURL module.
var Variables = []Variable{
	{"$phone_ip", "phone_ip", "The IP address of the phone"},
	{"$mac", "mac", "The MAC address of the phone"},
	{"$product", "product", "The product name of the phone"},
	{"$program_version", "program_version", "The software version of the phone"},
	{"$hardware_version", "hardware_version", "The hardware version of the phone"},
	{"$language", "language", "The display language of the phone"},
	{"$local", "local", "The called number on the phone"},
	{"$display_local", "display_local", "The display name of the called number on the phone"},
	{"$remote", "remote", "The call number on the remote phone"},
	{"$display_remote", "display_remote", "The display name of the call number on the remote phone"},
	{"$call-id", "call-id", "The SIP Call-ID of the session"},
	{"$active_user", "active_user", "The account number which is during a call on the phone"},
	{"$active_host", "active_host", "The SIP server of the account number which is during a call on the phone"},
	{"$duration", "duration", "Talk time (unit: seconds)"},
	{"$calldirection", "calldirection", "Direction of the call (calling party or called party)"},
}

// LookupVariable returns the variable whose Param matches name.
func LookupVariable(param string) (Variable, bool) {
	for _, v := range Variables {
		if v.Param == param {
			return v, true
		}
	}
	return Variable{}, false
}

// ParamNames returns the Param of every variable, in catalog order.
func ParamNames() []string {
	out := make([]string, len(Variables))
	for i, v := range Variables {
		out[i] = v.Param
	}
	return out
}

// Format selects which configuration key style to emit.
type Format string

const (
	// FormatPCode uses legacy numeric P-codes (config version="1").
	FormatPCode Format = "pcode"
	// FormatAlias uses v2 alias names (config version="2").
	FormatAlias Format = "alias"
)

// ParseFormat validates a format name.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatPCode, FormatAlias:
		return Format(s), nil
	case "v1", "p":
		return FormatPCode, nil
	case "v2":
		return FormatAlias, nil
	}
	return "", fmt.Errorf("unknown format %q (want %q or %q)", s, FormatPCode, FormatAlias)
}

// Key returns the configuration key for e in the given format.
func (e Event) Key(f Format) string {
	if f == FormatAlias {
		return e.Alias
	}
	return e.PCode
}
