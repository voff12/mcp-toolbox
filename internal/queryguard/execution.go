// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package queryguard

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/jackc/pgx/v5"
)

type executionKey struct{}

// Execution is the budget and record of one tool invocation. A tool may run
// several statements; they share the budget.
type Execution struct {
	policy     Policy
	statements atomic.Int64
	rows       atomic.Int64
	bytes      atomic.Int64
	dbNanos    atomic.Int64

	mu      sync.Mutex
	queryID string
}

func withExecution(ctx context.Context, e *Execution) context.Context {
	return context.WithValue(ctx, executionKey{}, e)
}

// LimitError reports that a read exceeded its budget.
type LimitError struct {
	Code string
}

func (e *LimitError) Error() string { return e.Code }

// Statement tracks one statement of an Execution. All methods are safe on a
// nil Statement, which is what Begin returns for unguarded calls, so sources
// have a single code path.
type Statement struct {
	exec   *Execution
	start  time.Time
	cancel context.CancelFunc
	once   sync.Once
}

// Begin records a statement. Pass the SQL before any comment is added so the
// fingerprint is stable. Use the returned context for the query: Row cancels it
// when the budget runs out, because closing rows mid-stream would otherwise
// read the remainder of the result.
func Begin(ctx context.Context, statement string) (context.Context, *Statement) {
	e, ok := ctx.Value(executionKey{}).(*Execution)
	if !ok {
		return ctx, nil
	}
	e.statements.Add(1)
	e.mu.Lock()
	if e.queryID == "" {
		e.queryID = util.SQLFingerprint(statement)
	}
	e.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	return ctx, &Statement{exec: e, start: time.Now(), cancel: cancel}
}

// Row charges one row of the given size. It stops the query and returns a
// *LimitError when the budget is exhausted; the row must then be discarded.
func (s *Statement) Row(size int64) error {
	if s == nil {
		return nil
	}
	code := ""
	if s.exec.rows.Add(1) > s.exec.policy.MaxRows {
		code = util.QueryRowLimit
	} else if s.exec.bytes.Add(size) > s.exec.policy.MaxResultBytes {
		code = util.QueryResultTooLarge
	}
	if code == "" {
		return nil
	}
	s.cancel()
	return &LimitError{Code: code}
}

// End records the elapsed time and releases the statement's context. It is safe
// to call more than once.
func (s *Statement) End() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		s.exec.dbNanos.Add(int64(time.Since(s.start)))
		s.cancel()
	})
}

// RowSize estimates the size of a row from driver values, as used for the
// byte budget. Variable-length values count their length.
func RowSize(values []any) int64 {
	var n int64
	for _, v := range values {
		switch t := v.(type) {
		case nil:
		case []byte:
			n += int64(len(t))
		case string:
			n += int64(len(t))
		default:
			n += 8
		}
	}
	return n
}

// PgxQuerier is satisfied by pgxpool.Pool.
type PgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// QueryPgx runs a query with budget enforcement wrapped around the returned
// rows, so callers keep their ordinary read loop. statement is the SQL to
// fingerprint and wire is the text actually sent.
func QueryPgx(ctx context.Context, q PgxQuerier, statement, wire string, args ...any) (pgx.Rows, error) {
	ctx, st := Begin(ctx, statement)
	if st == nil {
		return q.Query(ctx, wire, args...)
	}
	rows, err := q.Query(ctx, wire, args...)
	if err != nil {
		st.End()
		return nil, err
	}
	return &guardedRows{Rows: rows, st: st}, nil
}

type guardedRows struct {
	pgx.Rows
	st  *Statement
	err error
}

func (r *guardedRows) Next() bool {
	if r.err != nil || !r.Rows.Next() {
		return false
	}
	var size int64
	for _, v := range r.RawValues() {
		size += int64(len(v))
	}
	if err := r.st.Row(size); err != nil {
		r.err = err
		return false
	}
	return true
}

func (r *guardedRows) Err() error {
	if r.err != nil {
		return r.err
	}
	return r.Rows.Err()
}

func (r *guardedRows) Close() {
	r.Rows.Close()
	r.st.End()
}
