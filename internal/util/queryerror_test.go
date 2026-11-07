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
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestSQLFingerprintIgnoresValuesAndFormatting(t *testing.T) {
	base := SQLFingerprint("SELECT name FROM t1 WHERE phone = '13812345678' AND age > 30")
	same := []string{
		"select  name\n from t1 where phone = 'x''y' and age > 7",
		"/* note */ SELECT name FROM t1 WHERE phone = 'other' AND age > 1.5 -- trailing",
		"SELECT name FROM t1 WHERE phone = $$secret$$ AND age > 2",
		"SELECT name FROM t1 WHERE phone = $q$sec'ret$q$ AND age > 2",
	}
	for _, s := range same {
		if got := SQLFingerprint(s); got != base {
			t.Errorf("SQLFingerprint(%q) = %s, want %s (normalized %q)", s, got, base, normalizeSQL(s))
		}
	}
	different := []string{
		"SELECT name FROM t2 WHERE phone = 'x' AND age > 1",
		"SELECT email FROM t1 WHERE phone = 'x' AND age > 1",
	}
	for _, s := range different {
		if got := SQLFingerprint(s); got == base {
			t.Errorf("SQLFingerprint(%q) must differ from base", s)
		}
	}
}

func TestSQLFingerprintKeepsBindMarkers(t *testing.T) {
	if SQLFingerprint("SELECT $1, $2") == SQLFingerprint("SELECT $1") {
		t.Error("different bind markers must not collide")
	}
}

func TestNormalizeSQLHandlesUnterminatedInput(t *testing.T) {
	for _, s := range []string{"'", "'abc", "/* abc", "-- abc", "$$abc", "$a$b", "\\", "'\\"} {
		if got := normalizeSQL(s); strings.Contains(got, "abc") && s != "$a$b" {
			t.Errorf("normalizeSQL(%q) leaked literal: %q", s, got)
		}
	}
}

func TestQueryErrorHasNoCauseAndSerializes(t *testing.T) {
	e := &QueryError{Code: QueryTimeout, QueryID: "abc", Message: "m"}
	want := `{"errorCode":"QUERY_TIMEOUT","retryable":false,"queryId":"abc","message":"m"}`
	if e.Error() != want {
		t.Errorf("got %s, want %s", e.Error(), want)
	}
	if e.Category() != CategoryAgent {
		t.Errorf("category = %s", e.Category())
	}
	if e.Unwrap() != nil {
		t.Error("QueryError must not expose a cause")
	}
}

func TestErrorTypeIsLowCardinality(t *testing.T) {
	secret := errors.New("duplicate value 13812345678")
	tests := []struct {
		err  error
		want string
	}{
		{&QueryError{Code: QueryBusy}, QueryBusy},
		{fmt.Errorf("wrapped: %w", &QueryError{Code: QueryBusy}), QueryBusy},
		{NewAgentError("failed", secret), string(CategoryAgent)},
		{NewClientServerError("failed", 500, secret), string(CategoryServer)},
		{secret, "_OTHER"},
	}
	for _, tc := range tests {
		if got := ErrorType(tc.err); got != tc.want {
			t.Errorf("ErrorType(%v) = %s, want %s", tc.err, got, tc.want)
		}
	}
}
