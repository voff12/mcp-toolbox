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

package resultmask

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
)

func row(kv ...any) orderedmap.Row {
	var r orderedmap.Row
	for i := 0; i < len(kv); i += 2 {
		r.Add(kv[i].(string), kv[i+1])
	}
	return r
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"empty", Config{}, "at least one column"},
		{"no column", Config{Rules: []Rule{{Action: ActionRedact}}}, "without a column"},
		{"unknown action", Config{Rules: []Rule{{Column: "a", Action: "hash"}}}, "unknown action"},
		{"no action", Config{Rules: []Rule{{Column: "a"}}}, "unknown action"},
		{"duplicate rule", Config{Rules: []Rule{{Column: "a", Action: ActionRedact}, {Column: "a", Action: ActionEmail}}}, "more than one rule"},
		{"allow and rule", Config{Allow: []string{"a"}, Rules: []Rule{{Column: "a", Action: ActionRedact}}}, "both allow and rules"},
		{"duplicate allow", Config{Allow: []string{"a", "a"}}, "listed twice"},
		{"empty allow", Config{Allow: []string{""}}, "empty column"},
		{"keep nothing", Config{Rules: []Rule{{Column: "a", Action: ActionKeep}}}, "positive first or last"},
		{"negative keep", Config{Rules: []Rule{{Column: "a", Action: ActionKeep, First: 3, Last: -1}}}, "positive first or last"},
		{"first on redact", Config{Rules: []Rule{{Column: "a", Action: ActionRedact, First: 1}}}, "only apply"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(&tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if m, err := New(nil); m != nil || err != nil {
		t.Fatalf("New(nil) = %v, %v", m, err)
	}
}

func TestApplyMasksValues(t *testing.T) {
	phone := Rule{Column: "v", Action: ActionKeep, First: 3, Last: 4, Replacement: "****"}
	name := Rule{Column: "v", Action: ActionKeep, First: 1}
	email := Rule{Column: "v", Action: ActionEmail}
	redact := Rule{Column: "v", Action: ActionRedact}

	tests := []struct {
		name string
		rule Rule
		in   any
		want any
	}{
		{"phone", phone, "13812345678", "138****5678"},
		{"short phone reveals nothing", phone, "1234567", "****"},
		{"chinese name", name, "张三丰", "张***"},
		{"two char name", name, "张三", "张***"},
		{"single char name", name, "张", "***"},
		{"bytes", name, []byte("张三丰"), "张***"},
		{"last only", Rule{Column: "v", Action: ActionKeep, Last: 2}, "北京市朝阳区", "***阳区"},
		{"null keep", phone, nil, nil},
		{"null email", email, nil, nil},
		{"null redact", redact, nil, nil},
		{"empty keep", phone, "", ""},
		{"empty email", email, "", ""},
		{"email", email, "zhang.san@example.com", "z***@example.com"},
		{"chinese email", email, "张三@example.cn", "张***@example.cn"},
		{"one char local", email, "a@example.com", "***@example.com"},
		{"two at signs", email, "a@b@example.com", "a***@example.com"},
		{"not an email", email, "13812345678", "***"},
		{"leading at", email, "@example.com", "***"},
		{"trailing at", email, "secret@", "***"},
		{"redact string", redact, "secret", "***"},
		{"redact number", redact, int64(42), "***"},
		{"redact time", redact, time.Unix(0, 0), "***"},
		{"custom replacement", Rule{Column: "v", Action: ActionRedact, Replacement: "<hidden>"}, "x", "<hidden>"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, err := New(&Config{Rules: []Rule{tc.rule}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.Apply([]any{row("v", tc.in)})
			if err != nil {
				t.Fatal(err)
			}
			if v := got.([]any)[0].(orderedmap.Row).Columns[0].Value; !cmp.Equal(v, tc.want) {
				t.Fatalf("got %#v, want %#v", v, tc.want)
			}
		})
	}
}

func testMasker(t *testing.T) *Masker {
	t.Helper()
	m, err := New(&Config{
		Allow: []string{"id"},
		Rules: []Rule{{Column: "phone", Action: ActionKeep, First: 3, Last: 4, Replacement: "****"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestApplyKeepsOrderAndAllowedValues(t *testing.T) {
	in := []any{
		row("phone", "13812345678", "id", 1),
		row("phone", nil, "id", 2),
	}
	got, err := testMasker(t).Apply(in)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"phone":"138****5678","id":1},{"phone":null,"id":2}]`
	if string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
	// The input may be shared, for example with a cache.
	if v := in[0].(orderedmap.Row).Columns[0].Value; v != "13812345678" {
		t.Fatalf("input was modified: %v", v)
	}
}

func TestApplyMasksEveryColumnOfARepeatedName(t *testing.T) {
	got, err := testMasker(t).Apply([]any{row("id", 1, "phone", "13812345678", "phone", "13987654321")})
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(got); strings.Contains(string(b), "12345") || strings.Contains(string(b), "98765") {
		t.Fatalf("value leaked: %s", b)
	}
}

func TestApplyMaps(t *testing.T) {
	got, err := testMasker(t).Apply([]any{map[string]any{"id": 1, "phone": "13812345678"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{map[string]any{"id": 1, "phone": "138****5678"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
}

func TestApplyEmptyResults(t *testing.T) {
	m := testMasker(t)
	if got, err := m.Apply(nil); got != nil || err != nil {
		t.Fatalf("nil: got %v, %v", got, err)
	}
	got, err := m.Apply([]any{})
	if err != nil || len(got.([]any)) != 0 {
		t.Fatalf("empty: got %v, %v", got, err)
	}
}

func TestApplyFailsClosed(t *testing.T) {
	const secret = "13812345678"
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"unconfigured column", []any{row("id", 1, "phone", secret, "address", secret)}, `"address" has no masking rule`},
		{"unconfigured map column", []any{map[string]any{"id": 1, "phone": secret, "address": secret}}, `"address" has no masking rule`},
		{"missing allowed column", []any{row("phone", secret)}, `"id" is not in the result`},
		{"missing masked column", []any{row("id", 1)}, `"phone" is not in the result`},
		{"later row differs", []any{row("id", 1, "phone", secret), row("id", 2)}, `"phone" is not in the result`},
		{"string result", secret, "unsupported result type"},
		{"map result", map[string]any{"phone": secret}, "unsupported result type"},
		{"unknown row", []any{secret}, "unsupported row type"},
		{"nested row", []any{[]any{row("phone", secret)}}, "unsupported row type"},
		{"number with keep", []any{row("id", 1, "phone", 13812345678)}, "needs a string"},
		{"nested value with keep", []any{row("id", 1, "phone", map[string]any{"a": secret})}, "needs a string"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := testMasker(t).Apply(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want it to contain %q", err, tc.want)
			}
			if got != nil {
				t.Fatalf("result returned with error: %v", got)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error contains the value: %v", err)
			}
		})
	}
}
