// Package store persists Action URL events, per-phone state and per-call
// state in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no cgo required
)

// timeFormat is fixed-width so lexical order equals chronological order.
const timeFormat = "2006-01-02T15:04:05.000Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeFormat) }

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps two handles to the same SQLite database: a single-connection
// writer (SQLite allows one writer at a time) and a pooled reader.
type Store struct {
	w *sql.DB
	r *sql.DB
}

// Open opens (creating if needed) the database at path and applies
// migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := func(pragmas ...string) string {
		v := url.Values{}
		for _, p := range pragmas {
			v.Add("_pragma", p)
		}
		return "file:" + path + "?" + v.Encode()
	}

	w, err := sql.Open("sqlite", dsn("journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)"))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)

	r, err := sql.Open("sqlite", dsn("busy_timeout(5000)", "query_only(1)"))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)

	s := &Store{w: w, r: r}
	// Migrating first also creates the file and switches it to WAL before
	// any reader connects.
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes both database handles.
func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Event is one received Action URL request.
type Event struct {
	ID              int64             `json:"id"`
	ReceivedAt      time.Time         `json:"received_at"`
	Event           string            `json:"event"`
	SourceIP        string            `json:"source_ip,omitempty"`
	MAC             string            `json:"mac,omitempty"`
	PhoneIP         string            `json:"phone_ip,omitempty"`
	Product         string            `json:"product,omitempty"`
	ProgramVersion  string            `json:"program_version,omitempty"`
	HardwareVersion string            `json:"hardware_version,omitempty"`
	Language        string            `json:"language,omitempty"`
	Local           string            `json:"local,omitempty"`
	DisplayLocal    string            `json:"display_local,omitempty"`
	Remote          string            `json:"remote,omitempty"`
	DisplayRemote   string            `json:"display_remote,omitempty"`
	CallID          string            `json:"call_id,omitempty"`
	ActiveUser      string            `json:"active_user,omitempty"`
	ActiveHost      string            `json:"active_host,omitempty"`
	Duration        *int64            `json:"duration,omitempty"`
	CallDirection   string            `json:"call_direction,omitempty"`
	Extra           map[string]string `json:"extra,omitempty"`
	RawQuery        string            `json:"raw_query,omitempty"`
}

