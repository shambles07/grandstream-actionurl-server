# grandstream-actionurl-server

A backend that receives Grandstream **Action URL** events from phones and stores them in SQLite. It also includes a provisioning tool that programs those Action URLs onto phones, either as XML config files or live over the phone's SSH CLI.

Everything is written in Go with no C dependencies, so each tool builds as a single static binary that does not depend on the system's glibc (see [Building](#building)).

| Binary | Purpose |
|---|---|
| `gsactiond` | HTTP server. Phones send events to it, it writes them to SQLite, and it serves a read-only JSON API plus a live Server-Sent Events stream. |
| `gsprov` | Provisioning CLI. Builds Action URLs and writes them to phones as XML (P-code or v2 alias format), CLI command lists, or a live SSH session with read-back checks. It can also print every field's value to copy into the phone's web UI. |
| `scripts/push-actionurl.exp` | A classic `expect` alternative to `gsprov ssh`, for one phone at a time. |

## Quick start

```sh
make build          # static binaries in bin/ (see Building)

# 1. Run the backend (or install it as a service: see "Running under systemd")
export GSACTION_TOKEN=$(openssl rand -hex 16)
./bin/gsactiond -addr :8086 -db /var/lib/gsactiond/events.db -token "$GSACTION_TOKEN"

# 2a. Program phones over SSH (P-codes, verified, committed)
export GSPROV_PASSWORD='phone-admin-password'
./bin/gsprov ssh -server http://10.0.0.5:8086 -token "$GSACTION_TOKEN" \
    -hosts 10.0.0.20,10.0.0.21            # or -hosts-file phones.txt

# 2b. ...or generate provisioning files for your provisioning server
./bin/gsprov xml -server http://10.0.0.5:8086 -token "$GSACTION_TOKEN" \
    -format alias -macs macs.txt -dir /srv/tftp
```

In these examples `10.0.0.5` is the PBX host running `gsactiond`, and `10.0.0.20`/`10.0.0.21` are phones.

### What `-server` means

`-server` is the address of your running `gsactiond`, **written the way the phones will reach it**. `gsprov` never connects to it. It only copies that address into the start of every Action URL it programs onto the phones:

```
-server http://10.0.0.5:8086
        └────────┬─────────┘
http://10.0.0.5:8086/actionurl/incoming_call?mac=$mac&...
```

When the phone later has an incoming call, it requests that URL, so the phone is what connects to `-server`. Some things follow from that:

- **Use an address the phones can reach**, not the one you use from your workstation. If `gsactiond` is on the PBX, use the PBX's IP or hostname on the phone VLAN. `localhost` or `127.0.0.1` would make each phone send events to itself.
- **Include the scheme and port**: `http://host:8086`, or `https://host:8086` if `gsactiond` runs with TLS. The port must match the `gsactiond` listen port (8086 by default, or `ListenStream=` in the systemd socket).
- **A hostname works** only if the phones can resolve it through their own DNS.
- **If the address changes, reprovision.** The phones keep whatever URL was programmed. Re-run `gsprov` with the new `-server`, or put a stable DNS name in it from the start.
- **The `-token` value must match** the `gsactiond` token (`GSACTION_TOKEN`). It is added to each URL in the same way.

`-server` can also be set with the `GSPROV_SERVER` environment variable. Run `gsprov print -server ...` to see the exact URLs before you program any phones.

## Events

The first 20 rows are the events in Grandstream's ActionURL guide. P-codes come from the Action URL P-value tables in the GXP16xx, GXP21xx and GRP261x admin guides. Aliases come from the GXP21xx 1.0.11.x config template. The last three rows are events that only the WP8xx phones offer; they have no config keys (see [WP820](#wp820-and-other-web-ui-only-phones)). Run `gsprov events` to print this table.

