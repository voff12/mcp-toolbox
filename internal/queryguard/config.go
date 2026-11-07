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

// Package queryguard bounds what a single tool invocation can do to a database
// and to Toolbox: how long it may queue and run, how many may run at once and
// how much it may read. See docs/plans for the design.
//
// Enforcement has two halves. Guard.Invoke wraps Tool.Invoke and owns queueing
// and the overall deadline. Execution travels in the context to the source's
// read loop, which is the only place rows can be counted before they are all in
// memory and the only place the final SQL is known.
package queryguard

import (
	"fmt"
	"maps"
	"math"
	"strconv"
	"strings"
	"time"
)

// Config is the `queryGuard` block of a source. Sources that have not adapted
// their read path do not declare the field, so strict config decoding rejects
// it for them rather than silently ignoring the limits.
type Config struct {
	MaxConcurrent      int    `yaml:"maxConcurrent"`
	MaxQueue           int    `yaml:"maxQueue"`
	QueueTimeout       string `yaml:"queueTimeout"`
	QueryTimeout       string `yaml:"queryTimeout"`
	StatementTimeout   string `yaml:"statementTimeout"`
	LockTimeout        string `yaml:"lockTimeout"`
	MaxRows            int    `yaml:"maxRows"`
	MaxResultBytes     string `yaml:"maxResultBytes"`
	SlowQueryThreshold string `yaml:"slowQueryThreshold"`
}

// ToolConfig is the `queryGuard` block of a tool. It can only tighten the
// limits of the tool's source.
type ToolConfig struct {
	QueryTimeout       string `yaml:"queryTimeout"`
	MaxRows            int    `yaml:"maxRows"`
	MaxResultBytes     string `yaml:"maxResultBytes"`
	SlowQueryThreshold string `yaml:"slowQueryThreshold"`
	FallbackTool       string `yaml:"fallbackTool"`
}

// Provider is implemented by sources that enforce query guard limits. A nil
// Guard means the source has no `queryGuard` block.
type Provider interface {
	GetQueryGuard() *Guard
}

// Limits are the parsed values of Config.
type Limits struct {
	MaxConcurrent      int
	MaxQueue           int
	QueueTimeout       time.Duration
	QueryTimeout       time.Duration
	StatementTimeout   time.Duration
	LockTimeout        time.Duration
	MaxRows            int64
	MaxResultBytes     int64
	SlowQueryThreshold time.Duration
}

const defaultSlowQueryThreshold = time.Second

func (c *Config) parse() (Limits, error) {
	l := Limits{
		MaxConcurrent: c.MaxConcurrent,
		MaxQueue:      c.MaxQueue,
		MaxRows:       int64(c.MaxRows),
	}
	var err error
	for _, f := range []struct {
		name string
		src  string
		dst  *time.Duration
	}{
		{"queueTimeout", c.QueueTimeout, &l.QueueTimeout},
		{"queryTimeout", c.QueryTimeout, &l.QueryTimeout},
		{"statementTimeout", c.StatementTimeout, &l.StatementTimeout},
		{"lockTimeout", c.LockTimeout, &l.LockTimeout},
		{"slowQueryThreshold", c.SlowQueryThreshold, &l.SlowQueryThreshold},
	} {
		if *f.dst, err = parseDuration(f.src); err != nil {
			return Limits{}, fmt.Errorf("queryGuard.%s: %w", f.name, err)
		}
	}
	if l.MaxResultBytes, err = parseBytes(c.MaxResultBytes); err != nil {
		return Limits{}, fmt.Errorf("queryGuard.maxResultBytes: %w", err)
	}
	if l.SlowQueryThreshold == 0 {
		l.SlowQueryThreshold = defaultSlowQueryThreshold
	}

	switch {
	case l.MaxConcurrent <= 0:
		return Limits{}, fmt.Errorf("queryGuard.maxConcurrent must be positive")
	case l.MaxQueue < 0:
		return Limits{}, fmt.Errorf("queryGuard.maxQueue must not be negative")
	case l.MaxQueue > 0 && l.QueueTimeout <= 0:
		return Limits{}, fmt.Errorf("queryGuard.queueTimeout is required when maxQueue is set")
	case l.QueryTimeout <= 0:
		return Limits{}, fmt.Errorf("queryGuard.queryTimeout is required")
	case l.StatementTimeout <= 0:
		return Limits{}, fmt.Errorf("queryGuard.statementTimeout is required: without it a timed-out query can keep running on the server")
	case l.MaxRows <= 0:
		return Limits{}, fmt.Errorf("queryGuard.maxRows is required")
	case l.MaxResultBytes <= 0:
		return Limits{}, fmt.Errorf("queryGuard.maxResultBytes is required")
	}
	if err := checkOrdering(l, l.QueryTimeout); err != nil {
		return Limits{}, err
	}
	return l, nil
}

