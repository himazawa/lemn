package logstore

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const DSN = "file:./limn_data.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"

func Open() (*sql.DB, error) {
	db, err := sql.Open("sqlite", DSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS turns (
			id TEXT PRIMARY KEY,
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
			user_message TEXT,
			assistant_response TEXT,
			tool_calls TEXT,
			is_memory_worthy BOOLEAN,
			memory_type TEXT,
			extracted_summary TEXT,
			confidence REAL,
			human_reviewed BOOLEAN DEFAULT FALSE,
			raw_llm_json TEXT
		);
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to init turns schema: %w", err)
	}

	// Durable job queue: /log writes here SYNCHRONOUSLY before responding,
	// so a crash after acceptance can't silently lose a turn. Workers
	// claim rows atomically (see jobs.go) and update status as they go.
	// STATES: QUEUED, PROCESSING, COMPLETED, FAILED, RETRY.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS memory_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			turn_id TEXT NOT NULL,
			raw_payload TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'QUEUED',
			attempts INTEGER NOT NULL DEFAULT 0,
			max_attempts INTEGER NOT NULL DEFAULT 5,
			available_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			locked_at DATETIME,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME,
			error TEXT
		);
		CREATE INDEX IF NOT EXISTS idx_memory_jobs_claimable
			ON memory_jobs (status, available_at);
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to init memory_jobs schema: %w", err)
	}

	return db, nil
}