| Slug | Guide name | Web UI label | P-code | v2 alias |
|---|---|---|---|---|
| `incoming_call` | Incoming Call | Incoming Call | P8310 | `ons.actionUrl.incomingCall` |
| `outgoing_call` | Outgoing Call | Outgoing Call | P8311 | `ons.actionUrl.outgoingCall` |
| `established_call` | Establish Call | Established Call | P8313 | `ons.actionUrl.establishedCall` |
| `terminated_call` | Terminate Call | Terminated Call | P8314 | `ons.actionUrl.terminatedCall` |
| `off_hook` | Off Hook | Off Hook | P8308 | `ons.actionUrl.offHook` |
| `on_hook` | On Hook | On Hook | P8309 | `ons.actionUrl.onHook` |
| `missed_call` | Missed Call | Missed Call | P8312 | `ons.actionUrl.missedCall` |
| `dnd_on` | DND On | Open DND | P8316 | `ons.actionUrl.openDnd` |
| `dnd_off` | DND Off | Close DND | P8317 | `ons.actionUrl.closedDnd` |
| `forward_on` | Call Forwarding On | Open Forward | P8318 | `ons.actionUrl.openForward` |
| `forward_off` | Call Forwarding Off | Close Forward | P8319 | `ons.actionUrl.closedForward` |
| `hold_call` | Hold Call | Hold Call | P8324 | `ons.actionUrl.holdCall` |
| `resume_call` | Resume Call | UnHold Call | P8325 | `ons.actionUrl.unholdCall` |
| `syslog_on` | Syslog On | Open Syslog | P8330 | `ons.actionUrl.openSyslog` |
| `syslog_off` | Syslog Off | Close Syslog | P8331 | `ons.actionUrl.closedSyslog` |
| `boot_completed` | Booting Completed | Setup Completed | P8304 | `ons.actionUrl.setupCompleted` |
| `blind_transfer` | Blind Transferring | Blind Transfer | P8320 | `ons.actionUrl.blindTransfer` |
| `attended_transfer` | Attended Transferring | Attended Transfer | P8321 | `ons.actionUrl.attendedTransfer` |
| `registered` | Registration | Registered | P8305 | `ons.actionUrl.registered` |
| `unregistered` | Sign Off | Unregistered | P8306 | `ons.actionUrl.unregistered` |
| `log_on` | – | Log On (WP8xx) | – | – |
| `log_off` | – | Log Off (WP8xx) | – | – |
| `panic_call` | – | SAFE/Panic Call (WP8xx) | – | – |

## Dynamic variables

Each generated URL passes all 15 variables as query parameters. The phone fills in the values before it sends the request:

```
http://10.0.0.5:8086/actionurl/incoming_call?phone_ip=$phone_ip&mac=$mac&product=$product
  &program_version=$program_version&hardware_version=$hardware_version&language=$language
  &local=$local&display_local=$display_local&remote=$remote&display_remote=$display_remote
  &call-id=$call-id&active_user=$active_user&active_host=$active_host&duration=$duration
  &calldirection=$calldirection&token=...
```

Use `-vars mac,call-id,remote,...` to send fewer variables. This matters if a firmware limits how long a config value can be, which `gsprov ssh -verify` will detect.

## How the server handles phone requests

- **Two URL styles.** It accepts the generated `/actionurl/<event>?k=v&...` form and the `server/<path>/k=v&...` form shown in Grandstream's ActionURL guide.
- **Raw spaces.** Phones can put display names such as `John Doe` into the request line without encoding them, and Go's `net/http` would reject that with a 400. A small listener wrapper percent-encodes those bytes before parsing, including on keep-alive connections.
- **`+` signs.** A leading `+` in an E.164 number is kept as `+`, not turned into a space.
- **Unfilled variables.** If an event has no value for a variable, the phone sends the literal placeholder (for example `remote=$remote`). These are stored as empty.
- **MAC formats.** `00:0B:82:…`, `00-0b-82-…` and `000b82…` are all treated as the same phone.
- **Unknown parameters** are kept in the event's `extra` JSON column.

## Storage

The database is SQLite in WAL mode, using the pure-Go `modernc.org/sqlite` driver. It has one writer connection and a pool of readers. Each event is written in a single transaction:

- **`events`**: the append-only log. Every request gets one row with every variable, the source IP and the raw query, with the token redacted.
- **`phones`**: the latest state for each MAC: identity, firmware, active account, and on/off flags for registered, DND, forwarding, syslog and off-hook. It also records the last boot time, first and last seen times, and the event count.
- **`calls`**: one row per (MAC, Call-ID). A call starts as ringing or dialing, then becomes active, may be held, and finally ends up ended or missed. The row also holds the direction, both parties, the transfer type, the answered and ended times, and the duration.

`-retention 90d` (the default) deletes events and finished calls older than 90 days every hour. Phone rows are never deleted.

