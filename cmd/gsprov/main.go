// Command gsprov programs Grandstream phones with Action URLs that point at
// gsactiond, either by generating XML provisioning files or by driving each
// phone's SSH CLI.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/shambles07/grandstream-actionurl-server/internal/actionurl"
	"github.com/shambles07/grandstream-actionurl-server/internal/provision"
)

const usage = `gsprov - program Grandstream Action URLs

Usage:
  gsprov events                      list supported events, P-codes, aliases and models
  gsprov print -server URL [flags]   print the value of every Action URL field, to copy
                                     into the web UI (use -model wp820 for WP8xx phones)
  gsprov xml   -server URL [flags]   write an XML provisioning file
  gsprov cli   -server URL [flags]   print SSH CLI "set" commands (for scripts/push-actionurl.exp)
  gsprov ssh   -server URL -hosts ... [flags]
                                     push settings to phones over SSH

Run "gsprov <command> -h" for flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "events":
		err = cmdEvents()
	case "print", "urls":
		err = cmdPrint(args)
	case "xml":
		err = cmdXML(args)
	case "cli":
		err = cmdCLI(args)
	case "ssh":
		err = cmdSSH(ctx, args)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gsprov:", err)
		os.Exit(1)
	}
}

// common holds the flags shared by every command that builds settings.
type common struct {
	server, token, format, vars, events string
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.server, "server", os.Getenv("GSPROV_SERVER"), "gsactiond base URL as the phone reaches it, e.g. http://10.0.0.5:8086")
	fs.StringVar(&c.token, "token", os.Getenv("GSACTION_TOKEN"), "shared token (must match gsactiond -token)")
	fs.StringVar(&c.format, "format", "pcode", "config key style: pcode (P8310) or alias (ons.actionUrl.incomingCall)")
	fs.StringVar(&c.vars, "vars", "", "comma-separated dynamic variables to include (default: all 15)")
	fs.StringVar(&c.events, "events", "", "comma-separated event slugs to configure (default: all 20)")
}

func (c *common) settings() (actionurl.Format, []actionurl.Setting, error) {
	f, err := actionurl.ParseFormat(c.format)
	if err != nil {
		return "", nil, err
	}
	evs, err := actionurl.SelectEvents(splitList(c.events))
	if err != nil {
		return "", nil, err
	}
	s, err := actionurl.BuildSettings(f, actionurl.URLOptions{
		Server: c.server, Token: c.token, Params: splitList(c.vars),
	}, evs)
	if err != nil {
		return "", nil, err
	}
	if f == actionurl.FormatAlias {
		for _, e := range evs {
			if !e.AliasVerified {
				fmt.Fprintf(os.Stderr, "warning: alias %s (%s) is not in a published Grandstream template; confirm it against your model's template\n", e.Alias, e.Name)
			}
		}
	}
	return f, s, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdEvents() error {
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SLUG\tEVENT\tWEB UI LABEL\tP-CODE\tV2 ALIAS")
	for _, e := range actionurl.Events {
		alias := e.Alias
		if alias != "" && !e.AliasVerified {
			alias += " (unverified)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Slug, e.Name, e.WebUIName, dash(e.PCode), dash(alias))
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "VARIABLE\tPARAM\tDESCRIPTION")
	for _, v := range actionurl.Variables {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", v.Token, v.Param, v.Description)
	}
	fmt.Fprintln(tw)
	fmt.Fprintln(tw, "MODEL\tPHONES\tWEB UI LOCATION\tXML/SSH")
	for _, m := range actionurl.Models {
		prov := "yes"
		if !m.Provisionable {
			prov = "no (web UI only)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.Name, m.Title, m.WebUIPath, prov)
	}
	return tw.Flush()
}

// printField is one row of "gsprov print" output.
type printField struct {
	Event string `json:"event"`
	Label string `json:"label"`
	PCode string `json:"pcode,omitempty"`
	Alias string `json:"alias,omitempty"`
	URL   string `json:"url"`
}

func cmdPrint(args []string) error {
	fs := flag.NewFlagSet("print", flag.ExitOnError)
	var c common
	c.register(fs)
	model := fs.String("model", "gxp", "phone family, for web UI labels and order: gxp or wp820 (see gsprov events)")
	kv := fs.Bool("kv", false, "print KEY=VALUE lines using -format keys (models with config keys only)")
	jsonOut := fs.Bool("json", false, "print a JSON array")
	fs.Parse(args)

	m, err := actionurl.LookupModel(*model)
	if err != nil {
		return err
	}
	f, err := actionurl.ParseFormat(c.format)
	if err != nil {
		return err
	}
	sel, err := m.Select(splitList(c.events))
	if err != nil {
		return err
	}
	opts := actionurl.URLOptions{Server: c.server, Token: c.token, Params: splitList(c.vars)}
	rows := make([]printField, 0, len(sel))
	for _, fld := range sel {
		u, err := actionurl.BuildURL(fld.Event, opts)
		if err != nil {
			return err
		}
		rows = append(rows, printField{
			Event: fld.Event.Slug, Label: fld.Label,
			PCode: fld.Event.PCode, Alias: fld.Event.Alias, URL: u,
		})
	}
	return writePrint(os.Stdout, m, f, rows, *kv, *jsonOut)
}

func writePrint(w io.Writer, m actionurl.Model, f actionurl.Format, rows []printField, kv, jsonOut bool) error {
	switch {
	case jsonOut:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(rows)

	case kv:
		if !m.Provisionable {
			return fmt.Errorf("model %s stores these URLs outside P-values, so there are no config keys; drop -kv", m.Name)
		}
		for _, r := range rows {
			key := r.PCode
			if f == actionurl.FormatAlias {
				key = r.Alias
			}
			fmt.Fprintf(w, "%s=%s\n", key, r.URL)
		}
		return nil
	}

	// Default: one block per web UI field, with the URL alone on its line
	// so it can be selected and pasted as-is.
	fmt.Fprintf(w, "# %s\n# Web UI: %s\n", m.Title, m.WebUIPath)
	if !m.Provisionable {
		fmt.Fprintln(w, "# These fields are not stored in P-values: enter them in the web UI.")
	}
	if len(m.Unsupported) > 0 {
		fmt.Fprintf(w, "# Not available on this model: %s\n", strings.Join(m.Unsupported, ", "))
	}
	for _, r := range rows {
		fmt.Fprintln(w)
		if m.Provisionable {
			fmt.Fprintf(w, "%s  [%s / %s]\n", r.Label, r.PCode, r.Alias)
		} else {
			fmt.Fprintln(w, r.Label)
		}
		fmt.Fprintln(w, r.URL)
	}
	return nil
}

func cmdXML(args []string) error {
	fs := flag.NewFlagSet("xml", flag.ExitOnError)
	var c common
	c.register(fs)
	mac := fs.String("mac", "", "phone MAC; embeds <mac> and names the file cfg<mac>.xml")
	macsFile := fs.String("macs", "", "file with one MAC per line; writes one cfg<mac>.xml per phone into -dir")
	dir := fs.String("dir", ".", "output directory for -mac / -macs")
	out := fs.String("o", "", "output file (default: stdout, or cfg<mac>.xml in -dir with -mac)")
	fs.Parse(args)

	f, settings, err := c.settings()
	if err != nil {
		return err
	}

	var macs []string
	if *mac != "" {
		macs = append(macs, *mac)
	}
	if *macsFile != "" {
		lines, err := readLines(*macsFile)
		if err != nil {
			return err
		}
		macs = append(macs, lines...)
	}

	if len(macs) == 0 {
		if *out == "" {
			return provision.WriteXML(os.Stdout, f, "", settings)
		}
		return writeXMLFile(*out, f, "", settings)
	}
	if *out != "" && len(macs) > 1 {
		return errors.New("-o cannot be used with more than one MAC; use -dir")
	}
	for _, m := range macs {
		m = normalizeMAC(m)
		if len(m) != 12 {
			return fmt.Errorf("invalid MAC %q", m)
		}
		path := *out
		if path == "" {
			path = filepath.Join(*dir, provision.ConfigFileName(m))
		}
		if err := writeXMLFile(path, f, m, settings); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "wrote", path)
	}
	return nil
}

func writeXMLFile(path string, f actionurl.Format, mac string, settings []actionurl.Setting) error {
	fh, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := provision.WriteXML(fh, f, mac, settings); err != nil {
		fh.Close()
		return err
	}
	return fh.Close()
}

func cmdCLI(args []string) error {
	fs := flag.NewFlagSet("cli", flag.ExitOnError)
	var c common
	c.register(fs)
	prefix := fs.Bool("pcode-prefix", false, `send "set P8310 ..." instead of "set 8310 ..."`)
	fs.Parse(args)
	f, settings, err := c.settings()
	if err != nil {
		return err
	}
	for _, cmd := range provision.CLICommands(settings, f, *prefix) {
		fmt.Printf("set %s %s\n", cmd.Key, cmd.Setting.Value)
	}
	return nil
}

func cmdSSH(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ssh", flag.ExitOnError)
	var c common
	c.register(fs)
	home, _ := os.UserHomeDir()
	var (
		hosts       = fs.String("hosts", "", "comma-separated phone addresses (host or host:port)")
		hostsFile   = fs.String("hosts-file", "", "file with one phone address per line")
		user        = fs.String("user", "admin", "SSH user")
		password    = fs.String("password", "", "SSH password (prefer GSPROV_PASSWORD env var)")
		keyFile     = fs.String("key", "", "SSH private key file (for phones with an uploaded public key)")
		knownHosts  = fs.String("known-hosts", filepath.Join(home, ".config", "gsprov", "known_hosts"), "known_hosts file for phone host keys")
		acceptNew   = fs.Bool("accept-new", true, "add host keys of phones not yet in -known-hosts (a changed key is always rejected)")
		insecure    = fs.Bool("insecure-ignore-host-key", false, "do not verify host keys (lab use only)")
		legacy      = fs.Bool("legacy-algorithms", false, "allow SHA-1 key exchange/ciphers required by old firmware")
		timeout     = fs.Duration("timeout", 15*time.Second, "timeout for connecting and for each CLI step")
		verify      = fs.Bool("verify", true, "read back each value with \"get\" and compare")
		commit      = fs.Bool("commit", true, "commit changes to flash (false = trial run lost on reboot)")
		reboot      = fs.Bool("reboot", false, "reboot the phone after committing")
		dryRun      = fs.Bool("dry-run", false, "print the CLI session instead of connecting")
		concurrency = fs.Int("concurrency", 8, "phones to program in parallel")
		prefix      = fs.Bool("pcode-prefix", false, `send "set P8310 ..." instead of "set 8310 ..."`)
		transcripts = fs.String("transcript-dir", "", "write each phone's raw CLI output to <dir>/<host>.log")
		jsonOut     = fs.Bool("json", false, "print results as JSON lines")
	)
	fs.Parse(args)

	f, settings, err := c.settings()
	if err != nil {
		return err
	}
	cmds := provision.CLICommands(settings, f, *prefix)

	targets := splitList(*hosts)
	if *hostsFile != "" {
		lines, err := readLines(*hostsFile)
		if err != nil {
			return err
		}
		targets = append(targets, lines...)
	}
	if len(targets) == 0 {
		return errors.New("no phones given; use -hosts or -hosts-file")
	}

	if *dryRun {
		for _, t := range targets {
			fmt.Printf("# %s (ssh %s@%s)\nconfig\n", t, *user, t)
			for _, cmd := range cmds {
				fmt.Printf("set %s %s\n", cmd.Key, cmd.Setting.Value)
				if *verify {
					fmt.Printf("get %s\n", cmd.Key)
				}
			}
			if *commit {
				fmt.Println("commit")
			}
			fmt.Println("exit")
			if *commit && *reboot {
				fmt.Println("reboot")
			}
		}
		return nil
	}

	if *password == "" {
		*password = os.Getenv("GSPROV_PASSWORD")
	}
	var signers []ssh.Signer
	if *keyFile != "" {
		pem, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		s, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return fmt.Errorf("parse %s: %w", *keyFile, err)
		}
		signers = append(signers, s)
	}
	if *password == "" && len(signers) == 0 {
		return errors.New("no credentials: set GSPROV_PASSWORD, -password or -key")
	}
	hkcb, err := provision.HostKeyPolicy(*knownHosts, *acceptNew, *insecure)
	if err != nil {
		return err
	}
	if *transcripts != "" {
		if err := os.MkdirAll(*transcripts, 0o700); err != nil {
			return err
		}
	}

	type result struct {
		Host    string                `json:"host"`
		OK      bool                  `json:"ok"`
		Error   string                `json:"error,omitempty"`
		Results []provision.SetResult `json:"results,omitempty"`
	}
	var (
		mu       sync.Mutex
		failures int
		wg       sync.WaitGroup
		sem      = make(chan struct{}, max(1, *concurrency))
	)
	report := func(r result) {
		mu.Lock()
		defer mu.Unlock()
		if !r.OK {
			failures++
		}
		if *jsonOut {
			b, _ := json.Marshal(r)
			fmt.Println(string(b))
			return
		}
		if r.OK {
			fmt.Printf("ok    %s  (%d settings)\n", r.Host, len(r.Results))
		} else {
			fmt.Printf("FAIL  %s  %s (%d settings applied before failure)\n", r.Host, r.Error, len(r.Results))
		}
	}

	for _, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(host string) {
			defer wg.Done()
			defer func() { <-sem }()
			o := provision.SSHOptions{
				User: *user, Password: *password, Signers: signers,
				HostKeyCallback: hkcb, Legacy: *legacy, Timeout: *timeout,
				Verify: *verify, Commit: *commit, Reboot: *reboot,
			}
			if *transcripts != "" {
				name := strings.NewReplacer(":", "_", "/", "_").Replace(host) + ".log"
				if fh, err := os.Create(filepath.Join(*transcripts, name)); err == nil {
					defer fh.Close()
					o.Transcript = fh
				}
			}
			res, err := provision.PushSSH(ctx, host, cmds, o)
			r := result{Host: host, OK: err == nil, Results: res}
			if err != nil {
				r.Error = err.Error()
			}
			report(r)
		}(t)
	}
	wg.Wait()
	if failures > 0 {
		return fmt.Errorf("%d of %d phones failed", failures, len(targets))
	}
	return nil
}

func readLines(path string) ([]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var out []string
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, strings.Fields(line)[0])
	}
	return out, sc.Err()
}

func normalizeMAC(s string) string {
	return strings.ToLower(strings.NewReplacer(":", "", "-", "", ".", "").Replace(strings.TrimSpace(s)))
}
