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

// Package queryguard verifies the guarantees of internal/queryguard against real
// databases: that the server stops a query, that reading stops mid-stream and
// that nothing is left running. Each test skips unless its host variable is set.
//
//	QUERYGUARD_PG_HOST, QUERYGUARD_MYSQL_HOST, QUERYGUARD_CRDB_HOST
package queryguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/googleapis/mcp-toolbox/internal/queryguard"
	"github.com/googleapis/mcp-toolbox/internal/resultmask"
	"github.com/googleapis/mcp-toolbox/internal/sources/cockroachdb"
	"github.com/googleapis/mcp-toolbox/internal/sources/mysql"
	"github.com/googleapis/mcp-toolbox/internal/sources/postgres"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlexplain"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlrecommendindexes"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlslowqueryreport"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlsql"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
	"go.opentelemetry.io/otel/trace/noop"
)

func initContext() context.Context {
	return util.WithUserAgent(context.Background(), "queryguard-test")
}

type runner struct {
	guard *queryguard.Guard
	run   func(ctx context.Context, statement string) (any, error)
	// active counts statements still running on the server that contain marker.
	active func(ctx context.Context, marker string) (int, error)
}

func (r runner) invoke(ctx context.Context, t *testing.T, p queryguard.Policy, statement string) (any, *util.QueryError) {
	t.Helper()
	res, err := r.guard.Invoke(ctx, queryguard.Call{Tool: "test", Source: "test"}, p, func(ctx context.Context) (any, util.ToolboxError) {
		out, err := r.run(ctx, statement)
		if err != nil {
			return nil, util.NewAgentError("query failed: "+err.Error(), err)
		}
		return out, nil
	})
	if err == nil {
		return res, nil
	}
	var q *util.QueryError
	if !errors.As(err, &q) {
		t.Fatalf("got %T %v, want QueryError", err, err)
	}
	return nil, q
}

