package lemn

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
)

func TestMemoryTransactionRetries(t *testing.T) {
	operations := []struct {
		name   string
		call   func(context.Context, *sql.DB) error
		state  string
		writes int
	}{
		{name: "confirm", state: "PENDING", writes: 3, call: func(ctx context.Context, db *sql.DB) error {
			return ConfirmPendingMemory(ctx, db, 1)
		}},
		{name: "reject", state: "AUTHORITATIVE", writes: 2, call: func(ctx context.Context, db *sql.DB) error {
			return RejectMemory(ctx, db, 1, "retracted")
		}},
	}
	for _, operation := range operations {
		for _, test := range []struct {
			name         string
			stage        string
			err          error
			failures     int
			state        string
			wantAttempts int
			wantError    bool
		}{
			{name: "begin serialization", stage: "begin", err: &pq.Error{Code: "40001"}, failures: 1, wantAttempts: 2},
			{name: "query serialization", stage: "query", err: &pq.Error{Code: "40001"}, failures: 1, wantAttempts: 2},
			{name: "query deadlock exhausted", stage: "query", err: &pq.Error{Code: "40P01"}, failures: 3, wantAttempts: 3, wantError: true},
			{name: "write deadlock", stage: "exec", err: &pq.Error{Code: "40P01"}, failures: 1, wantAttempts: 2},
			{name: "commit serialization", stage: "commit", err: &pq.Error{Code: "40001"}, failures: 1, wantAttempts: 2},
			{name: "commit deadlock", stage: "commit", err: &pq.Error{Code: "40P01"}, failures: 1, wantAttempts: 2},
			{name: "ambiguous commit", stage: "commit", err: errors.New("commit connection lost"), failures: 1, wantAttempts: 1, wantError: true},
			{name: "other SQLSTATE", stage: "query", err: &pq.Error{Code: "23505"}, failures: 1, wantAttempts: 1, wantError: true},
			{name: "validation", state: "REJECTED", wantAttempts: 1, wantError: true},
		} {
			t.Run(operation.name+"/"+test.name, func(t *testing.T) {
				state := test.state
				if state == "" {
					state = operation.state
				}
				connection := &retryTestConnection{state: state, stage: test.stage, failure: test.err, failures: test.failures}
				db := sql.OpenDB(retryTestConnector{connection: connection})
				db.SetMaxOpenConns(1)
				defer db.Close()
				err := operation.call(context.Background(), db)
				if (err != nil) != test.wantError {
					t.Fatalf("error = %v, wantError = %v", err, test.wantError)
				}
				if test.wantError && test.err != nil && !errors.Is(err, test.err) {
					t.Fatalf("error = %v, want original cause %v", err, test.err)
				}
				if connection.attempts != test.wantAttempts {
					t.Fatalf("attempts = %d, want %d", connection.attempts, test.wantAttempts)
				}
				var wantEvents []string
				for attempt := 1; attempt <= test.wantAttempts; attempt++ {
					wantEvents = append(wantEvents, "begin")
					if attempt <= test.failures && test.stage == "begin" {
						continue
					}
					if test.state != "" || (attempt <= test.failures && test.stage != "commit") {
						wantEvents = append(wantEvents, "rollback")
					} else {
						wantEvents = append(wantEvents, "commit")
					}
				}
				if !reflect.DeepEqual(connection.events, wantEvents) {
					t.Fatalf("transaction events = %v, want %v", connection.events, wantEvents)
				}
				if !test.wantError && connection.execs != operation.writes {
					t.Fatalf("successful attempt writes = %d, want %d", connection.execs, operation.writes)
				}
			})
		}
	}
}

type retryTestConnector struct {
	connection *retryTestConnection
}

func (connector retryTestConnector) Connect(context.Context) (driver.Conn, error) {
	return connector.connection, nil
}

func (connector retryTestConnector) Driver() driver.Driver { return retryTestDriver{} }

type retryTestDriver struct{}

func (retryTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use connector")
}

type retryTestConnection struct {
	state    string
	stage    string
	failure  error
	failures int
	attempts int
	execs    int
	events   []string
}

func (connection *retryTestConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepared statements unsupported")
}

func (connection *retryTestConnection) Close() error { return nil }

func (connection *retryTestConnection) Begin() (driver.Tx, error) {
	return connection.BeginTx(context.Background(), driver.TxOptions{})
}

func (connection *retryTestConnection) BeginTx(_ context.Context, options driver.TxOptions) (driver.Tx, error) {
	if options.Isolation != driver.IsolationLevel(sql.LevelSerializable) {
		return nil, fmt.Errorf("unexpected isolation %d", options.Isolation)
	}
	connection.attempts++
	connection.execs = 0
	connection.events = append(connection.events, "begin")
	return connection, connection.failAt("begin")
}