## JSON API

The API is read-only and is served on the same port as the phone endpoint.

**Authentication.** The API token is optional:

- **With `-api-token` (`GSACTION_API_TOKEN`) set,** every `/api` request must send `Authorization: Bearer <token>`. Anything else gets `401`.
- **Without it,** the API is open to anyone who can reach the port. That is fine on a loopback-only or firewalled host. Otherwise set a token, because the API exposes phone IPs, extensions and call history.

The phone token (`-token`) is separate: it protects only `/actionurl/`, and the API never accepts it. `/healthz` never requires a token.

| Endpoint | Returns |
|---|---|
| `GET /api/v1/catalog` | Events (with P-codes and aliases) and variables |
| `GET /api/v1/phones` | All phones, most recently seen first |
| `GET /api/v1/phones/{mac}` | One phone |
| `GET /api/v1/phones/{mac}/events` | That phone's events (same filters as `/events`) |
| `GET /api/v1/phones/{mac}/calls` | That phone's calls (same filters as `/calls`) |
| `GET /api/v1/events?mac=&event=&call_id=&since=&until=&limit=&before_id=` | Events, newest first. `limit` defaults to 100 (max 1000). Paginate with `next_before_id`. `since` and `until` take RFC 3339 times or durations like `24h` / `7d`. |
| `GET /api/v1/events/stats?since=7d` | Count of events per type |
| `GET /api/v1/events/stream?mac=&event=` | Server-Sent Events stream of new events as they arrive |
| `GET /api/v1/calls?mac=&call_id=&state=&limit=` | Calls, most recently updated first. `state` is `open` (not yet ended) or one of `ringing`, `dialing`, `active`, `held`, `ended`, `missed`. |
| `GET /healthz` | Liveness check (no auth) |

MACs are accepted in any format (`00:0B:82:AA:BB:CC`, `00-0b-82-aa-bb-cc`, `000b82aabbcc`) and always returned as 12 lowercase hex digits.

### Querying with curl

The examples assume the following variables. If no API token is set, leave out `-H "$AUTH"`.

```sh
API=http://10.0.0.5:8086/api/v1
AUTH="Authorization: Bearer $GSACTION_API_TOKEN"
```

Quote URLs that contain `?` or `&` so the shell doesn't interpret them.

**All phones:**

```sh
curl -s -H "$AUTH" "$API/phones"
```

**One phone:**

```sh
curl -s -H "$AUTH" "$API/phones/00:0B:82:AA:BB:CC"
```

```json
{
  "mac": "000b82aabbcc",
  "phone_ip": "10.0.0.20",
  "product": "GXP2170",
  "program_version": "1.0.11.79",
  "active_user": "1001",
  "active_host": "pbx.example.com",
  "source_ip": "10.0.0.20",
  "registered": true,
  "dnd": true,
  "first_seen_at": "2026-09-26T18:05:16.187Z",
  "last_seen_at": "2026-09-26T18:05:16.212Z",
  "last_event": "dnd_on",
  "event_count": 4
}
```

State flags (`registered`, `dnd`, `forwarding`, `syslog`, `off_hook`) are left out until the phone has sent an event that sets them.

**Calls in progress:**

```sh
curl -s -H "$AUTH" "$API/calls?state=open"
```

```json
{
  "calls": [
    {
      "id": 1,
      "mac": "000b82aabbcc",
      "call_id": "8f2c1e@10.0.0.20",
      "direction": "incoming",
      "local": "1001",
      "remote": "+15551234567",
      "display_remote": "John Doe",
      "active_user": "1001",
      "active_host": "pbx.example.com",
      "state": "active",
      "started_at": "2026-09-26T18:05:16.195Z",
      "answered_at": "2026-09-26T18:05:16.204Z",
      "updated_at": "2026-09-26T18:05:16.204Z"
    }
  ]
}
```

**Recent calls on one phone:**

```sh
curl -s -H "$AUTH" "$API/phones/000b82aabbcc/calls?limit=10"
```

**Every event for one call:**

```sh
curl -s -H "$AUTH" "$API/events?call_id=8f2c1e@10.0.0.20"
```

**Missed calls in the last 24 hours:**

```sh
curl -s -H "$AUTH" "$API/events?event=missed_call&since=24h"
```

**Events in a time window:**

