package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAdmission(t *testing.T) {
	gate := New()
	_, release, err := gate.Acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := gate.Acquire(context.Background(), true); !errors.Is(err, ErrBusy) {
		t.Fatalf("background error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := gate.Acquire(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	release()
	release()
	_, release, err = gate.Acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestForegroundPreemptsActiveBackground(t *testing.T) {
	gate := New()
	backgroundCtx, releaseBackground, err := gate.Acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	backgroundDone := make(chan struct{})
	go func() {
		<-backgroundCtx.Done()
		releaseBackground()
		close(backgroundDone)
	}()
	foregroundCtx, releaseForeground, err := gate.Acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	<-backgroundDone
	if err := foregroundCtx.Err(); err != nil {
		t.Fatalf("foreground context canceled: %v", err)
	}
	releaseForeground()
}

func TestForegroundCancellationReleasesWaiter(t *testing.T) {
	gate := New()
	ctx, cancel := context.WithCancel(context.Background())
	_, release, err := gate.Acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := gate.Acquire(ctx, false); done <- err }()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for gate.Snapshot().ForegroundWaiters != 2 {
		select {
		case <-ticker.C:
		case <-deadline:
			t.Fatal("foreground waiter did not register")
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("wait result=%v", err)
	}
	release()
	state := gate.Snapshot()
	if state.Active || state.ForegroundWaiters != 0 {
		t.Fatalf("state after cancel/release=%+v", state)
	}
}
