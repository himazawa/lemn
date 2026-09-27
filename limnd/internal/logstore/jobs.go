package logstore

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

var ErrNoJobAvailable = errors.New("no job available")

type Job struct {
	ID          int64
	TurnID      string
	RawPayload  []byte
	Attempts    int
	MaxAttempts int
}

// EnqueueJob durably persists a raw turn payload BEFORE the HTTP handler
// responds. This is the fix for the old fire-and-forget behavior: if the
// process crashes one line later, the turn is already on disk and will
// be picked up by a worker (or recovered on next startup) instead of
// being lost.
func EnqueueJob(db *sql.DB, turnID string, rawPayload []byte) (int64, error) {
	res, err := db.Exec(`
		INSERT INTO memory_jobs (turn_id, raw_payload, status, available_at)
		VALUES (?, ?, 'QUEUED', CURRENT_TIMESTAMP)`,
		turnID, string(rawPayload),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to enqueue job for turn %s: %w", turnID, err)
	}
	return res.LastInsertId()
}

// ClaimNextJob atomically claims the oldest QUEUED or due RETRY job and
// marks it PROCESSING in a single statement, so two workers can never
// claim the same row. Returns ErrNoJobAvailable if the queue is empty.
func ClaimNextJob(db *sql.DB) (*Job, error) {
	row := db.QueryRow(`
		UPDATE memory_jobs
		SET status = 'PROCESSING', locked_at = CURRENT_TIMESTAMP
		WHERE id = (
			SELECT id FROM memory_jobs
			WHERE status IN ('QUEUED', 'RETRY') AND available_at <= CURRENT_TIMESTAMP
			ORDER BY created_at ASC
			LIMIT 1
		)
		RETURNING id, turn_id, raw_payload, attempts, max_attempts;
	`)

	var j Job
	var rawPayload string
	err := row.Scan(&j.ID, &j.TurnID, &rawPayload, &j.Attempts, &j.MaxAttempts)
	if err == sql.ErrNoRows {
		return nil, ErrNoJobAvailable
	}
	if err != nil {
		return nil, fmt.Errorf("failed to claim job: %w", err)
	}
	j.RawPayload = []byte(rawPayload)
	return &j, nil
}

func MarkCompleted(db *sql.DB, jobID int64) error {
	_, err := db.Exec(`
		UPDATE memory_jobs SET status = 'COMPLETED', completed_at = CURRENT_TIMESTAMP
		WHERE id = ?`, jobID)
	return err
}

// MarkFailed increments the attempt count and either schedules a retry
// with exponential backoff, or marks the job permanently FAILED once
// max_attempts is reached. FAILED jobs are left in place (not deleted)
// for later inspection rather than silently dropped.
func MarkFailed(db *sql.DB, job *Job, cause error) error {
	attempts := job.Attempts + 1

	if attempts >= job.MaxAttempts {
		_, err := db.Exec(`
			UPDATE memory_jobs
			SET status = 'FAILED', attempts = ?, error = ?, locked_at = NULL
			WHERE id = ?`,
			attempts, cause.Error(), job.ID,
		)
		return err
	}

	backoff := backoffDuration(attempts)
	_, err := db.Exec(`
		UPDATE memory_jobs
		SET status = 'RETRY', attempts = ?, error = ?, locked_at = NULL,
		    available_at = datetime(CURRENT_TIMESTAMP, ?)
		WHERE id = ?`,
		attempts, cause.Error(), fmt.Sprintf("+%d seconds", int(backoff.Seconds())), job.ID,
	)
	return err
}

func backoffDuration(attempt int) time.Duration {
	const base = 2 * time.Second
	const max = 5 * time.Minute
	d := time.Duration(math.Pow(2, float64(attempt))) * base
	if d > max {
		return max
	}
	return d
}

// RecoverStuckJobs runs once at daemon startup. Any job left in
// PROCESSING means the previous process died mid-processing without
// reaching MarkCompleted/MarkFailed — reset it to RETRY so a worker
// picks it back up instead of it being stuck forever.
func RecoverStuckJobs(db *sql.DB) (int64, error) {
	res, err := db.Exec(`
		UPDATE memory_jobs
		SET status = 'RETRY', available_at = CURRENT_TIMESTAMP, locked_at = NULL
		WHERE status = 'PROCESSING'`)
	if err != nil {
		return 0, fmt.Errorf("failed to recover stuck jobs: %w", err)
	}
	return res.RowsAffected()
}
