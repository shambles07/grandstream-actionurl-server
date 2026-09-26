package actionurl

import (
	"fmt"
	"sort"
	"strings"
)

// Model describes how a phone family presents Action URLs in its web UI:
// where the fields are, what they are called, and in which order.
type Model struct {
	Name  string // identifier used on the command line
	Title string // phone families it covers
	// WebUIPath is where the Action URL fields are in the web UI.
	WebUIPath string
	// Provisionable is false when the model stores these URLs outside the
	// P-value store, so XML provisioning and the SSH CLI cannot set them.
	Provisionable bool
	Fields        []ModelField // in web UI order
	// Unsupported lists catalog events this model does not offer.
	Unsupported []string
}

// ModelField is one Action URL field as labelled in a model's web UI.
type ModelField struct {
	Event Event
	Label string
}

func fields(pairs ...string) []ModelField {
	out := make([]ModelField, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		e, ok := LookupEvent(pairs[i])
		if !ok {
			panic("actionurl: unknown event in model: " + pairs[i])
		}
		out = append(out, ModelField{Event: e, Label: pairs[i+1]})
	}
	return out
}

// Models lists the supported phone families.
var Models = []Model{
	{
		Name:          "gxp",
		Title:         "GXP16xx / GXP21xx / GRP26xx",
		WebUIPath:     "Settings > Outbound Notification > Action URL",
		Provisionable: true,
		// Order of the web UI and the admin guides' P-value table.
		Fields: fields(
			"boot_completed", "Setup Completed",
			"registered", "Registered",
			"unregistered", "Unregistered",
			"off_hook", "Off Hook",
			"on_hook", "On Hook",
			"incoming_call", "Incoming Call",
			"outgoing_call", "Outgoing Call",
			"missed_call", "Missed Call",
			"established_call", "Established Call",
			"terminated_call", "Terminated Call",
			"dnd_on", "Open DND",
			"dnd_off", "Close DND",
			"forward_on", "Open Forward",
			"forward_off", "Close Forward",
			"blind_transfer", "Blind Transfer",
			"attended_transfer", "Attended Transfer",
			"hold_call", "Hold Call",
			"resume_call", "UnHold Call",
			"syslog_on", "Open Syslog",
			"syslog_off", "Close Syslog",
		),
		Unsupported: []string{"log_on", "log_off", "panic_call"},
	},
	{
		Name:          "wp820",
		Title:         "WP820 / WP8xx",
		WebUIPath:     "Maintenance > Event Notification",
		Provisionable: false,
		// Order and labels of the WP820 administration guide.
		Fields: fields(
			"boot_completed", "Bootup Completed",
			"incoming_call", "Incoming Call",
			"outgoing_call", "Outgoing Call",
			"missed_call", "Missed Call",
			"established_call", "Connected",
			"terminated_call", "Disconnected",
			"dnd_on", "DND On",
			"dnd_off", "DND Off",
			"forward_on", "Forward On",
			"forward_off", "Forward Off",
			"blind_transfer", "Blind Transfer",
			"attended_transfer", "Attended Transfer",
			"hold_call", "On Hold",
			"resume_call", "Unhold",
			"log_on", "Log On",
			"log_off", "Log Off",
			"registered", "Register",
			"unregistered", "Unregister",
			"panic_call", "SAFE/Panic Call",
		),
		Unsupported: []string{"off_hook", "on_hook", "syslog_on", "syslog_off"},
	},
}

// LookupModel returns the model with the given name (case-insensitive).
func LookupModel(name string) (Model, error) {
	for _, m := range Models {
		if strings.EqualFold(m.Name, name) {
			return m, nil
		}
	}
	names := make([]string, len(Models))
	for i, m := range Models {
		names[i] = m.Name
	}
	sort.Strings(names)
	return Model{}, fmt.Errorf("unknown model %q (want one of: %s)", name, strings.Join(names, ", "))
}

// Select returns the model's fields for the given event slugs, in web UI
// order. Empty means all fields.
func (m Model) Select(slugs []string) ([]ModelField, error) {
	if len(slugs) == 0 {
		return m.Fields, nil
	}
	want := map[string]bool{}
	for _, s := range slugs {
		if _, ok := LookupEvent(s); !ok {
			return nil, fmt.Errorf("unknown event %q", s)
		}
		want[s] = true
	}
	var out []ModelField
	for _, f := range m.Fields {
		if want[f.Event.Slug] {
			out = append(out, f)
			delete(want, f.Event.Slug)
		}
	}
	for s := range want {
		return nil, fmt.Errorf("model %s has no %q event", m.Name, s)
	}
	return out, nil
}