// Phone is the latest known state of one phone.
type Phone struct {
	MAC             string    `json:"mac"`
	PhoneIP         string    `json:"phone_ip,omitempty"`
	Product         string    `json:"product,omitempty"`
	ProgramVersion  string    `json:"program_version,omitempty"`
	HardwareVersion string    `json:"hardware_version,omitempty"`
	Language        string    `json:"language,omitempty"`
	ActiveUser      string    `json:"active_user,omitempty"`
	ActiveHost      string    `json:"active_host,omitempty"`
	SourceIP        string    `json:"source_ip,omitempty"`
	Registered      *bool     `json:"registered,omitempty"`
	DND             *bool     `json:"dnd,omitempty"`
	Forwarding      *bool     `json:"forwarding,omitempty"`
	Syslog          *bool     `json:"syslog,omitempty"`
	OffHook         *bool     `json:"off_hook,omitempty"`
	LastBootAt      string    `json:"last_boot_at,omitempty"`
	FirstSeenAt     time.Time `json:"first_seen_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	LastEvent       string    `json:"last_event"`
	EventCount      int64     `json:"event_count"`
}

// Call is the tracked state of one call on one phone.
type Call struct {
	ID            int64  `json:"id"`
	MAC           string `json:"mac"`
	CallID        string `json:"call_id"`
	Direction     string `json:"direction,omitempty"`
	CallDirection string `json:"call_direction,omitempty"`
	Local         string `json:"local,omitempty"`
	DisplayLocal  string `json:"display_local,omitempty"`
	Remote        string `json:"remote,omitempty"`
	DisplayRemote string `json:"display_remote,omitempty"`
	ActiveUser    string `json:"active_user,omitempty"`
	ActiveHost    string `json:"active_host,omitempty"`
	State         string `json:"state"`
	Transfer      string `json:"transfer,omitempty"`
	StartedAt     string `json:"started_at"`
	AnsweredAt    string `json:"answered_at,omitempty"`
	EndedAt       string `json:"ended_at,omitempty"`
	Duration      *int64 `json:"duration,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

// phoneFlag maps an event slug to the phones column it sets and the value.
var phoneFlag = map[string]struct {
	col string
	val int
}{
	"registered":   {"registered", 1},
	"unregistered": {"registered", 0},
	"dnd_on":       {"dnd", 1},
	"dnd_off":      {"dnd", 0},
	"forward_on":   {"forwarding", 1},
	"forward_off":  {"forwarding", 0},
	"syslog_on":    {"syslog", 1},
	"syslog_off":   {"syslog", 0},
	"off_hook":     {"off_hook", 1},
	"on_hook":      {"off_hook", 0},
}

// Record stores one event and updates the derived phone and call state in a
// single transaction. It sets e.ID on success.
func (s *Store) Record(ctx context.Context, e *Event) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var extra any
	if len(e.Extra) > 0 {
		b, _ := json.Marshal(e.Extra)
		extra = string(b)
	}
	now := fmtTime(e.ReceivedAt)

	res, err := tx.ExecContext(ctx, `
INSERT INTO events (received_at, event, source_ip, mac, phone_ip, product,
	program_version, hardware_version, language, local, display_local, remote,
	display_remote, call_id, active_user, active_host, duration, call_direction,
	extra, raw_query)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		now, e.Event, ns(e.SourceIP), ns(e.MAC), ns(e.PhoneIP), ns(e.Product),
		ns(e.ProgramVersion), ns(e.HardwareVersion), ns(e.Language), ns(e.Local),
		ns(e.DisplayLocal), ns(e.Remote), ns(e.DisplayRemote), ns(e.CallID),
		ns(e.ActiveUser), ns(e.ActiveHost), e.Duration, ns(e.CallDirection),
		extra, ns(e.RawQuery))
	if err != nil {
		return err
	}
	if e.ID, err = res.LastInsertId(); err != nil {
		return err
	}

	if e.MAC != "" {
		if err := upsertPhone(ctx, tx, e, now); err != nil {
			return fmt.Errorf("phone: %w", err)
		}
		if e.CallID != "" {
			if err := upsertCall(ctx, tx, e, now); err != nil {
				return fmt.Errorf("call: %w", err)
			}
		}
	}
	return tx.Commit()
}

func upsertPhone(ctx context.Context, tx *sql.Tx, e *Event, now string) error {
	// Identity fields keep their previous value when an event omits them.
	_, err := tx.ExecContext(ctx, `
INSERT INTO phones (mac, phone_ip, product, program_version, hardware_version,
	language, active_user, active_host, source_ip, first_seen_at, last_seen_at,
	last_event, event_count)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,1)
ON CONFLICT (mac) DO UPDATE SET
	phone_ip         = COALESCE(excluded.phone_ip, phone_ip),
	product          = COALESCE(excluded.product, product),
	program_version  = COALESCE(excluded.program_version, program_version),
	hardware_version = COALESCE(excluded.hardware_version, hardware_version),
	language         = COALESCE(excluded.language, language),
	active_user      = COALESCE(excluded.active_user, active_user),
	active_host      = COALESCE(excluded.active_host, active_host),
	source_ip        = COALESCE(excluded.source_ip, source_ip),
	last_seen_at     = excluded.last_seen_at,
	last_event       = excluded.last_event,
	event_count      = event_count + 1`,
		e.MAC, ns(e.PhoneIP), ns(e.Product), ns(e.ProgramVersion),
		ns(e.HardwareVersion), ns(e.Language), ns(e.ActiveUser), ns(e.ActiveHost),
		ns(e.SourceIP), now, now, e.Event)
	if err != nil {
		return err
	}
	if f, ok := phoneFlag[e.Event]; ok {
		// f.col comes from the fixed phoneFlag table, never from input.
		if _, err := tx.ExecContext(ctx,
			"UPDATE phones SET "+f.col+" = ? WHERE mac = ?", f.val, e.MAC); err != nil {
			return err
		}
	}
	if e.Event == "boot_completed" {
		if _, err := tx.ExecContext(ctx,
			"UPDATE phones SET last_boot_at = ?, off_hook = 0 WHERE mac = ?", now, e.MAC); err != nil {
			return err
		}
	}
	return nil
}

// callTransition describes how an event changes a call row.
type callTransition struct {
	state     string // new state; "" leaves it unchanged
	direction string // set direction if known from the event type
	answered  bool   // stamp answered_at
	ended     bool   // stamp ended_at
	transfer  string // record transfer type
}

var callTransitions = map[string]callTransition{
	"incoming_call":     {state: "ringing", direction: "incoming"},
	"outgoing_call":     {state: "dialing", direction: "outgoing"},
	"established_call":  {state: "active", answered: true},
	"hold_call":         {state: "held"},
	"resume_call":       {state: "active"},
	"blind_transfer":    {transfer: "blind"},
	"attended_transfer": {transfer: "attended"},
	"missed_call":       {state: "missed", direction: "incoming", ended: true},
	"terminated_call":   {state: "ended", ended: true},
}

func upsertCall(ctx context.Context, tx *sql.Tx, e *Event, now string) error {
	t, ok := callTransitions[e.Event]
	if !ok {
		// Events such as on_hook may carry a Call-ID. They don't change call
		// state, and must not create a call, but refresh a known call's details.
		_, err := tx.ExecContext(ctx, `
UPDATE calls SET
	call_direction = COALESCE(?, call_direction),
	local          = COALESCE(?, local),
	display_local  = COALESCE(?, display_local),
	remote         = COALESCE(?, remote),
	display_remote = COALESCE(?, display_remote),
	active_user    = COALESCE(?, active_user),
	active_host    = COALESCE(?, active_host),
	updated_at     = ?
WHERE mac = ? AND call_id = ?`,
			ns(e.CallDirection), ns(e.Local), ns(e.DisplayLocal), ns(e.Remote),
			ns(e.DisplayRemote), ns(e.ActiveUser), ns(e.ActiveHost), now,
			e.MAC, e.CallID)
		return err
	}
	initial := t.state
	if initial == "" {
		// A transfer seen before any other event for this call.
		initial = "active"
	}
	var answered, ended any
	if t.answered {
		answered = now
	}
	if t.ended {
		ended = now
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO calls (mac, call_id, direction, call_direction, local, display_local,
	remote, display_remote, active_user, active_host, state, transfer,
	started_at, answered_at, ended_at, duration, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (mac, call_id) DO UPDATE SET
	direction      = COALESCE(direction, excluded.direction),
	call_direction = COALESCE(excluded.call_direction, call_direction),
	local          = COALESCE(excluded.local, local),
	display_local  = COALESCE(excluded.display_local, display_local),
	remote         = COALESCE(excluded.remote, remote),
	display_remote = COALESCE(excluded.display_remote, display_remote),
	active_user    = COALESCE(excluded.active_user, active_user),
	active_host    = COALESCE(excluded.active_host, active_host),
	state          = CASE WHEN ? = '' THEN state
	                      -- a finished call never goes back to ringing/dialing
	                      WHEN state IN ('ended','missed') AND ? IN ('ringing','dialing') THEN state
	                      ELSE ? END,
	transfer       = COALESCE(excluded.transfer, transfer),
	answered_at    = COALESCE(answered_at, excluded.answered_at),
	ended_at       = COALESCE(excluded.ended_at, ended_at),
	duration       = COALESCE(excluded.duration, duration),
	updated_at     = excluded.updated_at`,
		e.MAC, e.CallID, ns(t.direction), ns(e.CallDirection), ns(e.Local),
		ns(e.DisplayLocal), ns(e.Remote), ns(e.DisplayRemote), ns(e.ActiveUser),
		ns(e.ActiveHost), initial, ns(t.transfer), now, answered, ended,
		e.Duration, now,
		t.state, t.state, t.state)
	return err
}

