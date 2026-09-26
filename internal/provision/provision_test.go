package provision

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/shambles07/grandstream-actionurl-server/internal/actionurl"
)

func testSettings(t *testing.T, f actionurl.Format) []actionurl.Setting {
	t.Helper()
	s, err := actionurl.BuildSettings(f, actionurl.URLOptions{Server: "http://10.0.0.5:8080", Token: "t"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWriteXML(t *testing.T) {
	for _, f := range []actionurl.Format{actionurl.FormatPCode, actionurl.FormatAlias} {
		var b bytes.Buffer
		if err := WriteXML(&b, f, "000b82aabbcc", testSettings(t, f)); err != nil {
			t.Fatal(err)
		}
		// Must be well-formed, and values must round-trip unescaped.
		dec := xml.NewDecoder(bytes.NewReader(b.Bytes()))
		var texts []string
		for {
			tok, err := dec.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("%s: invalid XML: %v\n%s", f, err, b.String())
			}
			if cd, ok := tok.(xml.CharData); ok && strings.HasPrefix(string(cd), "http://") {
				texts = append(texts, string(cd))
			}
		}
		provisionable, _ := actionurl.SelectEvents(nil)
		if len(texts) != len(provisionable) {
			t.Fatalf("%s: %d values, want %d", f, len(texts), len(provisionable))
		}
		if !strings.Contains(texts[0], "&call-id=$call-id&") {
			t.Errorf("%s: value not round-tripped: %s", f, texts[0])
		}
		want := map[actionurl.Format]string{
			actionurl.FormatPCode: "<P8310>",
			actionurl.FormatAlias: `<item name="ons.actionUrl.incomingCall">`,
		}[f]
		if !strings.Contains(b.String(), want) {
			t.Errorf("%s: missing %s", f, want)
		}
	}
}

// fakePhone emulates the Grandstream SSH CLI closely enough to exercise the
// expect logic: GS> prompt, config mode, set/get/commit/exit, echo.
type fakePhone struct {
	mu        sync.Mutex
	vals      map[string]string
	committed bool
	maxLine   int // truncate values longer than this (0 = unlimited)
}

func (p *fakePhone) serve(t *testing.T) string {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) == "pw" {
				return nil, nil
			}
			return nil, fmt.Errorf("bad password")
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(c, cfg)
		}
	}()
	return ln.Addr().String()
}

func (p *fakePhone) handle(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, _ := nc.Accept()
		go func() {
			for r := range creqs {
				r.Reply(r.Type == "pty-req" || r.Type == "shell", nil)
			}
		}()
		go p.shell(ch)
	}
}

func (p *fakePhone) shell(ch ssh.Channel) {
	defer ch.Close()
	io.WriteString(ch, "Grandstream GXP2170 Command Shell\r\nGS> ")
	br := bufio.NewReader(ch)
	prompt := "GS> "
	for {
		line, err := br.ReadString('\r')
		if err != nil {
			return
		}
		line = strings.TrimSuffix(line, "\r")
		io.WriteString(ch, line+"\r\n") // echo
		f := strings.SplitN(line, " ", 3)
		p.mu.Lock()
		switch {
		case f[0] == "config" && prompt == "GS> ":
			prompt = "CONFIG> "
		case f[0] == "set" && len(f) == 3:
			v := f[2]
			if p.maxLine > 0 && len(v) > p.maxLine {
				v = v[:p.maxLine]
			}
			p.vals[f[1]] = v
			fmt.Fprintf(ch, "%s = %s\r\n", f[1], v)
		case f[0] == "get" && len(f) == 2:
			fmt.Fprintf(ch, "%s = %s\r\n", f[1], p.vals[f[1]])
		case f[0] == "commit":
			p.committed = true
			io.WriteString(ch, "Changes are commited.\r\n")
		case f[0] == "exit":
			if prompt == "GS> " {
				p.mu.Unlock()
				return
			}
			prompt = "GS> "
		default:
			io.WriteString(ch, "Unknown command\r\n")
		}
		p.mu.Unlock()
		io.WriteString(ch, prompt)
	}
}

func TestPushSSH(t *testing.T) {
	phone := &fakePhone{vals: map[string]string{}}
	addr := phone.serve(t)
	settings := testSettings(t, actionurl.FormatPCode)
	cmds := CLICommands(settings, actionurl.FormatPCode, false)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := PushSSH(ctx, addr, cmds, SSHOptions{
		User: "admin", Password: "pw", HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout: 5 * time.Second, Verify: true, Commit: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(cmds) || len(cmds) != 20 || !res[0].Verified {
		t.Fatalf("results = %+v", res)
	}
	phone.mu.Lock()
	defer phone.mu.Unlock()
	if !phone.committed {
		t.Error("not committed")
	}
	if got := phone.vals["8310"]; got != settings[0].Value {
		t.Errorf("8310 = %q, want %q", got, settings[0].Value)
	}
}

func TestPushSSHDetectsTruncation(t *testing.T) {
	phone := &fakePhone{vals: map[string]string{}, maxLine: 64}
	addr := phone.serve(t)
	cmds := CLICommands(testSettings(t, actionurl.FormatPCode), actionurl.FormatPCode, false)
	_, err := PushSSH(context.Background(), addr, cmds, SSHOptions{
		User: "admin", Password: "pw", HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout: 5 * time.Second, Verify: true, Commit: true,
	})
	if err == nil || !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("want truncation error, got %v", err)
	}
	if phone.committed {
		t.Error("must not commit after a failed verify")
	}
}
