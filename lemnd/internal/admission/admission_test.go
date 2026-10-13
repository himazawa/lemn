package admission

import (
	"context"
	"errors"
	"testing"
)

func TestAdmission(t *testing.T) {
	gate := New()
	release, err := gate.Acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Acquire(context.Background(), true); !errors.Is(err, ErrBusy) {
		t.Fatalf("background error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gate.Acquire(ctx, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	release()
	release()
	release, err = gate.Acquire(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	release()
}