func (connection *retryTestConnection) failAt(stage string) error {
	if connection.stage == stage && connection.attempts <= connection.failures {
		return connection.failure
	}
	return nil
}

func (connection *retryTestConnection) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	if err := connection.failAt("query"); err != nil {
		return nil, err
	}
	columns := []string{"state"}
	if strings.Contains(query, "project_id") {
		columns = append(columns, "project_id", "provenance")
	}
	return &retryTestRows{state: connection.state, columns: columns}, nil
}

func (connection *retryTestConnection) ExecContext(_ context.Context, _ string, _ []driver.NamedValue) (driver.Result, error) {
	connection.execs++
	if connection.execs == 2 {
		if err := connection.failAt("exec"); err != nil {
			return nil, err
		}
	}
	return driver.RowsAffected(1), nil
}

func (connection *retryTestConnection) Commit() error {
	connection.events = append(connection.events, "commit")
	return connection.failAt("commit")
}

func (connection *retryTestConnection) Rollback() error {
	connection.events = append(connection.events, "rollback")
	return nil
}

type retryTestRows struct {
	state   string
	columns []string
	done    bool
}

func (rows *retryTestRows) Columns() []string { return rows.columns }

func (rows *retryTestRows) Close() error { return nil }

func (rows *retryTestRows) Next(values []driver.Value) error {
	if rows.done {
		return io.EOF
	}
	rows.done = true
	values[0] = rows.state
	if len(values) == 3 {
		values[1] = "project"
		values[2] = []byte(`{}`)
	}
	return nil
}

func TestRetryTransaction(t *testing.T) {
	validationError := errors.New("invalid relation")
	ambiguousCommitError := errors.New("connection lost during commit")
	tests := []struct {
		name         string
		err          error
		failures     int
		wantAttempts int
	}{
		{name: "success", wantAttempts: 1},
		{name: "serialization succeeds on third attempt", err: fmt.Errorf("query: %w", &pq.Error{Code: "40001"}), failures: 2, wantAttempts: 3},
		{name: "deadlock succeeds on second attempt", err: fmt.Errorf("query: %w", &pq.Error{Code: "40P01"}), failures: 1, wantAttempts: 2},
		{name: "serialization exhausted", err: &pq.Error{Code: "40001"}, failures: 4, wantAttempts: 3},
		{name: "deadlock exhausted", err: &pq.Error{Code: "40P01"}, failures: 4, wantAttempts: 3},
		{name: "validation", err: validationError, failures: 4, wantAttempts: 1},
		{name: "ambiguous commit", err: ambiguousCommitError, failures: 4, wantAttempts: 1},
		{name: "other SQLSTATE", err: &pq.Error{Code: "23505"}, failures: 4, wantAttempts: 1},
		{name: "SQLSTATE text alone", err: errors.New("40001 40P01"), failures: 4, wantAttempts: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			started := time.Now()
			err := retryTransaction(context.Background(), func() error {
				attempts++
				if attempts <= test.failures {
					return test.err
				}
				return nil
			})
			if attempts != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts, test.wantAttempts)
			}
			if test.failures >= test.wantAttempts {
				if err != test.err {
					t.Fatalf("error = %v, want original error %v", err, test.err)
				}
			} else if err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
			minimumWait := time.Duration(test.wantAttempts*(test.wantAttempts-1)/2) * 10 * time.Millisecond
			if elapsed := time.Since(started); elapsed < minimumWait {
				t.Fatalf("elapsed = %v, want at least %v", elapsed, minimumWait)
			}
		})
	}
}

func TestRetryTransactionContext(t *testing.T) {
	for _, test := range []struct {
		name     string
		before   bool
		deadline bool
	}{
		{name: "canceled before attempt", before: true},
		{name: "canceled during wait"},
		{name: "deadline during wait", deadline: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			if test.deadline {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 5*time.Millisecond)
			}
			defer cancel()
			if test.before {
				cancel()
			}
			attempts := 0
			err := retryTransaction(ctx, func() error {
				attempts++
				if !test.deadline {
					cancel()
				}
				return &pq.Error{Code: "40001"}
			})
			wantAttempts := 1
			if test.before {
				wantAttempts = 0
			}
			if err != ctx.Err() || err == nil || attempts != wantAttempts {
				t.Fatalf("error = %v, attempts = %d; want %v, %d", err, attempts, ctx.Err(), wantAttempts)
			}
		})
	}
}
