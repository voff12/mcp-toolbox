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
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/googleapis/mcp-toolbox/internal/telemetry"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	outcomeSuccess  = "success"
	maxPendingEvent = 1024

	msgTimeout = "The query exceeded its execution budget. Narrow the scope or contact an administrator for analysis."
	msgLock    = "The query timed out waiting for a lock. Do not retry automatically; contact an administrator if this persists."
	msgBusy    = "The data source is at capacity. Do not retry immediately; reduce concurrent requests."
	msgCancel  = "The query was canceled."
	msgRows    = "The result exceeded the row limit. Narrow the query; partial results are not returned."
	msgBytes   = "The result exceeded the size limit. Select fewer columns or rows; partial results are not returned."
	msgFailed  = "The query failed. See dbErrorCode, or contact an administrator."
	msgBypass  = "This tool does not support query guard limits on this source, so its result was withheld."
)

// Guard enforces a source's limits. It is shared by all tools on the source.
type Guard struct {
	limits  Limits
	slots   chan struct{}
	waiting atomic.Int64
	// pendingEvents bounds in-flight asynchronous log writes.
	pendingEvents chan struct{}
	stats         statTable
}

// New builds a Guard. A nil config returns a nil Guard.
func New(c *Config) (*Guard, error) {
	if c == nil {
		return nil, nil
	}
	l, err := c.parse()
	if err != nil {
		return nil, err
	}
	return &Guard{
		limits:        l,
		slots:         make(chan struct{}, l.MaxConcurrent),
		pendingEvents: make(chan struct{}, maxPendingEvent),
	}, nil
}

// Call identifies a guarded invocation.
type Call struct {
	Tool   string
	Source string
}

// Invoke runs fn under the policy. fn must use the context it is given.
func (g *Guard) Invoke(parent context.Context, call Call, p Policy, fn func(context.Context) (any, util.ToolboxError)) (res any, retErr util.ToolboxError) {
	start := time.Now()
	ev := event{Call: call, RequestID: uuid.NewString(), Outcome: outcomeSuccess}
	exec := &Execution{policy: p}
	defer func() {
		ev.QueryID = exec.id()
		ev.TotalMs = time.Since(start).Milliseconds()
		ev.DBMs = time.Duration(exec.dbNanos.Load()).Milliseconds()
		ev.Rows = exec.rows.Load()
		ev.Bytes = exec.bytes.Load()
		g.stats.record(ev)
		g.emit(parent, ev, p)
	}()

	ctx, cancel := context.WithTimeout(parent, p.QueryTimeout)
	defer cancel()

	release, qErr := g.acquire(ctx, parent, p)
	ev.QueueMs = time.Since(start).Milliseconds()
	if qErr != nil {
		ev.Outcome, ev.ErrorCode = qErr.Code, qErr.Code
		return nil, qErr
	}
	defer release()

	active(parent, call, 1)
	defer active(parent, call, -1)

	res, err := fn(withExecution(ctx, exec))
	if err != nil {
		if passThrough(err, exec) {
			ev.Outcome = string(err.Category())
			ev.ErrorCode = util.ErrorType(err)
			return nil, err
		}
		qErr := classify(ctx, parent, err)
		qErr.QueryID, qErr.FallbackTool = exec.id(), p.FallbackTool
		ev.Outcome, ev.ErrorCode, ev.DBErrorCode = qErr.Code, qErr.Code, qErr.DBErrorCode
		return nil, qErr
	}
	if exec.statements.Load() == 0 {
		// Nothing was charged to the budget, so the tool reached the database
		// some other way and its result may be unbounded. Fail closed.
		ev.Outcome, ev.ErrorCode = util.QueryGuardUnenforced, util.QueryGuardUnenforced
		return nil, &util.QueryError{Code: util.QueryGuardUnenforced, Message: msgBypass}
	}
	return res, nil
}

func (e *Execution) id() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.queryID
}

func (g *Guard) acquire(ctx, parent context.Context, p Policy) (func(), *util.QueryError) {
	release := func() { <-g.slots }
	select {
	case g.slots <- struct{}{}:
		return release, nil
	default:
	}
	busy := &util.QueryError{Code: util.QueryBusy, Message: msgBusy, FallbackTool: p.FallbackTool}
	if g.waiting.Add(1) > int64(g.limits.MaxQueue) {
		g.waiting.Add(-1)
		return nil, busy
	}
	defer g.waiting.Add(-1)

	timer := time.NewTimer(g.limits.QueueTimeout)
	defer timer.Stop()
	select {
	case g.slots <- struct{}{}:
		return release, nil
	case <-timer.C:
		return nil, busy
	case <-ctx.Done():
		if parent.Err() != nil {
			return nil, &util.QueryError{Code: util.QueryCanceled, Message: msgCancel}
		}
		return nil, busy
	}
}

// passThrough reports whether err can reach the caller unchanged: auth errors,
// which clients must act on, and agent errors raised before any statement ran,
// such as parameter validation, which cannot contain database values.
func passThrough(err util.ToolboxError, e *Execution) bool {
	var csErr *util.ClientServerError
	if errors.As(err, &csErr) {
		return csErr.Code == http.StatusUnauthorized || csErr.Code == http.StatusForbidden
	}
	return err.Category() == util.CategoryAgent && e.statements.Load() == 0
}