func (r runner) policy(t *testing.T) queryguard.Policy {
	t.Helper()
	p, err := r.guard.Policy(nil)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// waitIdle fails unless the marked statement leaves the server within d.
func (r runner) waitIdle(t *testing.T, marker string, d time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for {
		n, err := r.active(context.Background(), marker)
		if err != nil {
			t.Fatalf("checking active queries: %v", err)
		}
		if n == 0 {
			return time.Since(start)
		}
		if time.Since(start) > d {
			t.Fatalf("%d statement(s) with marker %q still running after %s", n, marker, d)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func wantCode(t *testing.T, got *util.QueryError, code string) {
	t.Helper()
	if got == nil {
		t.Fatalf("query succeeded, want %s", code)
	}
	if got.Code != code {
		t.Fatalf("code = %s (db %s), want %s", got.Code, got.DBErrorCode, code)
	}
}

func baseConfig() *queryguard.Config {
	return &queryguard.Config{
		MaxConcurrent:    2,
		QueryTimeout:     "3s",
		StatementTimeout: "1s",
		LockTimeout:      "500ms",
		MaxRows:          100,
		MaxResultBytes:   "1MiB",
	}
}

func postgresRunner(t *testing.T, c *queryguard.Config) runner {
	t.Helper()
	return runnerFor(postgresSource(t, c))
}

func postgresSource(t *testing.T, c *queryguard.Config) *postgres.Source {
	t.Helper()
	host := os.Getenv("QUERYGUARD_PG_HOST")
	if host == "" {
		t.Skip("QUERYGUARD_PG_HOST not set")
	}
	cfg := postgres.Config{
		Name: "pg", Type: "postgres", Host: host, Port: "5432", User: "postgres",
		Password: "test", Database: "postgres", QueryParams: map[string]string{"sslmode": "disable"}, QueryGuard: c,
	}
	src, err := cfg.Initialize(initContext(), noop.NewTracerProvider().Tracer(""))
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*postgres.Source)
	t.Cleanup(s.Pool.Close)
	if _, err := s.Pool.Exec(context.Background(),
		"CREATE TABLE IF NOT EXISTS qg_rows AS SELECT generate_series(1, 30000) AS g"); err != nil {
		t.Fatal(err)
	}
	return s
}

func runnerFor(s *postgres.Source) runner {
	return runner{
		guard: s.GetQueryGuard(),
		run: func(ctx context.Context, q string) (any, error) {
			return s.RunSQL(ctx, q, nil)
		},
		active: func(ctx context.Context, marker string) (int, error) {
			var n int
			err := s.Pool.QueryRow(ctx,
				"SELECT count(*) FROM pg_stat_activity WHERE state = 'active' AND query LIKE $1 AND query NOT LIKE '%pg_stat_activity%'",
				"%"+marker+"%").Scan(&n)
			return n, err
		},
	}
}

func cockroachRunner(t *testing.T, c *queryguard.Config) runner {
	t.Helper()
	host := os.Getenv("QUERYGUARD_CRDB_HOST")
	if host == "" {
		t.Skip("QUERYGUARD_CRDB_HOST not set")
	}
	cfg := cockroachdb.Config{
		Name: "crdb", Type: "cockroachdb", Host: host, Port: "26257", User: "root",
		Database: "defaultdb", QueryParams: map[string]string{"sslmode": "disable"},
		MaxRetries: 2, RetryBaseDelay: "200ms", ReadOnlyMode: true, MaxRowLimit: 1000, QueryGuard: c,
	}
	src, err := cfg.Initialize(initContext(), noop.NewTracerProvider().Tracer(""))
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*cockroachdb.Source)
	t.Cleanup(s.Pool.Close)
	return runner{
		guard: s.GetQueryGuard(),
		run: func(ctx context.Context, q string) (any, error) {
			rows, err := s.Query(ctx, q)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			var out []any
			for rows.Next() {
				v, err := rows.Values()
				if err != nil {
					return nil, err
				}
				out = append(out, v)
			}
			return out, rows.Err()
		},
		active: func(ctx context.Context, marker string) (int, error) {
			var n int
			err := s.Pool.QueryRow(ctx,
				"SELECT count(*) FROM [SHOW CLUSTER QUERIES] WHERE query LIKE $1 AND query NOT LIKE '%SHOW CLUSTER%'",
				"%"+marker+"%").Scan(&n)
			return n, err
		},
	}
}

func mysqlRunner(t *testing.T, c *queryguard.Config) runner {
	t.Helper()
	return mysqlRunnerFor(mysqlSource(t, c))
}

func mysqlSource(t *testing.T, c *queryguard.Config) *mysql.Source {
	t.Helper()
	return mysqlSourceIn(t, c, "mysql")
}

// mysqlAppSource connects to an application schema. Digest reports skip system
// schemas, so a session in `mysql` would not show up in them.
func mysqlAppSource(t *testing.T, c *queryguard.Config) *mysql.Source {
	t.Helper()
	s := mysqlSource(t, c)
	if _, err := s.Pool.ExecContext(context.Background(), "CREATE DATABASE IF NOT EXISTS qg_app"); err != nil {
		t.Fatal(err)
	}
	return mysqlSourceIn(t, c, "qg_app")
}

func mysqlSourceIn(t *testing.T, c *queryguard.Config, database string) *mysql.Source {
	t.Helper()
	host := os.Getenv("QUERYGUARD_MYSQL_HOST")
	if host == "" {
		t.Skip("QUERYGUARD_MYSQL_HOST not set")
	}
	cfg := mysql.Config{
		Name: "my", Type: "mysql", Host: host, Port: "3306", User: "root",
		Password: "test", Database: database, QueryGuard: c,
	}
	src, err := cfg.Initialize(initContext(), noop.NewTracerProvider().Tracer(""))
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*mysql.Source)
	t.Cleanup(func() { _ = s.Pool.Close() })
	if _, err := s.Pool.ExecContext(context.Background(),
		"CREATE TABLE IF NOT EXISTS qg_rows AS SELECT 1 AS id FROM information_schema.columns a, information_schema.columns b LIMIT 30000"); err != nil {
		t.Fatal(err)
	}
	return s
}

func mysqlRunnerFor(s *mysql.Source) runner {
	return runner{
		guard: s.GetQueryGuard(),
		run: func(ctx context.Context, q string) (any, error) {
			return s.RunSQL(ctx, q, nil)
		},
		active: func(ctx context.Context, marker string) (int, error) {
			var n int
			err := s.Pool.QueryRowContext(ctx,
				"SELECT count(*) FROM information_schema.processlist WHERE command = 'Query' AND info LIKE ? AND info NOT LIKE '%processlist%'",
				"%"+marker+"%").Scan(&n)
			return n, err
		},
	}
}

type dialect struct {
	name       string
	newRunner  func(*testing.T, *queryguard.Config) runner
	slow       string // runs far longer than the statement timeout
	manyRows   string // produces far more rows than any limit, slowly enough to stream
	bigValues  string // a few rows that exceed the byte limit
	ok         string
	serverStop string // database code expected from the server-side timeout
}

var dialects = []dialect{
	{
		name: "mysql", newRunner: mysqlRunner,
		slow: "SELECT /*MARK*/ count(*) FROM information_schema.columns a, information_schema.columns b, information_schema.columns c",
		// A hash join streams its probe side, unlike the join in slow.
		manyRows:   "SELECT /*MARK*/ STRAIGHT_JOIN a.id FROM qg_rows a, qg_rows b",
		bigValues:  "SELECT /*MARK*/ repeat('x', 1048576) FROM information_schema.columns LIMIT 5",
		ok:         "SELECT 1 AS one",
		serverStop: "3024",
	},
	{
		name: "postgres", newRunner: postgresRunner,
		slow: "SELECT /*MARK*/ pg_sleep(30)",
		// A join over a table streams, unlike a function scan, which materializes.
		manyRows:   "SELECT /*MARK*/ a.g FROM qg_rows a CROSS JOIN qg_rows b",
		bigValues:  "SELECT /*MARK*/ repeat('x', 1048576) FROM generate_series(1, 5)",
		ok:         "SELECT 1 AS one",
		serverStop: "57014",
	},
	{
		name: "cockroachdb", newRunner: cockroachRunner,
		slow:       "SELECT /*MARK*/ pg_sleep(30)",
		manyRows:   "SELECT /*MARK*/ g FROM generate_series(1, 2000000000) g",
		bigValues:  "SELECT /*MARK*/ repeat('x', 1048576) FROM generate_series(1, 5)",
		ok:         "SELECT 1 AS one",
		serverStop: "57014",
	},
}

func marked(q, marker string) string { return strings.ReplaceAll(q, "/*MARK*/", "/*"+marker+"*/") }

func uniqueMarker(t *testing.T) string {
	return fmt.Sprintf("qg_%d", time.Now().UnixNano())
}

func TestServerStopsBeforeToolbox(t *testing.T) {
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			r := d.newRunner(t, baseConfig())
			marker := uniqueMarker(t)
			start := time.Now()
			_, got := r.invoke(context.Background(), t, r.policy(t), marked(d.slow, marker))
			elapsed := time.Since(start)
			wantCode(t, got, util.QueryTimeout)
			if got.DBErrorCode != d.serverStop {
				t.Errorf("dbErrorCode = %q, want %q: the server, not Toolbox, should have stopped this", got.DBErrorCode, d.serverStop)
			}
			if elapsed >= 3*time.Second {
				t.Errorf("took %s: Toolbox deadline fired before the server limit", elapsed)
			}
			if got.Retryable {
				t.Error("timeouts must not be retryable")
			}
			if got.QueryID == "" {
				t.Error("queryId missing")
			}
			r.waitIdle(t, marker, 2*time.Second)
			if _, q := r.invoke(context.Background(), t, r.policy(t), d.ok); q != nil {
				t.Errorf("source unusable after timeout: %v", q)
			}
		})
	}
}

