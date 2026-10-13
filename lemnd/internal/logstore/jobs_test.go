package logstore

import (
	"context"
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
	for _, cause := range []error{&backenderr.ResourceError{Status: 507}, context.Canceled} {
		for count := 0; count < 6; count++ {
			if err := MarkFailed(db, job, cause); err != nil {
				t.Fatal(err)
			}
		}
	}
	var state string
	var attempts, deferrals int
	var backedOff bool
	if err := db.QueryRow(`SELECT status, attempts, deferrals, available_at >= datetime(CURRENT_TIMESTAMP, '+480 seconds') FROM memory_jobs WHERE id = ?`, id).Scan(&state, &attempts, &deferrals, &backedOff); err != nil {
		t.Fatal(err)
	}
	if state != "RETRY" || attempts != 0 || deferrals != 12 || !backedOff {
		t.Fatalf("state=%s attempts=%d deferrals=%d backedOff=%t", state, attempts, deferrals, backedOff)
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
