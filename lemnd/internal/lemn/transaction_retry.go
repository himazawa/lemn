package lemn

import (
	"context"
	"errors"
	"time"

	"github.com/lib/pq"
)

func retryTransaction(ctx context.Context, once func() error) error {
	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := once()
		var postgresError *pq.Error
		if err == nil || attempt == maxAttempts || !errors.As(err, &postgresError) || postgresError == nil ||
			(postgresError.Code != "40001" && postgresError.Code != "40P01") {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
