package server

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shambles07/grandstream-actionurl-server/internal/actionurl"
	"github.com/shambles07/grandstream-actionurl-server/internal/store"
)

// handleIngest receives an Action URL request from a phone. Accepted forms:
//
//	/actionurl/<event>?mac=...&local=...   (what gsprov generates)
//	/actionurl/<event>/mac=...&local=...   (the guide's "server/var=$var" style)
func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.EscapedPath(), actionurl.IngestPathPrefix)
	slug, pathQuery, _ := strings.Cut(rest, "/")
	ev, ok := actionurl.LookupEvent(slug)
	if !ok {
		http.Error(w, "unknown event", http.StatusNotFound)
		return
	}

	raw := r.URL.RawQuery
	if pathQuery != "" {
		if raw != "" {
			raw = pathQuery + "&" + raw
		} else {
			raw = pathQuery
		}
	}
	params := parseQuery(raw)

	if s.cfg.IngestToken != "" {
		if subtle.ConstantTimeCompare([]byte(params["token"]), []byte(s.cfg.IngestToken)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	delete(params, "token")

	e := eventFromParams(ev.Slug, params)
	e.ReceivedAt = time.Now()
	e.RawQuery = redactToken(raw)
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		e.SourceIP = host
	}

	if err := s.store.Record(r.Context(), &e); err != nil {
		s.log.Error("record event", "event", ev.Slug, "err", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	s.log.Debug("event", "id", e.ID, "event", e.Event, "mac", e.MAC, "src", e.SourceIP)
	s.hub.publish(e)

	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte("OK\n"))
}

// parseQuery splits a query string on '&' and the first '='. Unlike
// url.ParseQuery it does not turn '+' into a space, since phones send E.164
// numbers such as "+15551234567" unencoded. Later duplicates win.
func parseQuery(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v, _ := strings.Cut(pair, "=")
		if uk, err := url.PathUnescape(k); err == nil {
			k = uk
		}
		if uv, err := url.PathUnescape(v); err == nil {
			v = uv
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

// eventFromParams maps query parameters onto an Event. Values that are still
// the literal "$variable" placeholder (the phone had nothing to substitute
// for that event) are stored as empty.
func eventFromParams(slug string, p map[string]string) store.Event {
	take := func(param string) string {
		v, ok := p[param]
		if !ok {
			return ""
		}
		delete(p, param)
		if strings.HasPrefix(v, "$") {
			return ""
		}
		return v
	}
	e := store.Event{
		Event:           slug,
		MAC:             normalizeMAC(take("mac")),
		PhoneIP:         take("phone_ip"),
		Product:         take("product"),
		ProgramVersion:  take("program_version"),
		HardwareVersion: take("hardware_version"),
		Language:        take("language"),
		Local:           take("local"),
		DisplayLocal:    take("display_local"),
		Remote:          take("remote"),
		DisplayRemote:   take("display_remote"),
		CallID:          take("call-id"),
		ActiveUser:      take("active_user"),
		ActiveHost:      take("active_host"),
		CallDirection:   take("calldirection"),
	}
	if e.CallID == "" {
		e.CallID = take("call_id") // tolerate hand-written URLs
	}
	if d := take("duration"); d != "" {
		if n, err := strconv.ParseInt(d, 10, 64); err == nil {
			e.Duration = &n
		} else {
			p["duration"] = d
		}
	}
	if len(p) > 0 {
		e.Extra = p
	}
	return e
}

// normalizeMAC lowercases a MAC and strips separators, so "00:0B:82:12:34:56",
// "00-0b-82-12-34-56" and "000b82123456" all key the same phone.
func normalizeMAC(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		switch r {
		case ':', '-', '.', ' ':
			return -1
		}
		return r
	}, s)
}

func redactToken(raw string) string {
	parts := strings.Split(raw, "&")
	for i, p := range parts {
		if strings.HasPrefix(p, "token=") {
			parts[i] = "token=REDACTED"
		}
	}
	return strings.Join(parts, "&")
}
