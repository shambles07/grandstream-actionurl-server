package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/shambles07/grandstream-actionurl-server/internal/actionurl"
)

// Command is one CLI line to send in the phone's CONFIG> mode.
type Command struct {
	Setting actionurl.Setting
	Key     string // key as typed on the CLI
}

// CLICommands converts settings into the keys used by the phone's SSH CLI
// "set <key> <value>" command. The CLI takes P-codes as bare numbers
// ("set 8310 ...") unless pcodePrefix is true.
func CLICommands(settings []actionurl.Setting, f actionurl.Format, pcodePrefix bool) []Command {
	out := make([]Command, len(settings))
	for i, s := range settings {
		key := s.Key
		if f == actionurl.FormatPCode && !pcodePrefix {
			key = s.Event.PCodeNumber()
		}
		out[i] = Command{Setting: s, Key: key}
	}
	return out
}

// SSHOptions controls an SSH provisioning session.
type SSHOptions struct {
	User     string
	Password string
	Signers  []ssh.Signer
	// HostKeyCallback verifies the phone's host key. See HostKeyPolicy.
	HostKeyCallback ssh.HostKeyCallback
	// Legacy enables SHA-1 / group1 key exchanges and ciphers that older
	// phone firmware still requires.
	Legacy bool
	// Timeout bounds the TCP dial and each individual expect step.
	Timeout time.Duration
	// Verify reads every value back with "get" and compares it.
	Verify bool
	// Commit writes the changes to flash. Without it the changes are lost
	// on the next reboot (useful for a trial run on a live phone).
	Commit bool
	// Reboot reboots the phone after a successful commit.
	Reboot bool
	// Transcript, if non-nil, receives the raw terminal output.
	Transcript io.Writer
}

// Grandstream CLI prompts look like "GS> ", "GXP2170> " and "CONFIG> ".
var (
	rePrompt       = regexp.MustCompile(`(?:^|\n)[^\n>]{0,40}> ?$`)
	reConfigPrompt = regexp.MustCompile(`(?i)(?:^|\n)\s*config> ?$`)
	reUser         = regexp.MustCompile(`(?i)(?:user ?name|login)\s*:\s*$`)
	rePass         = regexp.MustCompile(`(?i)password\s*:\s*$`)
	reConfirm      = regexp.MustCompile(`(?i)\((?:y/n|yes/no)\)\s*[:?]?\s*$`)
	reCLIError     = regexp.MustCompile(`(?i)\b(error|invalid|unknown|unsupported|not (?:found|allowed|supported)|fail(?:ed)?|denied)\b`)
	reCommitted    = regexp.MustCompile(`(?i)commit`)
)

// SetResult is the outcome for one setting on one phone.
type SetResult struct {
	Event    string `json:"event"`
	Key      string `json:"key"`
	Verified bool   `json:"verified"`
	Output   string `json:"output,omitempty"`
}