```sh
curl -s -H "$AUTH" "$API/events?since=2026-09-01T00:00:00Z&until=2026-09-02T00:00:00Z"
```

**Paging through events.** Each response includes `next_before_id`. Pass it back as `before_id` to get the next (older) page:

```sh
curl -s -H "$AUTH" "$API/events?limit=500"
curl -s -H "$AUTH" "$API/events?limit=500&before_id=12345"
```

**Event counts per type for the last week:**

```sh
curl -s -H "$AUTH" "$API/events/stats?since=7d"
```

**Supported events and variables:**

```sh
curl -s -H "$AUTH" "$API/catalog"
```

**Watch events live.** `-N` turns off curl's output buffering. Filter with `mac=` and/or `event=`:

```sh
curl -sN -H "$AUTH" "$API/events/stream?event=incoming_call"
```

```
: connected

id: 5
event: incoming_call
data: {"id":5,"received_at":"2026-09-26T18:05:17.276Z","event":"incoming_call","mac":"000b82aabbcc","remote":"2000","call_id":"x9",...}
```

Lines starting with `:` are comments, and a `: keepalive` line is sent every 25 seconds.

**Combining with `jq`:**

```sh
# phones with DND on
curl -s -H "$AUTH" "$API/phones" | jq -r '.phones[] | select(.dnd) | .mac'

# phones not heard from in the last day (GNU date)
curl -s -H "$AUTH" "$API/phones" |
  jq -r --arg t "$(date -u -d '1 day ago' +%FT%TZ)" '.phones[] | select(.last_seen_at < $t) | "\(.mac) \(.phone_ip) \(.last_seen_at)"'

# firmware versions in use
curl -s -H "$AUTH" "$API/phones" | jq -r '.phones[] | "\(.product) \(.program_version)"' | sort | uniq -c
```

**Errors** return a non-2xx status with `{"error": "..."}`. Use `curl -f` to make curl exit non-zero on them, which is useful in scripts.

## gsactiond flags

Each flag can also be set with the environment variable shown.

| Flag | Env | Default | |
|---|---|---|---|
| `-addr` | `GSACTION_ADDR` | `:8086` | listen address (ignored when started by `gsactiond.socket`) |
| `-db` | `GSACTION_DB` | `gsactiond.db` | SQLite path |
| `-token` | `GSACTION_TOKEN` | | phones must send `token=` (recommended) |
| `-api-token` | `GSACTION_API_TOKEN` | | bearer token for `/api` |
| `-tls-cert`, `-tls-key` | `GSACTION_TLS_CERT/KEY` | | serve HTTPS (TLS 1.2 minimum, for older phones) |
| `-retention` | `GSACTION_RETENTION` | `90d` | `0` keeps data forever |
| `-log-level` | `GSACTION_LOG_LEVEL` | `info` | `debug` logs every event |

## Running under systemd

`deploy/systemd/` contains:

| File | Installed to | Purpose |
|---|---|---|
| `gsactiond.socket` | `/etc/systemd/system/` | systemd listens on TCP **8086** and starts `gsactiond` on the first phone request |
| `gsactiond.service` | `/etc/systemd/system/` | runs `/usr/local/bin/gsactiond` as a sandboxed, unprivileged user |
| `gsactiond.env` | `/etc/gsactiond/` (mode 0600) | tokens, retention and log level |

```sh
make build                      # as your user
sudo make install               # binaries, units, and an env file (an existing one is never overwritten)
sudoedit /etc/gsactiond/gsactiond.env     # set GSACTION_TOKEN (openssl rand -hex 16)
sudo systemctl daemon-reload
sudo systemctl enable --now gsactiond.socket
# optional: also start the service at boot instead of on the first request
sudo systemctl enable gsactiond.service
```

Useful commands:

- Check status: `systemctl status gsactiond.socket gsactiond.service`
- Follow the logs: `journalctl -u gsactiond -f`
- Quick check (starts the service if it isn't already running): `curl http://localhost:8086/healthz`

**Socket or no socket.** The service works either way.

- **With `gsactiond.socket` enabled,** systemd owns the port and hands it to `gsactiond`, which ignores `-addr`/`GSACTION_ADDR`. The port stays open across restarts and upgrades (`systemctl restart gsactiond`), so phones don't get "connection refused" while the service is down; their requests wait in the socket backlog.
- **Without the socket,** `gsactiond` binds `GSACTION_ADDR` (default `:8086`) itself. `gsactiond` reads the socket using the standard `LISTEN_FDS` protocol, with no libsystemd dependency, so the binary stays static.

**Changing the port or address.** Run `sudo systemctl edit gsactiond.socket` and add:

```ini
[Socket]
ListenStream=
ListenStream=10.0.0.5:8086
```

The empty `ListenStream=` line clears the default before the new one is added. After changing the port, reprovision the phones with a matching `-server`.

**Database location.** The database is at `/var/lib/gsactiond/gsactiond.db`. `DynamicUser=` runs the service as a transient user, and `StateDirectory=` gives that user ownership of this directory; on disk it is `/var/lib/private/gsactiond`, and `/var/lib/gsactiond` is a symlink to it. Back it up with `sqlite3 /var/lib/gsactiond/gsactiond.db ".backup /root/gsactiond.bak"`; this is safe while the service is running.

**HTTPS.** Uncomment the `LoadCredential=` lines in the service (via `systemctl edit gsactiond.service`), add the `-tls-cert`/`-tls-key` flags shown there to `ExecStart`, and use `https://` in `gsprov -server`.

**Firewall.** Allow TCP 8086 from the phone subnets only, for example:

```sh
sudo ufw allow from 10.0.0.0/24 to any port 8086 proto tcp
```

## Provisioning

| Method | Command | Phones |
|---|---|---|
| Print values to copy into the web UI | `gsprov print` | any, and the only option for WP8xx |
| XML provisioning file | `gsprov xml` | GXP16xx / GXP21xx / GRP26xx |
| Live over SSH | `gsprov ssh`, or `scripts/push-actionurl.exp` | GXP16xx / GXP21xx / GRP26xx |

### Printing values (`gsprov print`)

`gsprov print` shows the value for each Action URL field, using the phone's web UI labels and order. Nothing is sent to any phone. Each URL is on a line of its own so you can select and paste it:

```sh
gsprov print -server http://10.0.0.5:8086 -token "$GSACTION_TOKEN"
```

```
# GXP16xx / GXP21xx / GRP26xx
# Web UI: Settings > Outbound Notification > Action URL
# Not available on this model: log_on, log_off, panic_call

Setup Completed  [P8304 / ons.actionUrl.setupCompleted]
http://10.0.0.5:8086/actionurl/boot_completed?phone_ip=$phone_ip&mac=$mac&...&token=...

Registered  [P8305 / ons.actionUrl.registered]
http://10.0.0.5:8086/actionurl/registered?phone_ip=$phone_ip&mac=$mac&...&token=...
...
```

Options:

| Flag | Effect |
|---|---|
| `-model gxp\|wp820` | Which phone family's labels, order and fields to show (default `gxp`). |
| `-events a,b,...` | Only these events, still in web UI order. |
| `-vars a,b,...` | Fewer dynamic variables per URL, for shorter values. |
| `-kv` | `KEY=VALUE` lines, keyed by P-code, or by alias with `-format alias`. Only for models with config keys. |
| `-json` | A JSON array of `{event, label, pcode, alias, url}`. |

`gsprov urls` still works as another name for `gsprov print`.

### WP820 and other web-UI-only phones

The WP820 (and the rest of the WP8xx family) handles Action URLs differently from the desk phones:

- **No config keys.** Grandstream's WP820 config template marks the Event Notification section "Not stored in pvalue". These URLs have no P-codes or aliases, so `gsprov xml` and `gsprov ssh` can't set them. They have to be entered by hand in the web UI.
- **Different location.** The fields are under **Maintenance > Event Notification**, not Settings > Outbound Notification as on the GXP phones.
- **Different events.** The WP820 has no Off Hook, On Hook or Syslog events. It adds Log On, Log Off and SAFE/Panic Call, which `gsactiond` records as `log_on`, `log_off` and `panic_call`.

Print the values with the WP820 labels and order:

```sh
gsprov print -model wp820 -server http://10.0.0.5:8086 -token "$GSACTION_TOKEN"
```

```
# WP820 / WP8xx
# Web UI: Maintenance > Event Notification
# These fields are not stored in P-values: enter them in the web UI.
# Not available on this model: off_hook, on_hook, syslog_on, syslog_off

Bootup Completed
http://10.0.0.5:8086/actionurl/boot_completed?phone_ip=$phone_ip&mac=$mac&...&token=...

Incoming Call
http://10.0.0.5:8086/actionurl/incoming_call?phone_ip=$phone_ip&mac=$mac&...&token=...
...
```

Paste each URL into the field with the same label, then save. Every phone gets the same values, because the phone fills in `$mac`, `$phone_ip` and the other variables itself.

### XML

```sh
gsprov xml -server URL [-format pcode|alias] [-mac MAC | -macs FILE -dir DIR] [-o FILE]
```

- `pcode` writes `<config version="1"><P8310>…</P8310>`.
- `alias` writes `<config version="2"><item name="ons.actionUrl.incomingCall">…</item>`.
- `&` is escaped as `&amp;`.
- With `-mac`, the file includes `<mac>` and is named `cfg<mac>.xml`, the name phones request from the provisioning server.

### SSH (`gsprov ssh`)

This drives the phone's CLI the same way a person would, using a built-in expect-style engine on top of `golang.org/x/crypto/ssh`. It does not need `expect` or `ssh` installed:

```
GS> config
CONFIG> set 8310 http://…/actionurl/incoming_call?…
CONFIG> get 8310          # -verify (default): compares the stored value to what was sent
CONFIG> commit            # -commit (default); -commit=false for a trial run
CONFIG> exit
GS> reboot                # only with -reboot
```

Other behavior:

- **Parallel.** Several phones are programmed at once (`-concurrency 8`), and the tool prints one result line per phone (`-json` for JSON lines).
- **Stops on failure.** If a phone rejects a `set`, or a read-back shows a truncated value, the tool stops before `commit`, so the phone is left unchanged.
- **Host keys.** Keys are checked against `~/.config/gsprov/known_hosts`. New phones are added automatically (like OpenSSH's `accept-new`), and a changed key is always rejected.
- **Old firmware.** Use `-legacy-algorithms` if a phone only supports SHA-1 key exchange.
- **Debugging.** `-transcript-dir DIR` saves each phone's raw session output. `-dry-run` prints the commands without connecting.
- **Key format.** P-codes are sent as bare numbers (`set 8310 …`), the format the Grandstream CLI documents. Use `-pcode-prefix` to send `P8310` instead, or `-format alias` to send alias names.

### expect script

```sh
gsprov cli -server http://10.0.0.5:8086 -token "$GSACTION_TOKEN" > actionurl.cmds
GSPROV_PASSWORD=… ./scripts/push-actionurl.exp 10.0.0.20 admin actionurl.cmds
```

## Building

`make build` produces fully static binaries with no glibc (or any libc) dependency, so a binary built on one distro runs on any Linux of the same CPU architecture, including Alpine/musl and old enterprise releases.

The code itself needs no C: the SQLite driver (`modernc.org/sqlite`) is SQLite translated to Go. The only thing that would pull in glibc is Go's standard `net` package, which by default uses the C library's DNS resolver when cgo is available. The Makefile sets `CGO_ENABLED=0`, so Go uses its built-in resolver instead and links everything statically. If you build by hand, set it yourself:

```sh
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/ ./cmd/...
file bin/gsactiond    # ... statically linked
```

A plain `go build` without `CGO_ENABLED=0` still works but links against the build machine's glibc (2.34+ on a current distro), and the result fails with `GLIBC_x.yy not found` on older systems.

The pure-Go resolver reads `/etc/resolv.conf` and `/etc/hosts` directly. It does not consult NSS modules (`/etc/nsswitch.conf` entries such as LDAP or mDNS); normal DNS names and IP addresses are unaffected.

`make dist` cross-compiles static binaries for `linux/amd64`, `linux/arm64`, `linux/arm` (v7) and `linux/386` into `dist/<version>/`. Override the list with `PLATFORMS="linux/amd64 windows/amd64 darwin/arm64"`. No cross toolchain is needed.

## Development

```sh
make test            # or: go test ./...
go test -race ./...  # the race detector needs cgo, so run it with CGO_ENABLED unset
```

The tests cover the full ingest path over real TCP, including raw spaces in the request line and keep-alive connections. They also run the call-state logic, check that both XML formats are well-formed, and run SSH provisioning against a simulated Grandstream CLI. That includes a case where the phone truncates a value: the tool must detect it and refuse to commit.