func TestRowLimitStopsReadingMidStream(t *testing.T) {
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			r := d.newRunner(t, baseConfig())
			marker := uniqueMarker(t)
			start := time.Now()
			res, got := r.invoke(context.Background(), t, r.policy(t), marked(d.manyRows, marker))
			wantCode(t, got, util.QueryRowLimit)
			if res != nil {
				t.Error("partial results must not be returned")
			}
			if e := time.Since(start); e > 900*time.Millisecond {
				t.Errorf("took %s: reading did not stop at the limit", e)
			}
			r.waitIdle(t, marker, 2*time.Second)
			if _, q := r.invoke(context.Background(), t, r.policy(t), d.ok); q != nil {
				t.Errorf("source unusable after limit: %v", q)
			}
		})
	}
}

func TestByteLimit(t *testing.T) {
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			r := d.newRunner(t, baseConfig())
			_, got := r.invoke(context.Background(), t, r.policy(t), marked(d.bigValues, uniqueMarker(t)))
			wantCode(t, got, util.QueryResultTooLarge)
		})
	}
}

func TestClientCancelLeavesNothingRunning(t *testing.T) {
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			r := d.newRunner(t, baseConfig())
			marker := uniqueMarker(t)
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(200*time.Millisecond, cancel)
			_, got := r.invoke(ctx, t, r.policy(t), marked(d.slow, marker))
			wantCode(t, got, util.QueryCanceled)
			// The server limit is the backstop, so allow for it to fire.
			left := r.waitIdle(t, marker, 3*time.Second)
			t.Logf("statement left the server %s after the client canceled", left)
		})
	}
}

