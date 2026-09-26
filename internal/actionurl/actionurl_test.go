package actionurl

import "testing"

// Every model must account for every event exactly once, either as a field
// or as unsupported, so that adding an event forces a decision per model.
func TestModelsCoverCatalog(t *testing.T) {
	for _, m := range Models {
		seen := map[string]string{}
		for _, f := range m.Fields {
			if prev, dup := seen[f.Event.Slug]; dup {
				t.Errorf("%s: %s listed twice (%s)", m.Name, f.Event.Slug, prev)
			}
			seen[f.Event.Slug] = "field"
			if m.Provisionable && !f.Event.Provisionable() {
				t.Errorf("%s: field %s has no config key", m.Name, f.Event.Slug)
			}
		}
		for _, s := range m.Unsupported {
			if prev, dup := seen[s]; dup {
				t.Errorf("%s: %s is both %s and unsupported", m.Name, s, prev)
			}
			if _, ok := LookupEvent(s); !ok {
				t.Errorf("%s: unknown unsupported event %s", m.Name, s)
			}
			seen[s] = "unsupported"
		}
		for _, e := range Events {
			if _, ok := seen[e.Slug]; !ok {
				t.Errorf("%s: event %s is neither a field nor unsupported", m.Name, e.Slug)
			}
		}
	}
}

func TestSelectAndBuild(t *testing.T) {
	def, _ := SelectEvents(nil)
	if len(def) != 20 {
		t.Errorf("default events = %d, want the 20 provisionable ones", len(def))
	}
	if _, err := BuildSettings(FormatPCode, URLOptions{Server: "http://x"}, []Event{eventsBySlug["log_on"]}); err == nil {
		t.Error("building a setting for a web-UI-only event should fail")
	}

	m, _ := LookupModel("WP820")
	sel, err := m.Select([]string{"panic_call", "boot_completed"})
	if err != nil || len(sel) != 2 || sel[0].Label != "Bootup Completed" {
		t.Errorf("Select = %+v, %v (want web UI order)", sel, err)
	}
	if _, err := m.Select([]string{"off_hook"}); err == nil {
		t.Error("selecting an event the model lacks should fail")
	}

	u, err := BuildURL(eventsBySlug["incoming_call"], URLOptions{Server: "http://h:8086/", Token: "a b", Params: []string{"mac", "call-id"}})
	if err != nil || u != "http://h:8086/actionurl/incoming_call?mac=$mac&call-id=$call-id&token=a+b" {
		t.Errorf("BuildURL = %q, %v", u, err)
	}
	if got := eventsBySlug["log_on"].PCodeNumber(); got != "" {
		t.Errorf("PCodeNumber of web-UI-only event = %q", got)
	}
}
