package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shambles07/grandstream-actionurl-server/internal/actionurl"
	"github.com/shambles07/grandstream-actionurl-server/internal/store"
)

func startServer(t *testing.T, cfg Config) (string, *store.Store) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Addr = ln.Addr().String()
	ln.Close()
	srv := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
		st.Close()
	})
	base := "http://" + cfg.Addr
	for i := 0; i < 50; i++ {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return base, st
}

// rawGet sends a request line exactly as given, the way a phone might.
func rawGet(t *testing.T, addr, target string) int {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(addr, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n", target)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET %s: %d %s", url, resp.StatusCode, b)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

func TestCallFlow(t *testing.T) {
	base, _ := startServer(t, Config{IngestToken: "s3cret"})

	common := "mac=00:0B:82:AA:BB:CC&phone_ip=10.0.0.20&product=GXP2170&program_version=1.0.11.79" +
		"&hardware_version=V1.2A&language=English&active_user=1001&active_host=pbx.example.com" +
		"&call-id=abc123@10.0.0.20&token=s3cret"

	steps := []struct{ event, extra string }{
		{"boot_completed", "&call-id=$call-id&remote=$remote"},
		{"registered", ""},
		// Raw spaces and an unencoded '+' as a phone would send them.
		{"incoming_call", "&local=1001&display_local=Front Desk&remote=+15551234567&display_remote=John Doe&calldirection=called"},
		{"off_hook", ""},
		{"established_call", ""},
		{"hold_call", ""},
		{"resume_call", ""},
		{"terminated_call", "&duration=42"},
		{"on_hook", ""},
		{"dnd_on", ""},
	}
	for _, s := range steps {
		q := common + s.extra
		if s.event == "boot_completed" {
			q = strings.Replace(q, "call-id=abc123@10.0.0.20&", "", 1)
		}
		if code := rawGet(t, base, actionurl.IngestPathPrefix+s.event+"?"+q); code != 200 {
			t.Fatalf("%s: status %d", s.event, code)
		}
	}

	var phone store.Phone
	getJSON(t, base+"/api/v1/phones/000b82aabbcc", &phone)
	if phone.Product != "GXP2170" || phone.EventCount != int64(len(steps)) || phone.LastEvent != "dnd_on" {
		t.Errorf("phone = %+v", phone)
	}
	if phone.Registered == nil || !*phone.Registered || phone.DND == nil || !*phone.DND ||
		phone.OffHook == nil || *phone.OffHook || phone.LastBootAt == "" {
		t.Errorf("phone flags = %+v", phone)
	}

	var calls struct{ Calls []store.Call }
	getJSON(t, base+"/api/v1/calls?mac=00-0b-82-aa-bb-cc", &calls)
	if len(calls.Calls) != 1 {
		t.Fatalf("calls = %+v", calls)
	}
	c := calls.Calls[0]
	if c.State != "ended" || c.Direction != "incoming" || c.Remote != "+15551234567" ||
		c.DisplayRemote != "John Doe" || c.DisplayLocal != "Front Desk" ||
		c.Duration == nil || *c.Duration != 42 || c.AnsweredAt == "" || c.EndedAt == "" {
		t.Errorf("call = %+v", c)
	}

	var evs struct {
		Events []store.Event
	}
	getJSON(t, base+"/api/v1/events?event=boot_completed", &evs)
	if len(evs.Events) != 1 || evs.Events[0].CallID != "" || evs.Events[0].Remote != "" {
		t.Errorf("unsubstituted placeholders should be empty: %+v", evs.Events)
	}
	if strings.Contains(evs.Events[0].RawQuery, "s3cret") {
		t.Errorf("token leaked into raw_query: %s", evs.Events[0].RawQuery)
	}
}

func TestPathStyleAndAuth(t *testing.T) {
	base, _ := startServer(t, Config{IngestToken: "tok", APIToken: "api"})

	if code := rawGet(t, base, "/actionurl/missed_call?mac=000b82000001"); code != 403 {
		t.Errorf("missing token: status %d, want 403", code)
	}
	if code := rawGet(t, base, "/actionurl/nope?token=tok"); code != 404 {
		t.Errorf("unknown event: status %d, want 404", code)
	}
	// Guide style: variables in the path after the event.
	if code := rawGet(t, base, "/actionurl/missed_call/mac=000b82000001&remote=2000&call-id=x1&token=tok"); code != 200 {
		t.Fatalf("path style: status %d", code)
	}

	resp, _ := http.Get(base + "/api/v1/phones")
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("api without bearer: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", base+"/api/v1/calls?state=missed", nil)
	req.Header.Set("Authorization", "Bearer api")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var calls struct{ Calls []store.Call }
	json.NewDecoder(resp.Body).Decode(&calls)
	if len(calls.Calls) != 1 || calls.Calls[0].Remote != "2000" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestKeepAliveRequestsAreAllRewritten(t *testing.T) {
	base, st := startServer(t, Config{})
	c, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	br := bufio.NewReader(c)
	for i, name := range []string{"Ann Lee", "Bob Ray"} {
		fmt.Fprintf(c, "GET /actionurl/outgoing_call?mac=000b82000002&call-id=k%d&display_remote=%s HTTP/1.1\r\nHost: x\r\n\r\n", i, name)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
	calls, err := st.ListCalls(context.Background(), store.CallFilter{MAC: "000b82000002"})
	if err != nil || len(calls) != 2 {
		t.Fatalf("calls = %+v, %v", calls, err)
	}
}

func TestFixRequestLine(t *testing.T) {
	got := fixRequestLine("GET /a?x=John Doe&y=\"q\" HTTP/1.1\r\n")
	want := "GET /a?x=John%20Doe&y=%22q%22 HTTP/1.1\r\n"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	if l := "GET /ok HTTP/1.1\r\n"; fixRequestLine(l) != l {
		t.Errorf("clean line changed")
	}
}

func TestParseQueryKeepsPlus(t *testing.T) {
	q := parseQuery("remote=+1555&name=A%20B&bad=100%")
	if q["remote"] != "+1555" || q["name"] != "A B" || q["bad"] != "100%" {
		t.Errorf("%v", q)
	}
}