// classify maps an error to the stable contract. The original text is dropped,
// because database errors can quote row and parameter values.
func classify(ctx, parent context.Context, err error) *util.QueryError {
	q := &util.QueryError{Code: util.QueryFailed, Message: msgFailed}

	// Raised by Toolbox itself after the statement ran, so already safe to return.
	var own *util.QueryError
	if errors.As(err, &own) {
		c := *own
		return &c
	}

	var limit *LimitError
	if errors.As(err, &limit) {
		q.Code, q.Message = limit.Code, msgRows
		if limit.Code == util.QueryResultTooLarge {
			q.Message = msgBytes
		}
		return q
	}

	var pgErr *pgconn.PgError
	var myErr *mysql.MySQLError
	switch {
	case errors.As(err, &pgErr):
		q.DBErrorCode = pgErr.Code
		switch pgErr.Code {
		case "57014": // query_canceled, raised by statement_timeout
			q.Code, q.Message = util.QueryTimeout, msgTimeout
			return q
		case "55P03": // lock_not_available, raised by lock_timeout
			q.Code, q.Message = util.QueryLockTimeout, msgLock
			return q
		}
	case errors.As(err, &myErr):
		q.DBErrorCode = strconv.Itoa(int(myErr.Number))
		switch myErr.Number {
		case 3024: // ER_QUERY_TIMEOUT, raised by max_execution_time
			q.Code, q.Message = util.QueryTimeout, msgTimeout
			return q
		case 1205, 3572: // lock wait timeout, NOWAIT conflict
			q.Code, q.Message = util.QueryLockTimeout, msgLock
			return q
		}
	}

	switch {
	case parent.Err() != nil:
		q.Code, q.Message, q.DBErrorCode = util.QueryCanceled, msgCancel, ""
	case errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil:
		q.Code, q.Message, q.DBErrorCode = util.QueryTimeout, msgTimeout, ""
	}
	return q
}

type event struct {
	Call
	RequestID   string
	QueryID     string
	Outcome     string
	ErrorCode   string
	DBErrorCode string
	QueueMs     int64
	DBMs        int64
	TotalMs     int64
	Rows        int64
	Bytes       int64
}

func metricAttrs(c Call, outcome string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("gen_ai.tool.name", c.Tool),
		attribute.String("toolbox.source.name", c.Source),
		attribute.String("toolbox.query.outcome", outcome),
	)
}

func instruments(ctx context.Context) *telemetry.Instrumentation {
	inst, err := util.InstrumentationFromContext(ctx)
	if err != nil || inst.QueryOutcomes == nil {
		return nil
	}
	return inst
}

func active(ctx context.Context, c Call, delta int64) {
	if inst := instruments(ctx); inst != nil {
		inst.QueryActive.Add(ctx, delta, metric.WithAttributes(attribute.String("toolbox.source.name", c.Source)))
	}
}

// emit records metrics and the audit log entry. It never records SQL text,
// parameters, results or raw errors. Writes are asynchronous so a slow log sink
// cannot hold a query slot; when the backlog is full, timeouts are still written
// inline, as they must not be lost, and anything else is counted and dropped.
func (g *Guard) emit(parent context.Context, ev event, p Policy) {
	// The request context may already be canceled; telemetry must still record.
	ctx := context.WithoutCancel(parent)

	inst := instruments(ctx)
	if inst != nil {
		attrs := metricAttrs(ev.Call, ev.Outcome)
		inst.QueryOutcomes.Add(ctx, 1, attrs)
		inst.QueryQueueDuration.Record(ctx, float64(ev.QueueMs)/1000, attrs)
		inst.QueryRows.Record(ctx, ev.Rows, attrs)
		inst.QueryResultBytes.Record(ctx, ev.Bytes, attrs)
	}

	logger, err := util.LoggerFromContext(ctx)
	if err != nil {
		return
	}
	slow := time.Duration(ev.TotalMs)*time.Millisecond >= p.SlowQueryThreshold
	level := slog.LevelWarn
	if ev.Outcome == outcomeSuccess && !slow {
		level = slog.LevelDebug
	}
	attrs := []any{
		"event", "slow_query",
		"queryId", ev.QueryID,
		"tool", ev.Tool,
		"sourceName", ev.Source,
		"outcome", ev.Outcome,
		"queueMs", ev.QueueMs,
		"dbMs", ev.DBMs,
		"totalMs", ev.TotalMs,
		"rowsReturned", ev.Rows,
		"resultBytes", ev.Bytes,
		"errorCode", ev.ErrorCode,
		"dbErrorCode", ev.DBErrorCode,
		"requestId", ev.RequestID,
		"callerService", "unknown",
	}
	write := func() { logger.SlogLogger().Log(ctx, level, "query guard event", attrs...) }

	select {
	case g.pendingEvents <- struct{}{}:
		go func() {
			defer func() { <-g.pendingEvents }()
			write()
		}()
	default:
		if ev.ErrorCode == util.QueryTimeout || ev.ErrorCode == util.QueryLockTimeout {
			write()
			return
		}
		if inst != nil {
			inst.QueryDroppedEvents.Add(ctx, 1)
		}
	}
}
