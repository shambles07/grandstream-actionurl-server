package actionurl

import (
	"fmt"
	"net/url"
	"strings"
)

// IngestPathPrefix is the HTTP path under which the server receives events.
// The full path is IngestPathPrefix + event slug.
const IngestPathPrefix = "/actionurl/"

// URLOptions controls how Action URL templates are built.
type URLOptions struct {
	// Server is the base URL of the backend as reachable from the phone,
	// e.g. "http://10.0.0.5:8080". A missing scheme is left missing, since
	// some older firmware expects the bare "host[:port]/path" form.
	Server string
	// Token, when set, is appended as token=<Token> so the backend can reject
	// requests that do not come from a provisioned phone.
	Token string
	// Params limits which dynamic variables are included. Empty means all.
	Params []string
}

// BuildURL returns the Action URL template for event e: the string that is
// stored on the phone, with "$variable" placeholders left intact for the
// phone to substitute.
func BuildURL(e Event, o URLOptions) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(o.Server), "/")
	if base == "" {
		return "", fmt.Errorf("server address is required")
	}
	params := o.Params
	if len(params) == 0 {
		params = ParamNames()
	}

	var b strings.Builder
	b.WriteString(base)
	b.WriteString(IngestPathPrefix)
	b.WriteString(e.Slug)
	sep := byte('?')
	for _, p := range params {
		v, ok := LookupVariable(p)
		if !ok {
			return "", fmt.Errorf("unknown dynamic variable %q", p)
		}
		b.WriteByte(sep)
		sep = '&'
		b.WriteString(v.Param)
		b.WriteByte('=')
		b.WriteString(v.Token)
	}
	if o.Token != "" {
		b.WriteByte(sep)
		b.WriteString("token=")
		b.WriteString(url.QueryEscape(o.Token))
	}
	return b.String(), nil
}

// Setting is one configuration key/value pair to program onto a phone.
type Setting struct {
	Event Event
	Key   string
	Value string
}

// BuildSettings returns one Setting per event, in catalog order.
func BuildSettings(f Format, o URLOptions, events []Event) ([]Setting, error) {
	if len(events) == 0 {
		events = Events
	}
	out := make([]Setting, 0, len(events))
	for _, e := range events {
		u, err := BuildURL(e, o)
		if err != nil {
			return nil, err
		}
		out = append(out, Setting{Event: e, Key: e.Key(f), Value: u})
	}
	return out, nil
}

// SelectEvents resolves a list of slugs to events. Empty means all events.
func SelectEvents(slugs []string) ([]Event, error) {
	if len(slugs) == 0 {
		return Events, nil
	}
	out := make([]Event, 0, len(slugs))
	for _, s := range slugs {
		e, ok := LookupEvent(s)
		if !ok {
			return nil, fmt.Errorf("unknown event %q", s)
		}
		out = append(out, e)
	}
	return out, nil
}
