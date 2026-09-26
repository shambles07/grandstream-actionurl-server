package store

// migrations are applied in order; PRAGMA user_version records how many have
// run. Append new migrations, never edit old ones.
var migrations = []string{
	`
CREATE TABLE phones (
	mac              TEXT PRIMARY KEY,
	phone_ip         TEXT,
	product          TEXT,
	program_version  TEXT,
	hardware_version TEXT,
	language         TEXT,
	active_user      TEXT,
	active_host      TEXT,
	source_ip        TEXT,
	registered       INTEGER,          -- 1 after Registration, 0 after Sign Off
	dnd              INTEGER,          -- 1 after DND On, 0 after DND Off
	forwarding       INTEGER,          -- 1 after Call Forwarding On, 0 after Off
	syslog           INTEGER,          -- 1 after Syslog On, 0 after Off
	off_hook         INTEGER,          -- 1 after Off Hook, 0 after On Hook
	last_boot_at     TEXT,
	first_seen_at    TEXT NOT NULL,
	last_seen_at     TEXT NOT NULL,
	last_event       TEXT NOT NULL,
	event_count      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE events (
	id               INTEGER PRIMARY KEY,
	received_at      TEXT NOT NULL,
	event            TEXT NOT NULL,
	source_ip        TEXT,
	mac              TEXT,
	phone_ip         TEXT,
	product          TEXT,
	program_version  TEXT,
	hardware_version TEXT,
	language         TEXT,
	local            TEXT,
	display_local    TEXT,
	remote           TEXT,
	display_remote   TEXT,
	call_id          TEXT,
	active_user      TEXT,
	active_host      TEXT,
	duration         INTEGER,
	call_direction   TEXT,
	extra            TEXT,             -- JSON object of unrecognised query params
	raw_query        TEXT
);
CREATE INDEX events_received_at ON events(received_at);
CREATE INDEX events_mac_received_at ON events(mac, received_at);
CREATE INDEX events_event_received_at ON events(event, received_at);
CREATE INDEX events_call_id ON events(call_id) WHERE call_id IS NOT NULL;

-- One row per (phone, SIP Call-ID), maintained from call-related events.
CREATE TABLE calls (
	id               INTEGER PRIMARY KEY,
	mac              TEXT NOT NULL,
	call_id          TEXT NOT NULL,
	direction        TEXT,             -- incoming | outgoing, from the event type
	call_direction   TEXT,             -- raw $calldirection value
	local            TEXT,
	display_local    TEXT,
	remote           TEXT,
	display_remote   TEXT,
	active_user      TEXT,
	active_host      TEXT,
	state            TEXT NOT NULL,    -- ringing | dialing | active | held | ended | missed
	transfer         TEXT,             -- blind | attended
	started_at       TEXT NOT NULL,
	answered_at      TEXT,
	ended_at         TEXT,
	duration         INTEGER,
	updated_at       TEXT NOT NULL,
	UNIQUE (mac, call_id)
);
CREATE INDEX calls_started_at ON calls(started_at);
CREATE INDEX calls_state ON calls(state) WHERE state NOT IN ('ended', 'missed');
`,
}