// ns converts "" to NULL.
func ns(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Prune deletes events received before cutoff and finished calls that ended
// before it. Phones are never pruned.
func (s *Store) Prune(ctx context.Context, cutoff time.Time) (events, calls int64, err error) {
	c := fmtTime(cutoff)
	res, err := s.w.ExecContext(ctx, "DELETE FROM events WHERE received_at < ?", c)
	if err != nil {
		return 0, 0, err
	}
	events, _ = res.RowsAffected()
	res, err = s.w.ExecContext(ctx,
		"DELETE FROM calls WHERE updated_at < ? AND state IN ('ended','missed')", c)
	if err != nil {
		return events, 0, err
	}
	calls, _ = res.RowsAffected()
	return events, calls, nil
}

// EventFilter selects events for ListEvents.
type EventFilter struct {
	MAC      string
	Event    string
	CallID   string
	Since    time.Time
	Until    time.Time
	BeforeID int64 // keyset pagination: only rows with id < BeforeID
	Limit    int
}

// ListEvents returns matching events, newest first.
func (s *Store) ListEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	var where []string
	var args []any
	if f.MAC != "" {
		where, args = append(where, "mac = ?"), append(args, f.MAC)
	}
	if f.Event != "" {
		where, args = append(where, "event = ?"), append(args, f.Event)
	}
	if f.CallID != "" {
		where, args = append(where, "call_id = ?"), append(args, f.CallID)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "received_at >= ?"), append(args, fmtTime(f.Since))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "received_at < ?"), append(args, fmtTime(f.Until))
	}
	if f.BeforeID > 0 {
		where, args = append(where, "id < ?"), append(args, f.BeforeID)
	}
	q := `SELECT id, received_at, event, source_ip, mac, phone_ip, product,
	program_version, hardware_version, language, local, display_local, remote,
	display_remote, call_id, active_user, active_host, duration, call_direction,
	extra, raw_query FROM events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, clampLimit(f.Limit))

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var recv string
		var extra sql.NullString
		var cols [17]sql.NullString
		if err := rows.Scan(&e.ID, &recv, &e.Event, &cols[0], &cols[1], &cols[2],
			&cols[3], &cols[4], &cols[5], &cols[6], &cols[7], &cols[8], &cols[9],
			&cols[10], &cols[11], &cols[12], &cols[13], &e.Duration, &cols[14],
			&extra, &cols[15]); err != nil {
			return nil, err
		}
		e.ReceivedAt, _ = time.Parse(timeFormat, recv)
		e.SourceIP, e.MAC, e.PhoneIP, e.Product = cols[0].String, cols[1].String, cols[2].String, cols[3].String
		e.ProgramVersion, e.HardwareVersion, e.Language = cols[4].String, cols[5].String, cols[6].String
		e.Local, e.DisplayLocal, e.Remote, e.DisplayRemote = cols[7].String, cols[8].String, cols[9].String, cols[10].String
		e.CallID, e.ActiveUser, e.ActiveHost = cols[11].String, cols[12].String, cols[13].String
		e.CallDirection, e.RawQuery = cols[14].String, cols[15].String
		if extra.Valid {
			_ = json.Unmarshal([]byte(extra.String), &e.Extra)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const phoneCols = `mac, phone_ip, product, program_version, hardware_version,
	language, active_user, active_host, source_ip, registered, dnd, forwarding,
	syslog, off_hook, last_boot_at, first_seen_at, last_seen_at, last_event,
	event_count`

func scanPhone(sc interface{ Scan(...any) error }) (Phone, error) {
	var p Phone
	var c [9]sql.NullString
	var flags [5]sql.NullBool
	var first, last string
	err := sc.Scan(&p.MAC, &c[0], &c[1], &c[2], &c[3], &c[4], &c[5], &c[6], &c[7],
		&flags[0], &flags[1], &flags[2], &flags[3], &flags[4], &c[8],
		&first, &last, &p.LastEvent, &p.EventCount)
	if err != nil {
		return p, err
	}
	p.PhoneIP, p.Product, p.ProgramVersion, p.HardwareVersion = c[0].String, c[1].String, c[2].String, c[3].String
	p.Language, p.ActiveUser, p.ActiveHost, p.SourceIP = c[4].String, c[5].String, c[6].String, c[7].String
	p.LastBootAt = c[8].String
	for i, dst := range []**bool{&p.Registered, &p.DND, &p.Forwarding, &p.Syslog, &p.OffHook} {
		if flags[i].Valid {
			v := flags[i].Bool
			*dst = &v
		}
	}
	p.FirstSeenAt, _ = time.Parse(timeFormat, first)
	p.LastSeenAt, _ = time.Parse(timeFormat, last)
	return p, nil
}

// ListPhones returns all known phones, most recently seen first.
func (s *Store) ListPhones(ctx context.Context) ([]Phone, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT "+phoneCols+" FROM phones ORDER BY last_seen_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Phone{}
	for rows.Next() {
		p, err := scanPhone(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetPhone returns one phone by normalised MAC.
func (s *Store) GetPhone(ctx context.Context, mac string) (Phone, error) {
	p, err := scanPhone(s.r.QueryRowContext(ctx,
		"SELECT "+phoneCols+" FROM phones WHERE mac = ?", mac))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// CallFilter selects calls for ListCalls.
type CallFilter struct {
	MAC    string
	CallID string
	State  string // a specific state, or "open" for any unfinished call
	Limit  int
}

// ListCalls returns matching calls, most recently updated first.
func (s *Store) ListCalls(ctx context.Context, f CallFilter) ([]Call, error) {
	var where []string
	var args []any
	if f.MAC != "" {
		where, args = append(where, "mac = ?"), append(args, f.MAC)
	}
	if f.CallID != "" {
		where, args = append(where, "call_id = ?"), append(args, f.CallID)
	}
	switch f.State {
	case "":
	case "open":
		where = append(where, "state NOT IN ('ended','missed')")
	default:
		where, args = append(where, "state = ?"), append(args, f.State)
	}
	q := `SELECT id, mac, call_id, direction, call_direction, local, display_local,
	remote, display_remote, active_user, active_host, state, transfer, started_at,
	answered_at, ended_at, duration, updated_at FROM calls`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY updated_at DESC, id DESC LIMIT ?"
	args = append(args, clampLimit(f.Limit))

	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Call{}
	for rows.Next() {
		var c Call
		var n [11]sql.NullString
		if err := rows.Scan(&c.ID, &c.MAC, &c.CallID, &n[0], &n[1], &n[2], &n[3],
			&n[4], &n[5], &n[6], &n[7], &c.State, &n[8], &c.StartedAt, &n[9],
			&n[10], &c.Duration, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Direction, c.CallDirection, c.Local, c.DisplayLocal = n[0].String, n[1].String, n[2].String, n[3].String
		c.Remote, c.DisplayRemote, c.ActiveUser, c.ActiveHost = n[4].String, n[5].String, n[6].String, n[7].String
		c.Transfer, c.AnsweredAt, c.EndedAt = n[8].String, n[9].String, n[10].String
		out = append(out, c)
	}
	return out, rows.Err()
}

// EventCount is one row of CountEvents.
type EventCount struct {
	Event string `json:"event"`
	Count int64  `json:"count"`
}

// CountEvents returns the number of stored events per event type since t
// (all time when t is zero).
func (s *Store) CountEvents(ctx context.Context, since time.Time) ([]EventCount, error) {
	rows, err := s.r.QueryContext(ctx,
		"SELECT event, COUNT(*) FROM events WHERE received_at >= ? GROUP BY event ORDER BY 2 DESC",
		fmtTime(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EventCount{}
	for rows.Next() {
		var c EventCount
		if err := rows.Scan(&c.Event, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func clampLimit(n int) int {
	switch {
	case n <= 0:
		return 100
	case n > 1000:
		return 1000
	}
	return n
}