func TestConcurrencyLimitReturnsBusy(t *testing.T) {
	for _, d := range dialects {
		t.Run(d.name, func(t *testing.T) {
			c := baseConfig()
			c.MaxConcurrent = 1
			r := d.newRunner(t, c)
			var wg sync.WaitGroup
			wg.Go(func() { _, _ = r.invoke(context.Background(), t, r.policy(t), marked(d.slow, uniqueMarker(t))) })
			time.Sleep(200 * time.Millisecond)
			_, got := r.invoke(context.Background(), t, r.policy(t), d.ok)
			wantCode(t, got, util.QueryBusy)
			wg.Wait()
		})
	}
}

func TestUnguardedBehaviorUnchanged(t *testing.T) {
	r := postgresRunner(t, nil)
	if r.guard != nil {
		t.Fatal("no config must mean no guard")
	}
	out, err := r.run(context.Background(), "SELECT generate_series(1, 5000)")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out.([]any)); n != 5000 {
		t.Errorf("got %d rows", n)
	}
}

func TestPostgresLockTimeout(t *testing.T) {
	s := postgresSource(t, baseConfig())
	r := runnerFor(s)
	ctx := context.Background()
	if _, err := s.Pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS qg_lock (id int)"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE qg_lock IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}

	_, got := r.invoke(ctx, t, r.policy(t), "SELECT * FROM qg_lock")
	wantCode(t, got, util.QueryLockTimeout)
	if got.DBErrorCode != "55P03" {
		t.Errorf("dbErrorCode = %q", got.DBErrorCode)
	}
}

// max_execution_time does not cover this statement, so only KILL QUERY stops it.
func TestMySQLKillsStatementsOutsideExecutionLimit(t *testing.T) {
	r := mysqlRunner(t, baseConfig())
	marker := uniqueMarker(t)
	start := time.Now()
	_, got := r.invoke(context.Background(), t, r.policy(t), "DO /*"+marker+"*/ SLEEP(30)")
	wantCode(t, got, util.QueryTimeout)
	if e := time.Since(start); e > 4*time.Second {
		t.Errorf("took %s", e)
	}
	r.waitIdle(t, marker, time.Second)
}

