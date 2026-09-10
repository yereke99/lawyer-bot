package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// This file contains the CRM migration. It is deliberately separate from the
// original schema so the production tables that already hold customer data are
// never redefined, dropped or renamed.
//
// Migration safety rules honoured here:
//   - only CREATE TABLE IF NOT EXISTS, CREATE INDEX IF NOT EXISTS and
//     ALTER TABLE ... ADD COLUMN are used;
//   - every added column has a DEFAULT, so existing rows migrate silently;
//   - column additions are guarded by PRAGMA table_info, because SQLite has no
//     ADD COLUMN IF NOT EXISTS and re-running must stay a no-op;
//   - no statement touches or rewrites an existing row's business meaning.

// addColumn is one guarded ALTER TABLE ... ADD COLUMN.
type addColumn struct {
	table      string
	column     string
	definition string
}

// crmColumns are the CRM fields added to the existing production tables.
var crmColumns = []addColumn{
	// --------------------------------------------------------- users (clients)
	{"users", "crm_status", "TEXT NOT NULL DEFAULT 'new'"},
	{"users", "conversation_mode", "TEXT NOT NULL DEFAULT 'ai'"},
	{"users", "ai_enabled", "INTEGER NOT NULL DEFAULT 1"},
	{"users", "blocked", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "blocked_at", "DATETIME"},
	{"users", "blocked_by", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "block_reason", "TEXT NOT NULL DEFAULT ''"},
	{"users", "assigned_admin_id", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "assigned_at", "DATETIME"},
	{"users", "assigned_by", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "last_inbound_at", "DATETIME"},
	{"users", "last_outbound_at", "DATETIME"},
	{"users", "unread_count", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "ai_summary", "TEXT NOT NULL DEFAULT ''"},
	{"users", "important_facts", "TEXT NOT NULL DEFAULT ''"},
	{"users", "next_action", "TEXT NOT NULL DEFAULT ''"},
	{"users", "qualification_stage", "TEXT NOT NULL DEFAULT ''"},
	{"users", "ai_confidence", "REAL NOT NULL DEFAULT 0"},
	{"users", "intent", "TEXT NOT NULL DEFAULT ''"},
	{"users", "follow_up_stage", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "next_follow_up_at", "DATETIME"},
	{"users", "close_reason", "TEXT NOT NULL DEFAULT ''"},
	{"users", "tags", "TEXT NOT NULL DEFAULT ''"},
	{"users", "language_locked", "INTEGER NOT NULL DEFAULT 0"},
	{"users", "summary_watermark", "INTEGER NOT NULL DEFAULT 0"},

	// ------------------------------------------------------------- messages
	{"messages", "sender_type", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "sender_admin_id", "INTEGER NOT NULL DEFAULT 0"},
	{"messages", "media_path", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "media_mime", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "media_name", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "media_size", "INTEGER NOT NULL DEFAULT 0"},
	{"messages", "delivery_status", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "reply_to", "TEXT NOT NULL DEFAULT ''"},
	{"messages", "metadata", "TEXT NOT NULL DEFAULT ''"},
}

