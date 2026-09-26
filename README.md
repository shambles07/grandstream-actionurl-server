# grandstream-actionurl-server

A backend that receives Grandstream **Action URL** events from phones and stores them in SQLite. It also includes a provisioning tool that programs those Action URLs onto phones, either as XML config files or live over the phone's SSH CLI.

Everything is written in Go with no cgo, so each tool builds as a single static binary.

| Binary | Purpose |
|---|---|
| `gsactiond` | HTTP server. Phones send events to it, it writes them to SQLite, and it serves a read-only JSON API plus a live SSE stream for a future web UI. |
| `gsprov` | Provisioning CLI. Builds Action URLs and writes them to phones as XML (P-code or v2 alias format), CLI command lists, or a live SSH session with read-back checks. |
| `scripts/push-actionurl.exp` | A classic `expect` alternative to `gsprov ssh`, for one phone at a time. |

## Quick start

```sh
go build -o bin/ ./cmd/...

# 1. Run the backend
export GSACTION_TOKEN=$(openssl rand -hex 16)
./bin/gsactiond -addr :8080 -db /var/lib/gsactiond/events.db -token "$GSACTION_TOKEN"

# 2a. Program phones over SSH (P-codes, verified, committed)
export GSPROV_PASSWORD='phone-admin-password'
./bin/gsprov ssh -server http://10.0.0.5:8080 -token "$GSACTION_TOKEN" \
    -hosts 10.0.0.20,10.0.0.21            # or -hosts-file phones.txt

# 2b. ...or generate provisioning files for your provisioning server
./bin/gsprov xml -server http://10.0.0.5:8080 -token "$GSACTION_TOKEN" \
    -format alias -macs macs.txt -dir /srv/tftp
```

## Events

The table covers all 20 events in the ActionURL guide. P-codes come from the Action URL P-value tables in the GXP16xx, GXP21xx and GRP261x admin guides. Aliases come from the GXP21xx 1.0.11.x config template. Run `gsprov events` to print this table.

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
| `syslog_on` | Syslog On | Open Syslog | P8330 | `ons.actionUrl.openSyslog` ⚠ |
| `syslog_off` | Syslog Off | Close Syslog | P8331 | `ons.actionUrl.closedSyslog` ⚠ |
| `boot_completed` | Booting Completed | Setup Completed | P8304 | `ons.actionUrl.setupCompleted` |
| `blind_transfer` | Blind Transferring | Blind Transfer | P8320 | `ons.actionUrl.blindTransfer` |
| `attended_transfer` | Attended Transferring | Attended Transfer | P8321 | `ons.actionUrl.attendedTransfer` |
| `registered` | Registration | Registered | P8305 | `ons.actionUrl.registered` |
| `unregistered` | Sign Off | Unregistered | P8306 | `ons.actionUrl.unregistered` |

⚠ I couldn't find the two syslog aliases in any published template, so I guessed them from the pattern of the other aliases. `gsprov` prints a warning when it emits them. Check them against your model's config template, or use `-format pcode`, which is verified for every event.

## Dynamic variables

Each generated URL passes all 15 variables as query parameters. The phone fills in the values before it sends the request:

```
http://10.0.0.5:8080/actionurl/incoming_call?phone_ip=$phone_ip&mac=$mac&product=$product
  &program_version=$program_version&hardware_version=$hardware_version&language=$language
  &local=$local&display_local=$display_local&remote=$remote&display_remote=$display_remote
  &call-id=$call-id&active_user=$active_user&active_host=$active_host&duration=$duration
  &calldirection=$calldirection&token=...
```

Use `-vars mac,call-id,remote,...` to send fewer variables. This matters if a firmware limits how long a config value can be, which `gsprov ssh -verify` will detect.

## How the server handles phone requests

- **Two URL styles.** It accepts the generated `/actionurl/<event>?k=v&...` form and the guide's `server/<path>/k=v&...` form.
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

If `-api-token` is set, every endpoint requires `Authorization: Bearer <token>`.

| Endpoint | Returns |
|---|---|
| `GET /api/v1/catalog` | Events (with P-codes and aliases) and variables |
| `GET /api/v1/phones` | All phones, most recently seen first |
| `GET /api/v1/phones/{mac}` | One phone |
| `GET /api/v1/phones/{mac}/events` | That phone's events |
| `GET /api/v1/phones/{mac}/calls` | That phone's calls |
| `GET /api/v1/events?mac=&event=&call_id=&since=&until=&limit=&before_id=` | Events, newest first. Paginate with `next_before_id`. `since` and `until` take RFC 3339 times or durations like `24h` / `7d`. |
| `GET /api/v1/events/stats?since=7d` | Count of events per type |
| `GET /api/v1/events/stream?mac=&event=` | Server-Sent Events stream of new events as they arrive |
| `GET /api/v1/calls?mac=&call_id=&state=open\|ringing\|active\|held\|ended\|missed` | Calls |
| `GET /healthz` | Liveness check |

## gsactiond flags

Each flag can also be set with the environment variable shown.

| Flag | Env | Default | |
|---|---|---|---|
| `-addr` | `GSACTION_ADDR` | `:8080` | listen address |
| `-db` | `GSACTION_DB` | `gsactiond.db` | SQLite path |
| `-token` | `GSACTION_TOKEN` | | phones must send `token=` (recommended) |
| `-api-token` | `GSACTION_API_TOKEN` | | bearer token for `/api` |
| `-tls-cert`, `-tls-key` | `GSACTION_TLS_CERT/KEY` | | serve HTTPS (TLS 1.2 minimum, for older phones) |
| `-retention` | `GSACTION_RETENTION` | `90d` | `0` keeps data forever |
| `-log-level` | `GSACTION_LOG_LEVEL` | `info` | `debug` logs every event |

## Provisioning

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
gsprov cli -server http://10.0.0.5:8080 -token "$GSACTION_TOKEN" > actionurl.cmds
GSPROV_PASSWORD=… ./scripts/push-actionurl.exp 10.0.0.20 admin actionurl.cmds
```

## Development

```sh
go test -race ./...
```

The tests cover the full ingest path over real TCP, including raw spaces in the request line and keep-alive connections. They also run the call-state logic, check that both XML formats are well-formed, and run SSH provisioning against a simulated Grandstream CLI. That includes a case where the phone truncates a value: the tool must detect it and refuse to commit.