// PushSSH connects to addr (host or host:port), enters CONFIG mode, sets
// every command, optionally verifies and commits, and returns per-setting
// results.
func PushSSH(ctx context.Context, addr string, cmds []Command, o SSHOptions) ([]SetResult, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, "22")
	}
	if o.Timeout <= 0 {
		o.Timeout = 15 * time.Second
	}

	auth := []ssh.AuthMethod{}
	if len(o.Signers) > 0 {
		auth = append(auth, ssh.PublicKeys(o.Signers...))
	}
	if o.Password != "" {
		pw := o.Password
		auth = append(auth, ssh.Password(pw),
			ssh.KeyboardInteractive(func(_, _ string, qs []string, _ []bool) ([]string, error) {
				ans := make([]string, len(qs))
				for i := range qs {
					ans[i] = pw
				}
				return ans, nil
			}))
	}
	cfg := &ssh.ClientConfig{
		User:            o.User,
		Auth:            auth,
		HostKeyCallback: o.HostKeyCallback,
		Timeout:         o.Timeout,
	}
	if o.Legacy {
		sup, ins := ssh.SupportedAlgorithms(), ssh.InsecureAlgorithms()
		cfg.KeyExchanges = append(sup.KeyExchanges, ins.KeyExchanges...)
		cfg.Ciphers = append(sup.Ciphers, ins.Ciphers...)
		cfg.MACs = append(sup.MACs, ins.MACs...)
		cfg.HostKeyAlgorithms = append(sup.HostKeys, ins.HostKeys...)
	}

	d := net.Dialer{Timeout: o.Timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}
	client := ssh.NewClient(sc, chans, reqs)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}
	// A wide terminal keeps long URLs from being wrapped in the echo.
	if err := sess.RequestPty("vt100", 50, 1000, modes); err != nil {
		return nil, fmt.Errorf("request pty: %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := sess.Shell(); err != nil {
		return nil, fmt.Errorf("start shell: %w", err)
	}
	ex := newExpecter(stdout, stdin, o.Transcript)
	defer ex.close()

	// Some firmware authenticates again inside the shell.
	for attempts := 0; ; attempts++ {
		i, _, err := ex.expect(ctx, o.Timeout, rePrompt, reUser, rePass)
		if err != nil {
			return nil, fmt.Errorf("waiting for CLI prompt: %w", err)
		}
		if i == 0 {
			break
		}
		if attempts >= 3 {
			return nil, errors.New("CLI login failed")
		}
		if i == 1 {
			err = ex.send(o.User)
		} else {
			err = ex.send(o.Password)
		}
		if err != nil {
			return nil, err
		}
	}

	if err := ex.send("config"); err != nil {
		return nil, err
	}
	if _, out, err := ex.expect(ctx, o.Timeout, reConfigPrompt); err != nil {
		return nil, fmt.Errorf("entering config mode: %w", err)
	} else if reCLIError.MatchString(out) {
		return nil, fmt.Errorf("entering config mode: %s", strings.TrimSpace(out))
	}

	results := make([]SetResult, 0, len(cmds))
	for _, c := range cmds {
		r := SetResult{Event: c.Setting.Event.Slug, Key: c.Key}
		line := "set " + c.Key + " " + c.Setting.Value
		if err := ex.send(line); err != nil {
			return results, err
		}
		_, out, err := ex.expect(ctx, o.Timeout, reConfigPrompt)
		if err != nil {
			return results, fmt.Errorf("set %s: %w", c.Key, err)
		}
		reply := stripEcho(out, line)
		// Don't let words inside the echoed URL look like an error.
		if reCLIError.MatchString(strings.ReplaceAll(reply, c.Setting.Value, "")) {
			return results, fmt.Errorf("set %s rejected: %s", c.Key, strings.TrimSpace(reply))
		}
		r.Output = strings.TrimSpace(reply)

		if o.Verify {
			get := "get " + c.Key
			if err := ex.send(get); err != nil {
				return results, err
			}
			_, out, err := ex.expect(ctx, o.Timeout, reConfigPrompt)
			if err != nil {
				return results, fmt.Errorf("get %s: %w", c.Key, err)
			}
			got := stripEcho(out, get)
			if !strings.Contains(squash(got), squash(c.Setting.Value)) {
				return results, fmt.Errorf("verify %s: phone reports %q; the value may have been truncated (try fewer -vars)",
					c.Key, strings.TrimSpace(got))
			}
			r.Verified = true
		}
		results = append(results, r)
	}

	if o.Commit {
		if err := ex.send("commit"); err != nil {
			return results, err
		}
		_, out, err := ex.expect(ctx, o.Timeout, reConfigPrompt)
		if err != nil {
			return results, fmt.Errorf("commit: %w", err)
		}
		if reply := stripEcho(out, "commit"); reCLIError.MatchString(reply) || !reCommitted.MatchString(reply) {
			return results, fmt.Errorf("commit not confirmed: %q", strings.TrimSpace(reply))
		}
	}

	if err := ex.send("exit"); err != nil {
		return results, err
	}
	if _, _, err := ex.expect(ctx, o.Timeout, rePrompt); err != nil {
		return results, fmt.Errorf("leaving config mode: %w", err)
	}

	if o.Commit && o.Reboot {
		if err := ex.send("reboot"); err != nil {
			return results, err
		}
		// The phone may ask for confirmation or simply drop the connection.
		if i, _, err := ex.expect(ctx, o.Timeout, reConfirm); err == nil && i == 0 {
			ex.send("y")
		}
		return results, nil
	}
	ex.send("exit")
	return results, nil
}

// stripEcho removes the echoed command line and the trailing prompt from a
// command's output, leaving just the phone's reply.
func stripEcho(out, cmd string) string {
	if i := strings.Index(out, cmd); i >= 0 {
		out = out[i+len(cmd):]
	}
	if i := strings.LastIndex(out, "\n"); i >= 0 {
		out = out[:i]
	} else {
		out = ""
	}
	return out
}

// squash removes all whitespace so that terminal line-wrapping does not break
// comparisons.
func squash(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// HostKeyPolicy builds a host key callback.
//
//   - insecure: accept any key (lab use only).
//   - otherwise: verify against knownHostsFile; keys for hosts not yet in the
//     file are appended (like OpenSSH StrictHostKeyChecking=accept-new) when
//     acceptNew is set, and a changed key is always an error.
func HostKeyPolicy(knownHostsFile string, acceptNew, insecure bool) (ssh.HostKeyCallback, error) {
	if insecure {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	if err := os.MkdirAll(filepath.Dir(knownHostsFile), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(knownHostsFile, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	check, err := knownhosts.New(knownHostsFile)
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		mu.Lock()
		defer mu.Unlock()
		err := check(hostname, remote, key)
		var ke *knownhosts.KeyError
		if !errors.As(err, &ke) || len(ke.Want) > 0 || !acceptNew {
			return err
		}
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		af, ferr := os.OpenFile(knownHostsFile, os.O_APPEND|os.O_WRONLY, 0o600)
		if ferr != nil {
			return ferr
		}
		defer af.Close()
		_, ferr = fmt.Fprintln(af, line)
		return ferr
	}, nil
}