// checkOrdering enforces that the database gives up before Toolbox does.
// Otherwise Toolbox abandons a statement that keeps consuming server resources.
func checkOrdering(l Limits, queryTimeout time.Duration) error {
	if l.StatementTimeout+l.QueueTimeout >= queryTimeout {
		return fmt.Errorf("queryGuard: statementTimeout (%s) + queueTimeout (%s) must be less than queryTimeout (%s)", l.StatementTimeout, l.QueueTimeout, queryTimeout)
	}
	return nil
}

// Policy is the effective limit set for one tool.
type Policy struct {
	QueryTimeout       time.Duration
	MaxRows            int64
	MaxResultBytes     int64
	SlowQueryThreshold time.Duration
	FallbackTool       string
}

// Policy resolves the limits for a tool, rejecting any that loosen the source.
func (g *Guard) Policy(t *ToolConfig) (Policy, error) {
	p := Policy{
		QueryTimeout:       g.limits.QueryTimeout,
		MaxRows:            g.limits.MaxRows,
		MaxResultBytes:     g.limits.MaxResultBytes,
		SlowQueryThreshold: g.limits.SlowQueryThreshold,
	}
	if t == nil {
		return p, nil
	}
	p.FallbackTool = t.FallbackTool

	queryTimeout, err := parseDuration(t.QueryTimeout)
	if err != nil {
		return Policy{}, fmt.Errorf("queryGuard.queryTimeout: %w", err)
	}
	if queryTimeout != 0 {
		if queryTimeout > p.QueryTimeout {
			return Policy{}, fmt.Errorf("queryGuard.queryTimeout (%s) must not exceed the source limit (%s)", queryTimeout, p.QueryTimeout)
		}
		if err := checkOrdering(g.limits, queryTimeout); err != nil {
			return Policy{}, err
		}
		p.QueryTimeout = queryTimeout
	}
	if t.MaxRows < 0 {
		return Policy{}, fmt.Errorf("queryGuard.maxRows must not be negative")
	}
	if t.MaxRows != 0 {
		if int64(t.MaxRows) > p.MaxRows {
			return Policy{}, fmt.Errorf("queryGuard.maxRows (%d) must not exceed the source limit (%d)", t.MaxRows, p.MaxRows)
		}
		p.MaxRows = int64(t.MaxRows)
	}
	maxBytes, err := parseBytes(t.MaxResultBytes)
	if err != nil {
		return Policy{}, fmt.Errorf("queryGuard.maxResultBytes: %w", err)
	}
	if maxBytes != 0 {
		if maxBytes > p.MaxResultBytes {
			return Policy{}, fmt.Errorf("queryGuard.maxResultBytes (%d) must not exceed the source limit (%d)", maxBytes, p.MaxResultBytes)
		}
		p.MaxResultBytes = maxBytes
	}
	slow, err := parseDuration(t.SlowQueryThreshold)
	if err != nil {
		return Policy{}, fmt.Errorf("queryGuard.slowQueryThreshold: %w", err)
	}
	if slow != 0 {
		p.SlowQueryThreshold = slow
	}
	return p, nil
}

func millis(d time.Duration) string {
	return strconv.FormatInt(d.Milliseconds(), 10)
}

// ceilSeconds is for MySQL settings that only take whole seconds.
func ceilSeconds(d time.Duration) string {
	return strconv.FormatInt(int64(math.Ceil(d.Seconds())), 10)
}

// PostgresParams returns connection parameters that make the server enforce
// the limits. It is also used for CockroachDB. A nil Guard returns params as is.
func (g *Guard) PostgresParams(params map[string]string) map[string]string {
	if g == nil {
		return params
	}
	out := make(map[string]string, len(params)+2)
	maps.Copy(out, params)
	out["statement_timeout"] = millis(g.limits.StatementTimeout)
	if g.limits.LockTimeout > 0 {
		out["lock_timeout"] = millis(g.limits.LockTimeout)
	}
	return out
}

// MySQLParams is the MySQL counterpart of PostgresParams. `max_execution_time`
// only covers read-only SELECT statements, and MariaDB does not have it: there
// the connection fails, which is intended over running without a server limit.
func (g *Guard) MySQLParams(params map[string]string) map[string]string {
	if g == nil {
		return params
	}
	out := make(map[string]string, len(params)+3)
	maps.Copy(out, params)
	out["max_execution_time"] = millis(g.limits.StatementTimeout)
	if g.limits.LockTimeout > 0 {
		out["lock_wait_timeout"] = ceilSeconds(g.limits.LockTimeout)
		out["innodb_lock_wait_timeout"] = ceilSeconds(g.limits.LockTimeout)
	}
	return out
}

func parseDuration(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative: %q", s)
	}
	return d, nil
}

func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}} {
		if num, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = strings.TrimSpace(num), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q: use a number of bytes or a KiB, MiB or GiB suffix", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q is too large", s)
	}
	return n * mult, nil
}
