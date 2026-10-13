package logstore

import (
	"errors"
	"lemnd/internal/backenderr"
	"path/filepath"
	"testing"
)

func TestResourceFailuresRemainRecoverable(t *testing.T) {
	t.Setenv("LEMN_SQLITE_PATH", filepath.Join(t.TempDir(), "jobs.db"))
	db, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := EnqueueJob(db, "turn", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	job := &Job{ID: id, MaxAttempts: 5}
	for count := 0; count < 10; count++ {
		if err := MarkFailed(db, job, &backenderr.ResourceError{Status: 507}); err != nil {
			t.Fatal(err)
		}
	}
	var state string
	var attempts int
	var deferred bool
	if err := db.QueryRow(`SELECT status, attempts, available_at > CURRENT_TIMESTAMP FROM memory_jobs WHERE id = ?`, id).Scan(&state, &attempts, &deferred); err != nil {
		t.Fatal(err)
	}
	if state != "RETRY" || attempts != 0 || !deferred {
		t.Fatalf("state=%s attempts=%d deferred=%t", state, attempts, deferred)
	}
	job.Attempts = 4
	if err := MarkFailed(db, job, errors.New("invalid extraction JSON")); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status, attempts FROM memory_jobs WHERE id = ?`, id).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "FAILED" || attempts != 5 {
		t.Fatalf("state=%s attempts=%d", state, attempts)
	}
}
