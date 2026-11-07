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

package util

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// Stable error codes returned to callers by the query guard. Callers should
// treat every one of them as a signal to stop or narrow the request, not to
// retry.
const (
	QueryTimeout         = "QUERY_TIMEOUT"
	QueryLockTimeout     = "QUERY_LOCK_TIMEOUT"
	QueryBusy            = "QUERY_BUSY"
	QueryCanceled        = "QUERY_CANCELED"
	QueryRowLimit        = "QUERY_ROW_LIMIT_EXCEEDED"
	QueryResultTooLarge  = "QUERY_RESULT_TOO_LARGE"
	QueryFailed          = "QUERY_FAILED"
	QueryGuardUnenforced = "QUERY_GUARD_UNENFORCED"
	QueryMaskingFailed   = "QUERY_MASKING_FAILED"
)

// QueryError is a machine-readable agent error. It deliberately has no cause:
// database errors can quote row or parameter values, so the underlying text
// must never reach the caller, logs or metrics through this type.
type QueryError struct {
	Code         string `json:"errorCode"`
	Retryable    bool   `json:"retryable"`
	QueryID      string `json:"queryId,omitempty"`
	Message      string `json:"message"`
	FallbackTool string `json:"fallbackTool,omitempty"`
	// DBErrorCode is the database's own error code (for example a Postgres
	// SQLSTATE). It carries no data values and lets a caller distinguish a
	// syntax error from a permissions error.
	DBErrorCode string `json:"dbErrorCode,omitempty"`
}

var _ ToolboxError = &QueryError{}

// Error returns the JSON form, which every response path already sends as the
// error text, so HTTP and all MCP versions share one contract.
func (e *QueryError) Error() string {
	b, err := json.Marshal(e)
	if err != nil {
		return e.Code
	}
	return string(b)
}

func (e *QueryError) Category() ErrorCategory { return CategoryAgent }

func (e *QueryError) Unwrap() error { return nil }

// ErrorType returns a low-cardinality value for the `error.type` metric
// attribute. Error text must not be used there: it can contain data values and
// makes the attribute unbounded.
func ErrorType(err error) string {
	var qErr *QueryError
	if errors.As(err, &qErr) {
		return qErr.Code
	}
	var tbErr ToolboxError
	if errors.As(err, &tbErr) {
		return string(tbErr.Category())
	}
	return "_OTHER"
}

// SQLFingerprint returns a short stable hash of a statement with comments
// removed, literals replaced and whitespace and case normalized, so statements
// differing only in their values share a fingerprint. It is safe to log.
func SQLFingerprint(statement string) string {
	sum := sha256.Sum256([]byte(normalizeSQL(statement)))
	return hex.EncodeToString(sum[:8])
}

func normalizeSQL(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	r := []rune(s)
	pendingSpace := false
	write := func(c rune) {
		if pendingSpace && b.Len() > 0 {
			b.WriteByte(' ')
		}
		pendingSpace = false
		b.WriteRune(unicode.ToLower(c))
	}
	isWord := func(c rune) bool {
		return c == '_' || c == '$' || unicode.IsLetter(c) || unicode.IsDigit(c)
	}
	for i := 0; i < len(r); i++ {
		c := r[i]
		switch {
		case unicode.IsSpace(c):
			pendingSpace = true
		case c == '-' && i+1 < len(r) && r[i+1] == '-':
			for i < len(r) && r[i] != '\n' {
				i++
			}
			pendingSpace = true
		case c == '/' && i+1 < len(r) && r[i+1] == '*':
			i += 2
			for i+1 < len(r) && (r[i] != '*' || r[i+1] != '/') {
				i++
			}
			i++
			pendingSpace = true
		case c == '\'' || c == '"':
			i++
			for i < len(r) {
				if r[i] == '\\' {
					i += 2
					continue
				}
				if r[i] == c {
					if i+1 < len(r) && r[i+1] == c {
						i += 2
						continue
					}
					break
				}
				i++
			}
			write('?')
		case c == '$' && i+1 < len(r) && (r[i+1] == '$' || unicode.IsLetter(r[i+1]) || r[i+1] == '_'):
			// Dollar-quoted Postgres string: $tag$ ... $tag$.
			end := i + 1
			for end < len(r) && r[end] != '$' && isWord(r[end]) {
				end++
			}
			if end >= len(r) || r[end] != '$' {
				write(c)
				continue
			}
			tag := string(r[i : end+1])
			rest := string(r[end+1:])
			idx := strings.Index(rest, tag)
			if idx < 0 {
				i = len(r)
			} else {
				i = end + len([]rune(rest[:idx])) + len([]rune(tag))
			}
			write('?')
		case unicode.IsDigit(c):
			// Keep digits inside identifiers ("t1") and bind markers ("$1").
			if i > 0 && isWord(r[i-1]) {
				write(c)
				continue
			}
			for i+1 < len(r) && (unicode.IsDigit(r[i+1]) || r[i+1] == '.') {
				i++
			}
			write('?')
		default:
			write(c)
		}
	}
	return b.String()
}
