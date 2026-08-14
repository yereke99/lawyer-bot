package repository

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go driver, no CGO required
)

// DB wraps the SQLite handle shared by every repository.
type DB struct {
	*sql.DB
}

// Open initialises the SQLite database, creating the file and its parent
// directory when needed, and applies the schema idempotently.
func Open(ctx context.Context, path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory: %w", err)
		}
	}

	dsn := "file:" + path +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// SQLite tolerates exactly one writer. Serialising access through a single
	// connection removes SQLITE_BUSY entirely; the traffic of a lead bot is far
	// below the point where this matters for throughput.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	db := &DB{DB: sqlDB}
	if err := db.Migrate(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Migrate applies the schema. Every statement is idempotent, so it is safe to
// run on every start-up.
func (db *DB) Migrate(ctx context.Context) error {
	for i, stmt := range schema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate statement %d: %w", i, err)
		}
	}
	return nil
}

// schema is the complete, idempotent database definition.
var schema = []string{
	// ---------------------------------------------------------------- users
	`CREATE TABLE IF NOT EXISTS users (
		id               INTEGER PRIMARY KEY AUTOINCREMENT,
		whatsapp_user_id TEXT    NOT NULL UNIQUE,
		phone_number     TEXT    NOT NULL DEFAULT '',
		display_name     TEXT    NOT NULL DEFAULT '',
		language         TEXT    NOT NULL DEFAULT '',
		current_state    TEXT    NOT NULL DEFAULT 'new',
		detected_service TEXT    NOT NULL DEFAULT '',
		lead_score       REAL    NOT NULL DEFAULT 0,
		is_lead          INTEGER NOT NULL DEFAULT 0,
		first_seen_at    DATETIME NOT NULL,
		last_seen_at     DATETIME NOT NULL,
		created_at       DATETIME NOT NULL,
		updated_at       DATETIME NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_whatsapp_user_id ON users(whatsapp_user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_users_phone_number     ON users(phone_number)`,
	`CREATE INDEX IF NOT EXISTS idx_users_current_state    ON users(current_state)`,
	`CREATE INDEX IF NOT EXISTS idx_users_detected_service ON users(detected_service)`,
	`CREATE INDEX IF NOT EXISTS idx_users_lead_score       ON users(lead_score)`,
	`CREATE INDEX IF NOT EXISTS idx_users_created_at       ON users(created_at)`,

	// ------------------------------------------------------------- messages
	`CREATE TABLE IF NOT EXISTS messages (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id             INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		whatsapp_message_id TEXT    NOT NULL DEFAULT '',
		trace_id            TEXT    NOT NULL DEFAULT '',
		message_type        TEXT    NOT NULL DEFAULT 'text',
		text                TEXT    NOT NULL DEFAULT '',
		media_id            TEXT    NOT NULL DEFAULT '',
		caption             TEXT    NOT NULL DEFAULT '',
		direction           TEXT    NOT NULL,
		processed           INTEGER NOT NULL DEFAULT 0,
		ai_processed        INTEGER NOT NULL DEFAULT 0,
		ai_intent           TEXT    NOT NULL DEFAULT '',
		ai_confidence       REAL    NOT NULL DEFAULT 0,
		bot_responded       INTEGER NOT NULL DEFAULT 0,
		created_at          DATETIME NOT NULL
	)`,
	// Partial unique index: provider IDs are unique when present, and outgoing
	// rows created before the provider replies may legitimately have none.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_messages_whatsapp_message_id
		ON messages(whatsapp_message_id) WHERE whatsapp_message_id <> ''`,
	`CREATE INDEX IF NOT EXISTS idx_messages_user_id    ON messages(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_trace_id   ON messages(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_direction  ON messages(direction)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_user_created ON messages(user_id, created_at)`,

	// --------------------------------------------------------- media_assets
	`CREATE TABLE IF NOT EXISTS media_assets (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		message_id  INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		media_id    TEXT    NOT NULL DEFAULT '',
		mime_type   TEXT    NOT NULL DEFAULT '',
		sha256      TEXT    NOT NULL DEFAULT '',
		file_size   INTEGER NOT NULL DEFAULT 0,
		filename    TEXT    NOT NULL DEFAULT '',
		caption     TEXT    NOT NULL DEFAULT '',
		voice       INTEGER NOT NULL DEFAULT 0,
		downloaded  INTEGER NOT NULL DEFAULT 0,
		local_path  TEXT    NOT NULL DEFAULT '',
		created_at  DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_media_message_id ON media_assets(message_id)`,
	`CREATE INDEX IF NOT EXISTS idx_media_media_id   ON media_assets(media_id)`,

	// ------------------------------------------------------ ai_interactions
	`CREATE TABLE IF NOT EXISTS ai_interactions (
		id                 INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id            INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		message_id         INTEGER NOT NULL DEFAULT 0,
		trace_id           TEXT    NOT NULL DEFAULT '',
		model              TEXT    NOT NULL DEFAULT '',
		input_tokens       INTEGER NOT NULL DEFAULT 0,
		output_tokens      INTEGER NOT NULL DEFAULT 0,
		intent             TEXT    NOT NULL DEFAULT '',
		service_code       TEXT    NOT NULL DEFAULT '',
		confidence         REAL    NOT NULL DEFAULT 0,
		raw_response       TEXT    NOT NULL DEFAULT '',
		error              TEXT    NOT NULL DEFAULT '',
		processing_time_ms INTEGER NOT NULL DEFAULT 0,
		created_at         DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_ai_user_id      ON ai_interactions(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_ai_message_id   ON ai_interactions(message_id)`,
	`CREATE INDEX IF NOT EXISTS idx_ai_trace_id     ON ai_interactions(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_ai_created_at   ON ai_interactions(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_ai_service_code ON ai_interactions(service_code)`,

	// ----------------------------------------------------------------- leads
	`CREATE TABLE IF NOT EXISTS leads (
		id                    INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id               INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		service_code          TEXT    NOT NULL DEFAULT '',
		service_name          TEXT    NOT NULL DEFAULT '',
		language              TEXT    NOT NULL DEFAULT '',
		phone_number          TEXT    NOT NULL DEFAULT '',
		lead_score            REAL    NOT NULL DEFAULT 0,
		status                TEXT    NOT NULL DEFAULT 'new',
		source                TEXT    NOT NULL DEFAULT 'whatsapp',
		qualification_summary TEXT    NOT NULL DEFAULT '',
		notified_at           DATETIME,
		created_at            DATETIME NOT NULL,
		updated_at            DATETIME NOT NULL
	)`,
	// One open lead per user: repeated qualification updates the same row.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_leads_open_user ON leads(user_id)
		WHERE status NOT IN ('converted','closed','rejected')`,
	`CREATE INDEX IF NOT EXISTS idx_leads_status       ON leads(status)`,
	`CREATE INDEX IF NOT EXISTS idx_leads_service_code ON leads(service_code)`,
	`CREATE INDEX IF NOT EXISTS idx_leads_lead_score   ON leads(lead_score)`,
	`CREATE INDEX IF NOT EXISTS idx_leads_created_at   ON leads(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_leads_source       ON leads(source)`,

	// ---------------------------------------------------------- trace_events
	`CREATE TABLE IF NOT EXISTS trace_events (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id    TEXT    NOT NULL,
		user_id     INTEGER NOT NULL DEFAULT 0,
		message_id  INTEGER NOT NULL DEFAULT 0,
		stage       TEXT    NOT NULL,
		decision    TEXT    NOT NULL DEFAULT '',
		reason      TEXT    NOT NULL DEFAULT '',
		detail      TEXT    NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_trace_trace_id   ON trace_events(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_trace_user_id    ON trace_events(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_trace_message_id ON trace_events(message_id)`,
	`CREATE INDEX IF NOT EXISTS idx_trace_stage      ON trace_events(stage)`,
	`CREATE INDEX IF NOT EXISTS idx_trace_created_at ON trace_events(created_at)`,

	// ----------------------------------------------------- state_transitions
	`CREATE TABLE IF NOT EXISTS state_transitions (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		trace_id   TEXT    NOT NULL DEFAULT '',
		from_state TEXT    NOT NULL DEFAULT '',
		to_state   TEXT    NOT NULL DEFAULT '',
		reason     TEXT    NOT NULL DEFAULT '',
		message_id INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_transitions_user_id    ON state_transitions(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_transitions_trace_id   ON state_transitions(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_transitions_created_at ON state_transitions(created_at)`,

	// ------------------------------------------------------------ user_facts
	`CREATE TABLE IF NOT EXISTS user_facts (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		key        TEXT    NOT NULL,
		value      TEXT    NOT NULL DEFAULT '',
		source     TEXT    NOT NULL DEFAULT '',
		message_id INTEGER NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		UNIQUE(user_id, key)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_facts_user_id ON user_facts(user_id)`,

	// ------------------------------------------------------------ deliveries
	`CREATE TABLE IF NOT EXISTS deliveries (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id             INTEGER NOT NULL DEFAULT 0,
		message_id          INTEGER NOT NULL DEFAULT 0,
		trace_id            TEXT    NOT NULL DEFAULT '',
		recipient           TEXT    NOT NULL DEFAULT '',
		kind                TEXT    NOT NULL DEFAULT 'reply',
		status              TEXT    NOT NULL DEFAULT '',
		attempts            INTEGER NOT NULL DEFAULT 0,
		provider_message_id TEXT    NOT NULL DEFAULT '',
		error               TEXT    NOT NULL DEFAULT '',
		created_at          DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_deliveries_user_id    ON deliveries(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_deliveries_trace_id   ON deliveries(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_deliveries_status     ON deliveries(status)`,
	`CREATE INDEX IF NOT EXISTS idx_deliveries_created_at ON deliveries(created_at)`,

	// -------------------------------------------------------- webhook_events
	`CREATE TABLE IF NOT EXISTS webhook_events (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		trace_id      TEXT    NOT NULL DEFAULT '',
		provider      TEXT    NOT NULL DEFAULT 'whatsapp',
		signature     TEXT    NOT NULL DEFAULT '',
		payload       TEXT    NOT NULL DEFAULT '',
		message_count INTEGER NOT NULL DEFAULT 0,
		status        TEXT    NOT NULL DEFAULT 'received',
		error         TEXT    NOT NULL DEFAULT '',
		created_at    DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_webhook_trace_id   ON webhook_events(trace_id)`,
	`CREATE INDEX IF NOT EXISTS idx_webhook_created_at ON webhook_events(created_at)`,

	// --------------------------------------------------------- notifications
	`CREATE TABLE IF NOT EXISTS notifications (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		lead_id    INTEGER NOT NULL DEFAULT 0,
		user_id    INTEGER NOT NULL DEFAULT 0,
		trace_id   TEXT    NOT NULL DEFAULT '',
		channel    TEXT    NOT NULL DEFAULT 'whatsapp',
		recipient  TEXT    NOT NULL DEFAULT '',
		body       TEXT    NOT NULL DEFAULT '',
		status     TEXT    NOT NULL DEFAULT '',
		error      TEXT    NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_lead_id    ON notifications(lead_id)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_user_id    ON notifications(user_id)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_created_at ON notifications(created_at)`,
}