// crmTables are the new CRM tables and the indexes that only touch them.
var crmTables = []string{
	// ---------------------------------------------------------- admin_users
	`CREATE TABLE IF NOT EXISTS admin_users (
		id              INTEGER PRIMARY KEY AUTOINCREMENT,
		email           TEXT    NOT NULL,
		name            TEXT    NOT NULL DEFAULT '',
		password_hash   TEXT    NOT NULL,
		password_salt   TEXT    NOT NULL,
		password_iter   INTEGER NOT NULL DEFAULT 0,
		role            TEXT    NOT NULL DEFAULT 'admin',
		active          INTEGER NOT NULL DEFAULT 1,
		last_login_at   DATETIME,
		failed_attempts INTEGER NOT NULL DEFAULT 0,
		locked_until    DATETIME,
		created_at      DATETIME NOT NULL,
		updated_at      DATETIME NOT NULL
	)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_admin_users_email ON admin_users(lower(email))`,

	// ------------------------------------------------------- admin_sessions
	// id is the SHA-256 digest of the cookie token, never the token itself.
	`CREATE TABLE IF NOT EXISTS admin_sessions (
		id           TEXT     PRIMARY KEY,
		admin_id     INTEGER  NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
		csrf_token   TEXT     NOT NULL DEFAULT '',
		created_at   DATETIME NOT NULL,
		expires_at   DATETIME NOT NULL,
		last_seen_at DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_admin_sessions_admin   ON admin_sessions(admin_id)`,
	`CREATE INDEX IF NOT EXISTS idx_admin_sessions_expires ON admin_sessions(expires_at)`,

	// -------------------------------------------------------- follow_up_jobs
	`CREATE TABLE IF NOT EXISTS follow_up_jobs (
		id           INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		stage        INTEGER NOT NULL DEFAULT 1,
		scheduled_at DATETIME NOT NULL,
		status       TEXT    NOT NULL DEFAULT 'pending',
		claim_token  TEXT    NOT NULL DEFAULT '',
		claimed_at   DATETIME,
		attempts     INTEGER NOT NULL DEFAULT 0,
		last_error   TEXT    NOT NULL DEFAULT '',
		dedupe_key   TEXT    NOT NULL,
		message_id   INTEGER NOT NULL DEFAULT 0,
		created_at   DATETIME NOT NULL,
		updated_at   DATETIME NOT NULL
	)`,
	// One job per client per stage per anchor message: the idempotency key that
	// makes a duplicate schedule impossible even across processes.
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_followup_dedupe ON follow_up_jobs(dedupe_key)`,
	`CREATE INDEX IF NOT EXISTS idx_followup_due    ON follow_up_jobs(status, scheduled_at)`,
	`CREATE INDEX IF NOT EXISTS idx_followup_user   ON follow_up_jobs(user_id, status)`,

	// -------------------------------------------------------- internal_notes
	`CREATE TABLE IF NOT EXISTS internal_notes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		admin_id   INTEGER NOT NULL DEFAULT 0,
		body       TEXT    NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		deleted_at DATETIME
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notes_user ON internal_notes(user_id, created_at)`,

	// ------------------------------------------------------------ audit_logs
	`CREATE TABLE IF NOT EXISTS audit_logs (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		admin_id   INTEGER NOT NULL DEFAULT 0,
		action     TEXT    NOT NULL,
		entity     TEXT    NOT NULL DEFAULT '',
		entity_id  INTEGER NOT NULL DEFAULT 0,
		detail     TEXT    NOT NULL DEFAULT '',
		ip_hash    TEXT    NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_admin   ON audit_logs(admin_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_entity  ON audit_logs(entity, entity_id)`,
	`CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_logs(created_at)`,

	// ---------------------------------------------------------- crm_settings
	`CREATE TABLE IF NOT EXISTS crm_settings (
		key        TEXT PRIMARY KEY,
		value      TEXT NOT NULL DEFAULT '',
		updated_at DATETIME NOT NULL,
		updated_by INTEGER NOT NULL DEFAULT 0
	)`,

	// ------------------------------------------------------- login_attempts
	// Rate limiting survives a restart, so a brute-force run cannot be reset by
	// bouncing the service.
	`CREATE TABLE IF NOT EXISTS login_attempts (
		id          INTEGER PRIMARY KEY AUTOINCREMENT,
		identifier  TEXT     NOT NULL,
		created_at  DATETIME NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_login_attempts ON login_attempts(identifier, created_at)`,
}

// crmIndexes cover columns added by crmColumns, so they must run after the
// ALTER TABLE statements.
var crmIndexes = []string{
	`CREATE INDEX IF NOT EXISTS idx_users_crm_status    ON users(crm_status)`,
	`CREATE INDEX IF NOT EXISTS idx_users_mode          ON users(conversation_mode)`,
	`CREATE INDEX IF NOT EXISTS idx_users_blocked       ON users(blocked)`,
	`CREATE INDEX IF NOT EXISTS idx_users_assigned      ON users(assigned_admin_id)`,
	`CREATE INDEX IF NOT EXISTS idx_users_last_inbound  ON users(last_inbound_at)`,
	`CREATE INDEX IF NOT EXISTS idx_users_next_followup ON users(next_follow_up_at)`,
	`CREATE INDEX IF NOT EXISTS idx_users_language      ON users(language)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_sender     ON messages(sender_type)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_user_dir   ON messages(user_id, direction, created_at)`,
}

// crmBackfill gives pre-CRM rows sensible operational values exactly once. Each
// statement is written so a second run changes nothing.
var crmBackfill = []string{
	// Existing conversations keep their history and enter the pipeline at a
	// status derived from the qualification state they already reached.
	`UPDATE users SET crm_status = CASE
		WHEN current_state = 'completed'       THEN 'closed'
		WHEN current_state = 'ready_for_diana' THEN 'qualified'
		WHEN current_state = 'new'             THEN 'new'
		ELSE 'needs_qualification' END
	 WHERE crm_status = 'new' AND current_state <> 'new'`,

	// Historical messages get a sender type so the CRM chat renders them
	// correctly: inbound is always the client, outbound was always the bot.
	`UPDATE messages SET sender_type = 'client' WHERE sender_type = '' AND direction = 'incoming'`,
	`UPDATE messages SET sender_type = 'ai'     WHERE sender_type = '' AND direction = 'outgoing'`,

	// Activity timestamps power the CRM list without scanning the message table.
	`UPDATE users SET last_inbound_at = (
		SELECT MAX(created_at) FROM messages m WHERE m.user_id = users.id AND m.direction = 'incoming')
	 WHERE last_inbound_at IS NULL`,
	`UPDATE users SET last_outbound_at = (
		SELECT MAX(created_at) FROM messages m WHERE m.user_id = users.id AND m.direction = 'outgoing')
	 WHERE last_outbound_at IS NULL`,
}

// MigrateCRM applies the CRM migration on top of the existing schema.
func (db *DB) MigrateCRM(ctx context.Context) error {
	for i, stmt := range crmTables {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("crm table statement %d: %w", i, err)
		}
	}
	// Columns come next: the indexes below reference them.
	for _, col := range crmColumns {
		if err := db.addColumnIfMissing(ctx, col); err != nil {
			return err
		}
	}
	for i, stmt := range crmIndexes {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("crm index statement %d: %w", i, err)
		}
	}
	for i, stmt := range crmBackfill {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("crm backfill statement %d: %w", i, err)
		}
	}
	return nil
}

// addColumnIfMissing adds a column only when the table does not already have it.
func (db *DB) addColumnIfMissing(ctx context.Context, col addColumn) error {
	has, err := db.hasColumn(ctx, col.table, col.column)
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col.table, col.column, col.definition)
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		// A concurrent process may have added it between the check and here.
		if again, checkErr := db.hasColumn(ctx, col.table, col.column); checkErr == nil && again {
			return nil
		}
		return fmt.Errorf("add column %s.%s: %w", col.table, col.column, err)
	}
	return nil
}

// hasColumn reports whether a table already declares a column.
func (db *DB) hasColumn(ctx context.Context, table, column string) (bool, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect column %s.%s: %w", table, column, err)
	}
	return true, nil
}