func TestMySQLClientCancelKillsImmediately(t *testing.T) {
	r := mysqlRunner(t, baseConfig())
	marker := uniqueMarker(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, got := r.invoke(ctx, t, r.policy(t), "DO /*"+marker+"*/ SLEEP(30)")
	wantCode(t, got, util.QueryCanceled)
	r.waitIdle(t, marker, time.Second)
}

// A kill must never land on a later statement that reuses the session.
func TestMySQLConnectionsStayUsableUnderChurn(t *testing.T) {
	c := baseConfig()
	c.MaxConcurrent = 8
	r := mysqlRunner(t, c)
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%5)*time.Millisecond)
			defer cancel()
			_, _ = r.invoke(ctx, t, r.policy(t), "SELECT SLEEP(0.002)")
		})
	}
	wg.Wait()
	for range 20 {
		if _, q := r.invoke(context.Background(), t, r.policy(t), "SELECT SLEEP(0.01)"); q != nil {
			t.Fatalf("healthy statement failed after churn: %v", q)
		}
	}
}

func TestMySQLLockWaitTimeout(t *testing.T) {
	// MySQL lock timeouts are whole seconds, so 1s must beat the statement limit.
	c := baseConfig()
	c.LockTimeout = "1s"
	c.StatementTimeout = "2500ms"
	s := mysqlSource(t, c)
	r := mysqlRunnerFor(s)
	ctx := context.Background()
	for _, q := range []string{
		"CREATE TABLE IF NOT EXISTS qg_lock (id int PRIMARY KEY) ENGINE=InnoDB",
		"INSERT IGNORE INTO qg_lock VALUES (1)",
	} {
		if _, err := s.Pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	tx, err := s.Pool.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT * FROM qg_lock WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}

	_, got := r.invoke(ctx, t, r.policy(t), "SELECT * FROM qg_lock WHERE id = 1 FOR UPDATE")
	wantCode(t, got, util.QueryLockTimeout)
	if got.DBErrorCode != "1205" {
		t.Errorf("dbErrorCode = %q", got.DBErrorCode)
	}
}

const plantedPhone = "13812345678"

