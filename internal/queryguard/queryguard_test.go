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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/jackc/pgx/v5/pgconn"
)

func validConfig() *Config {
	return &Config{
		MaxConcurrent:    2,
		MaxQueue:         1,
		QueueTimeout:     "50ms",
		QueryTimeout:     "6s",
		StatementTimeout: "4s",
		LockTimeout:      "1500ms",
		MaxRows:          500,
		MaxResultBytes:   "2MiB",
	}
}

func newGuard(t *testing.T, mutate func(*Config)) *Guard {
	t.Helper()
	c := validConfig()
	if mutate != nil {
		mutate(c)
	}
	g, err := New(c)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestNewNilConfig(t *testing.T) {
	g, err := New(nil)
	if g != nil || err != nil {
		t.Fatalf("got %v, %v", g, err)
	}
	params := map[string]string{"a": "b"}
	if got := g.PostgresParams(params); len(got) != 1 {
		t.Errorf("nil guard changed params: %v", got)
	}
	if got := g.MySQLParams(params); len(got) != 1 {
		t.Errorf("nil guard changed params: %v", got)
	}
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"no concurrency", func(c *Config) { c.MaxConcurrent = 0 }, "maxConcurrent"},
		{"negative queue", func(c *Config) { c.MaxQueue = -1 }, "maxQueue"},
		{"queue without timeout", func(c *Config) { c.QueueTimeout = "" }, "queueTimeout"},
		{"no query timeout", func(c *Config) { c.QueryTimeout = "" }, "queryTimeout"},
		{"no statement timeout", func(c *Config) { c.StatementTimeout = "" }, "statementTimeout"},
		{"no max rows", func(c *Config) { c.MaxRows = 0 }, "maxRows"},
		{"no max bytes", func(c *Config) { c.MaxResultBytes = "" }, "maxResultBytes"},
		{"bad duration", func(c *Config) { c.QueryTimeout = "soon" }, "queryTimeout"},
		{"negative duration", func(c *Config) { c.LockTimeout = "-1s" }, "lockTimeout"},
		{"bad size", func(c *Config) { c.MaxResultBytes = "2TB" }, "maxResultBytes"},
		{"size overflow", func(c *Config) { c.MaxResultBytes = "9999999999999GiB" }, "maxResultBytes"},
		{"database outlives toolbox", func(c *Config) { c.StatementTimeout = "6s" }, "must be less than"},
		{"queue pushes past deadline", func(c *Config) { c.QueueTimeout = "2s" }, "must be less than"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			_, err := New(c)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestPolicyOnlyTightens(t *testing.T) {
	g := newGuard(t, nil)

	p, err := g.Policy(&ToolConfig{QueryTimeout: "5s", MaxRows: 200, MaxResultBytes: "1KiB", SlowQueryThreshold: "2s", FallbackTool: "summary"})
	if err != nil {
		t.Fatal(err)
	}
	if p.QueryTimeout != 5*time.Second || p.MaxRows != 200 || p.MaxResultBytes != 1024 || p.SlowQueryThreshold != 2*time.Second || p.FallbackTool != "summary" {
		t.Errorf("policy = %+v", p)
	}

	for name, tc := range map[string]struct {
		cfg  ToolConfig
		want string
	}{
		"longer timeout":     {ToolConfig{QueryTimeout: "7s"}, "must not exceed"},
		"more rows":          {ToolConfig{MaxRows: 501}, "must not exceed"},
		"more bytes":         {ToolConfig{MaxResultBytes: "3MiB"}, "must not exceed"},
		"negative rows":      {ToolConfig{MaxRows: -1}, "negative"},
		"below server limit": {ToolConfig{QueryTimeout: "3s"}, "must be less than"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := g.Policy(&tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServerParams(t *testing.T) {
	g := newGuard(t, nil)
	in := map[string]string{"sslmode": "disable"}
	pg := g.PostgresParams(in)
	if pg["statement_timeout"] != "4000" || pg["lock_timeout"] != "1500" || pg["sslmode"] != "disable" {
		t.Errorf("postgres params = %v", pg)
	}
	if len(in) != 1 {
		t.Error("input map must not be mutated")
	}
	my := g.MySQLParams(nil)
	if my["max_execution_time"] != "4000" || my["innodb_lock_wait_timeout"] != "2" || my["lock_wait_timeout"] != "2" {
		t.Errorf("mysql params = %v", my)
	}
}

func invoke(g *Guard, p Policy, fn func(context.Context) (any, util.ToolboxError)) (any, util.ToolboxError) {
	return g.Invoke(context.Background(), Call{Tool: "t", Source: "s"}, p, fn)
}

func mustPolicy(t *testing.T, g *Guard) Policy {
	t.Helper()
	p, err := g.Policy(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func wantCode(t *testing.T, err util.ToolboxError, code string) *util.QueryError {
	t.Helper()
	var q *util.QueryError
	if !errors.As(err, &q) {
		t.Fatalf("got %v, want QueryError", err)
	}
	if q.Code != code {
		t.Fatalf("code = %s, want %s", q.Code, code)
	}
	return q
}

func TestInvokeSuccess(t *testing.T) {
	g := newGuard(t, nil)
	res, err := invoke(g, mustPolicy(t, g), func(ctx context.Context) (any, util.ToolboxError) {
		_, st := Begin(ctx, "select 1")
		defer st.End()
		return "ok", nil
	})
	if err != nil || res != "ok" {
		t.Fatalf("got %v, %v", res, err)
	}
}

func TestInvokeFailsClosedWhenBudgetNotUsed(t *testing.T) {
	g := newGuard(t, nil)
	res, err := invoke(g, mustPolicy(t, g), func(context.Context) (any, util.ToolboxError) {
		return "unbounded rows", nil
	})
	_ = wantCode(t, err, util.QueryGuardUnenforced)
	if res != nil {
		t.Errorf("result must be withheld, got %v", res)
	}
}

func TestRowAndByteLimits(t *testing.T) {
	g := newGuard(t, func(c *Config) { c.MaxRows = 2; c.MaxResultBytes = "10" })
	p := mustPolicy(t, g)

	readRows := func(size int64) func(context.Context) (any, util.ToolboxError) {
		return func(ctx context.Context) (any, util.ToolboxError) {
			sctx, st := Begin(ctx, "select 1")
			defer st.End()
			for range 3 {
				if err := st.Row(size); err != nil {
					if sctx.Err() == nil {
						t.Error("limit must cancel the statement context")
					}
					return nil, util.NewAgentError("wrapped", fmt.Errorf("read: %w", err))
				}
			}
			return "all", nil
		}
	}
	if res, err := invoke(g, p, readRows(1)); res != nil || err == nil {
		t.Fatalf("got %v, %v", res, err)
	} else {
		q := wantCode(t, err, util.QueryRowLimit)
		if q.QueryID == "" {
			t.Error("queryId missing")
		}
	}
	_, err := invoke(g, p, readRows(8))
	_ = wantCode(t, err, util.QueryResultTooLarge)
}

func TestNilStatementIsNoOp(t *testing.T) {
	ctx, st := Begin(context.Background(), "select 1")
	if st != nil {
		t.Fatal("unguarded Begin must return nil")
	}
	if err := st.Row(1 << 40); err != nil {
		t.Fatal(err)
	}
	st.End()
	if ctx.Err() != nil {
		t.Fatal("context must be untouched")
	}
}

func TestBusyWhenQueueFull(t *testing.T) {
	g := newGuard(t, func(c *Config) { c.MaxConcurrent = 1; c.MaxQueue = 0; c.QueueTimeout = "" })
	p := mustPolicy(t, g)
	started, finish := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = invoke(g, p, func(ctx context.Context) (any, util.ToolboxError) {
			_, st := Begin(ctx, "select 1")
			defer st.End()
			close(started)
			<-finish
			return nil, nil
		})
	})
	<-started
	_, err := invoke(g, p, func(context.Context) (any, util.ToolboxError) {
		t.Error("must not run")
		return nil, nil
	})
	_ = wantCode(t, err, util.QueryBusy)
	close(finish)
	wg.Wait()
}

func TestBusyAfterQueueTimeoutAndSlotReleased(t *testing.T) {
	g := newGuard(t, func(c *Config) { c.MaxConcurrent = 1 })
	p := mustPolicy(t, g)
	started, finish := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		_, _ = invoke(g, p, func(ctx context.Context) (any, util.ToolboxError) {
			_, st := Begin(ctx, "select 1")
			defer st.End()
			close(started)
			<-finish
			return nil, nil
		})
	})
	<-started
	begin := time.Now()
	_, err := invoke(g, p, func(context.Context) (any, util.ToolboxError) { return nil, nil })
	_ = wantCode(t, err, util.QueryBusy)
	if time.Since(begin) < 50*time.Millisecond {
		t.Error("should have waited for queueTimeout")
	}
	close(finish)
	wg.Wait()

	if g.waiting.Load() != 0 {
		t.Errorf("waiting = %d", g.waiting.Load())
	}
	if len(g.slots) != 0 {
		t.Errorf("slots still held: %d", len(g.slots))
	}
}

func TestClassifyDropsDatabaseText(t *testing.T) {
	const secret = "13812345678"
	pg := func(code string) error { return &pgconn.PgError{Code: code, Message: "bad value " + secret} }
	my := func(n uint16) error { return &mysql.MySQLError{Number: n, Message: "bad value " + secret} }

	tests := []struct {
		name   string
		err    error
		code   string
		dbCode string
	}{
		{"pg statement timeout", pg("57014"), util.QueryTimeout, "57014"},
		{"pg lock timeout", pg("55P03"), util.QueryLockTimeout, "55P03"},
		{"pg syntax", pg("42601"), util.QueryFailed, "42601"},
		{"mysql statement timeout", my(3024), util.QueryTimeout, "3024"},
		{"mysql lock wait", my(1205), util.QueryLockTimeout, "1205"},
		{"mysql other", my(1064), util.QueryFailed, "1064"},
		{"deadline", context.DeadlineExceeded, util.QueryTimeout, ""},
		{"plain", errors.New("bad value " + secret), util.QueryFailed, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuard(t, nil)
			p := mustPolicy(t, g)
			p.FallbackTool = "summary"
			for _, wrapped := range []util.ToolboxError{
				util.NewAgentError("unable to run: "+tc.err.Error(), tc.err),
				util.NewClientServerError("unable to run", 500, tc.err),
			} {
				_, err := invoke(g, p, func(ctx context.Context) (any, util.ToolboxError) {
					_, st := Begin(ctx, "select 1")
					defer st.End()
					return nil, wrapped
				})
				q := wantCode(t, err, tc.code)
				if q.DBErrorCode != tc.dbCode {
					t.Errorf("dbErrorCode = %q, want %q", q.DBErrorCode, tc.dbCode)
				}
				if q.FallbackTool != "summary" {
					t.Errorf("fallbackTool = %q", q.FallbackTool)
				}
				if strings.Contains(err.Error(), secret) {
					t.Errorf("value leaked: %s", err)
				}
			}
		})
	}
}

func TestDeadlineAndCancel(t *testing.T) {
	g := newGuard(t, func(c *Config) { c.QueryTimeout = "120ms"; c.StatementTimeout = "20ms"; c.QueueTimeout = "10ms" })
	p := mustPolicy(t, g)
	blocking := func(ctx context.Context) (any, util.ToolboxError) {
		sctx, st := Begin(ctx, "select 1")
		defer st.End()
		<-sctx.Done()
		return nil, util.NewAgentError("query failed", sctx.Err())
	}
	_, err := invoke(g, p, blocking)
	_ = wantCode(t, err, util.QueryTimeout)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err = g.Invoke(ctx, Call{}, p, blocking)
	_ = wantCode(t, err, util.QueryCanceled)
}

func TestPassThrough(t *testing.T) {
	g := newGuard(t, nil)
	p := mustPolicy(t, g)

	auth := util.NewClientServerError("denied", http.StatusForbidden, nil)
	_, err := invoke(g, p, func(ctx context.Context) (any, util.ToolboxError) {
		_, st := Begin(ctx, "select 1")
		defer st.End()
		return nil, auth
	})
	if err != auth {
		t.Errorf("auth error must pass through, got %v", err)
	}

	validation := util.NewAgentError("unable to extract params", nil)
	_, err = invoke(g, p, func(context.Context) (any, util.ToolboxError) { return nil, validation })
	if err != validation {
		t.Errorf("pre-execution validation error must pass through, got %v", err)
	}
}

func TestRowSize(t *testing.T) {
	if got := RowSize([]any{nil, []byte("abc"), "de", int64(9)}); got != 13 {
		t.Errorf("RowSize = %d, want 13", got)
	}
}