// maskedTable creates the table the masking tests read, with values that cover
// Chinese text, NULL and the empty string.
func maskedTable(t *testing.T, exec func(string) error) {
	t.Helper()
	for _, q := range []string{
		"DROP TABLE IF EXISTS qg_customers",
		"CREATE TABLE qg_customers (id INT PRIMARY KEY, customer_name VARCHAR(50), phone VARCHAR(20), email VARCHAR(100))",
		"INSERT INTO qg_customers VALUES (1, '张三丰', '" + plantedPhone + "', 'zhang.san@example.com'), (2, '李', '', NULL), (3, NULL, NULL, 'not-an-email')",
	} {
		if err := exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func customerMask() *resultmask.Config {
	return &resultmask.Config{
		Allow: []string{"id"},
		Rules: []resultmask.Rule{
			{Column: "customer_name", Action: resultmask.ActionKeep, First: 1},
			{Column: "phone", Action: resultmask.ActionKeep, First: 3, Last: 4, Replacement: "****"},
			{Column: "email", Action: resultmask.ActionEmail},
		},
	}
}

const wantMasked = `[{"id":1,"customer_name":"张***","phone":"138****5678","email":"z***@example.com"},` +
	`{"id":2,"customer_name":"***","phone":"","email":null},` +
	`{"id":3,"customer_name":null,"phone":null,"email":"***"}]`

func TestMaskingRealRows(t *testing.T) {
	tests := []struct {
		name   string
		runner func(t *testing.T) (runner, func(string) error)
	}{
		{"mysql", func(t *testing.T) (runner, func(string) error) {
			s := mysqlSource(t, baseConfig())
			return mysqlRunnerFor(s), func(q string) error {
				_, err := s.Pool.ExecContext(context.Background(), q)
				return err
			}
		}},
		{"postgres", func(t *testing.T) (runner, func(string) error) {
			s := postgresSource(t, baseConfig())
			return runnerFor(s), func(q string) error {
				_, err := s.Pool.Exec(context.Background(), q)
				return err
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, exec := tc.runner(t)
			maskedTable(t, exec)
			m, err := resultmask.New(customerMask())
			if err != nil {
				t.Fatal(err)
			}
			res, qErr := r.invoke(initContext(), t, r.policy(t), "SELECT id, customer_name, phone, email FROM qg_customers ORDER BY id")
			if qErr != nil {
				t.Fatal(qErr)
			}
			masked, err := m.Apply(res)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(masked)
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != wantMasked {
				t.Fatalf("got  %s\nwant %s", b, wantMasked)
			}

			// SELECT * after a column is added must not expose it.
			if err := exec("ALTER TABLE qg_customers ADD COLUMN address VARCHAR(50) DEFAULT 'secret'"); err != nil {
				t.Fatal(err)
			}
			res, qErr = r.invoke(initContext(), t, r.policy(t), "SELECT * FROM qg_customers")
			if qErr != nil {
				t.Fatal(qErr)
			}
			if _, err := m.Apply(res); err == nil || !strings.Contains(err.Error(), "address") {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func explainTool(t *testing.T, statement string, params parameters.Parameters) tools.Tool {
	t.Helper()
	target := mysqlsql.Config{
		ConfigBase: tools.ConfigBase{Name: "query", Description: "d"},
		Type:       "mysql-sql", Source: "my", Statement: statement, Parameters: params,
	}
	cfg := mysqlexplain.Config{
		ConfigBase: tools.ConfigBase{Name: "explain", Description: "d"},
		Type:       "mysql-explain", Tool: "query",
	}
	bound, err := cfg.BindTools(map[string]tools.ToolConfig{"query": target})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bound.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestMySQLExplainSummarizesWithoutValuesOrExecution(t *testing.T) {
	s := mysqlSource(t, baseConfig())
	r := mysqlRunnerFor(s)
	maskedTable(t, func(q string) error {
		_, err := s.Pool.ExecContext(context.Background(), q)
		return err
	})
	tool := explainTool(t,
		"SELECT id FROM qg_customers WHERE phone = ? AND email LIKE CONCAT('%', ?, '%') AND SLEEP(5) = 0",
		parameters.Parameters{
			parameters.NewStringParameter("phone", "phone"),
			parameters.NewStringParameter("email", "email"),
		})
	params := parameters.ParamValues{{Name: "phone", Value: plantedPhone}, {Name: "email", Value: "secret-" + plantedPhone}}

	start := time.Now()
	res, err := r.guard.Invoke(initContext(), queryguard.Call{Tool: "explain", Source: "my"}, r.policy(t),
		func(ctx context.Context) (any, util.ToolboxError) { return tool.Invoke(ctx, s, params, "") })
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("took %s: the statement seems to have run", d)
	}
	b, jsonErr := json.Marshal(res)
	if jsonErr != nil {
		t.Fatal(jsonErr)
	}
	if strings.Contains(string(b), plantedPhone) {
		t.Fatalf("value leaked: %s", b)
	}
	sum := res.(mysqlexplain.Summary)
	if len(sum.Tables) != 1 || sum.Tables[0].Table != "qg_customers" || sum.EstimatedCost == "" {
		t.Fatalf("unexpected summary: %s", b)
	}
}

func TestMySQLExplainDoesNotWrite(t *testing.T) {
	s := mysqlSource(t, baseConfig())
	r := mysqlRunnerFor(s)
	exec := func(q string) error {
		_, err := s.Pool.ExecContext(context.Background(), q)
		return err
	}
	maskedTable(t, exec)
	tool := explainTool(t, "DELETE FROM qg_customers", nil)
	if _, err := r.guard.Invoke(initContext(), queryguard.Call{Tool: "explain", Source: "my"}, r.policy(t),
		func(ctx context.Context) (any, util.ToolboxError) { return tool.Invoke(ctx, s, nil, "") }); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM qg_customers").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("rows = %d, EXPLAIN must not execute the statement", n)
	}
}

// analysisTable creates a table large enough to be worth indexing, using
// repeated doubling, and runs the query under test so it appears in digests.
func analysisTable(t *testing.T, s *mysql.Source) {
	t.Helper()
	ctx := context.Background()
	for _, q := range []string{
		"DROP TABLE IF EXISTS qg_big",
		"CREATE TABLE qg_big (id INT PRIMARY KEY AUTO_INCREMENT, phone VARCHAR(20), status VARCHAR(10), notes TEXT)",
		"INSERT INTO qg_big (phone, status, notes) VALUES ('" + plantedPhone + "', 'new', 'x')",
	} {
		if _, err := s.Pool.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	for range 13 {
		if _, err := s.Pool.ExecContext(ctx, "INSERT INTO qg_big (phone, status, notes) SELECT CONCAT('1', FLOOR(RAND() * 1e10)), 'old', 'x' FROM qg_big"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Pool.ExecContext(ctx, "ANALYZE TABLE qg_big"); err != nil {
		t.Fatal(err)
	}
}

func recommendTool(t *testing.T, statement string, params parameters.Parameters) tools.Tool {
	t.Helper()
	target := mysqlsql.Config{
		ConfigBase: tools.ConfigBase{Name: "query", Description: "d"},
		Type:       "mysql-sql", Source: "my", Statement: statement, Parameters: params,
	}
	cfg := mysqlrecommendindexes.Config{
		ConfigBase: tools.ConfigBase{Name: "recommend", Description: "d"},
		Type:       "mysql-recommend-indexes", Tool: "query",
	}
	bound, err := cfg.BindTools(map[string]tools.ToolConfig{"query": target})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bound.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func guarded(t *testing.T, r runner, fn func(context.Context) (any, util.ToolboxError)) any {
	t.Helper()
	res, err := r.guard.Invoke(initContext(), queryguard.Call{Tool: "analysis", Source: "my"}, r.policy(t), fn)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestMySQLRecommendIndexes(t *testing.T) {
	s := mysqlAppSource(t, baseConfig())
	r := mysqlRunnerFor(s)
	analysisTable(t, s)

	const statement = "SELECT id FROM qg_big b WHERE b.phone = ? AND b.status = ? AND b.notes = 'x'"
	// Bound parameters use the binary protocol, which server digests do not count,
	// so these calls can only be found through what Toolbox measured.
	call := queryguard.Call{Tool: "query", Source: "my"}
	for range 3 {
		if _, err := r.guard.Invoke(initContext(), call, r.policy(t), func(ctx context.Context) (any, util.ToolboxError) {
			if _, err := s.RunSQL(ctx, statement, []any{plantedPhone, "new"}); err != nil {
				return nil, util.NewAgentError("query failed", err)
			}
			return []any{}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	tool := recommendTool(t, statement, parameters.Parameters{
		parameters.NewStringParameter("phone", "phone"),
		parameters.NewStringParameter("status", "status"),
	})
	params := parameters.ParamValues{{Name: "phone", Value: plantedPhone}, {Name: "status", Value: "new"}}
	res := guarded(t, r, func(ctx context.Context) (any, util.ToolboxError) { return tool.Invoke(ctx, s, params, "") })

	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("report: %s", b)
	if strings.Contains(string(b), plantedPhone) {
		t.Fatalf("value leaked: %s", b)
	}
	report := res.(mysqlrecommendindexes.Report)
	if len(report.Recommendations) != 1 {
		t.Fatalf("recommendations: %s", b)
	}
	rec := report.Recommendations[0]
	// Both are equality filters, so their relative order is not significant.
	cols := slices.Sorted(slices.Values(rec.Columns))
	if rec.Table != "qg_big" || strings.Join(cols, ",") != "phone,status" {
		t.Fatalf("got %+v: TEXT columns must be skipped and the alias resolved", rec)
	}
	if rec.Status != "requires_dba_approval" {
		t.Errorf("status = %q", rec.Status)
	}
	if report.Digest == "" {
		t.Errorf("no digest: %s", b)
	}
	if report.Runtime == nil || report.Runtime.Calls != 3 || report.Runtime.MeasuredBy != "toolbox" {
		t.Errorf("runtime statistics not found: %s", b)
	}

	// The tool only suggests. No index may appear until a person adds it.
	var n int
	if err := s.Pool.QueryRow("SELECT COUNT(*) FROM information_schema.statistics WHERE table_name = 'qg_big' AND index_name <> 'PRIMARY'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d indexes were created", n)
	}

	// The suggested DDL must be valid, and must actually fix the plan.
	if _, err := s.Pool.Exec(rec.SuggestedDDL); err != nil {
		t.Fatalf("suggested DDL failed: %v", err)
	}
	after := guarded(t, r, func(ctx context.Context) (any, util.ToolboxError) { return tool.Invoke(ctx, s, params, "") })
	if got := after.(mysqlrecommendindexes.Report).Recommendations; len(got) != 0 {
		t.Fatalf("still recommended after applying: %+v", got)
	}
	if _, err := s.Pool.Exec(rec.RollbackDDL); err != nil {
		t.Fatalf("rollback DDL failed: %v", err)
	}
}

func TestMySQLSlowQueryReport(t *testing.T) {
	s := mysqlAppSource(t, baseConfig())
	r := mysqlRunnerFor(s)
	analysisTable(t, s)
	statement := "SELECT COUNT(*) FROM qg_big WHERE phone = '" + plantedPhone + "'"
	for range 3 {
		if _, err := s.RunSQL(initContext(), statement, nil); err != nil {
			t.Fatal(err)
		}
	}
	const parameterized = "SELECT id FROM qg_big WHERE phone = ?"
	for range 3 {
		if _, err := r.guard.Invoke(initContext(), queryguard.Call{Tool: "bound_tool", Source: "my"}, r.policy(t), func(ctx context.Context) (any, util.ToolboxError) {
			if _, err := s.RunSQL(ctx, parameterized, []any{plantedPhone}); err != nil {
				return nil, util.NewAgentError("query failed", err)
			}
			return []any{}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	bound, err := mysqlslowqueryreport.Config{
		ConfigBase: tools.ConfigBase{Name: "report"}, Type: "mysql-slow-query-report", Source: "my",
	}.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	params := parameters.ParamValues{{Name: "database", Value: "qg_app"}, {Name: "limit", Value: 50}}
	res := guarded(t, r, func(ctx context.Context) (any, util.ToolboxError) { return bound.Invoke(ctx, s, params, "") })
	b, jsonErr := json.Marshal(res)
	if jsonErr != nil {
		t.Fatal(jsonErr)
	}
	t.Logf("report: %.900s", b)
	if strings.Contains(string(b), plantedPhone) {
		t.Fatalf("literal leaked: %.400s", b)
	}
	if !strings.Contains(string(b), "qg_big") || !strings.Contains(string(b), "full_scan") || !strings.Contains(string(b), "low_row_efficiency") {
		t.Fatalf("statement or findings missing: %.800s", b)
	}
	for _, field := range []string{`"p95_ms"`, `"digest"`, `"calls"`} {
		if !strings.Contains(string(b), field) {
			t.Fatalf("%s missing: %.400s", field, b)
		}
	}
	// The whole reason for the toolbox section: this statement is parameterized.
	found := false
	for _, st := range res.(mysqlslowqueryreport.Report).Toolbox {
		if st.Tool == "bound_tool" {
			found = st.Calls == 3
		}
	}
	if !found {
		t.Fatalf("parameterized tool not reported: %.600s", b)
	}
}
