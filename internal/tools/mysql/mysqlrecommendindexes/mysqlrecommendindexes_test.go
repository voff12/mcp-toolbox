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

package mysqlrecommendindexes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlsql"
	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

func cols(names ...string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, n := range names {
		out[n] = map[string]any{"data_type": "varchar"}
	}
	return out
}

func TestFilterColumns(t *testing.T) {
	tests := []struct {
		name      string
		condition string
		want      []string
	}{
		{"equality", "(`qg`.`c`.`level` = 'gold')", []string{"level"}},
		{"range", "(`qg`.`c`.`age` > 5)", []string{"age"}},
		{"equality goes first", "((`qg`.`c`.`age` > 5) and (`qg`.`c`.`level` = 'gold'))", []string{"level", "age"}},
		{"prefix like", "(`qg`.`c`.`level` like 'go%')", []string{"level"}},
		{"leading wildcard", "(`qg`.`c`.`level` like '%old')", nil},
		{"like on a function", "(`qg`.`c`.`level` like concat('%','x'))", nil},
		{"function of a column", "(lower(`qg`.`c`.`level`) = 'gold')", nil},
		{"other table", "(`qg`.`o`.`level` = 'gold')", nil},
		{"unknown column", "(`qg`.`c`.`nope` = 1)", nil},
		{"repeated", "((`qg`.`c`.`level` = 'a') or (`qg`.`c`.`level` = 'b'))", []string{"level"}},
		{"at most three", "((`qg`.`c`.`a` = 1) and (`qg`.`c`.`b` = 1) and (`qg`.`c`.`age` = 1) and (`qg`.`c`.`level` = 1))", []string{"a", "b", "age"}},
		{"identifier inside a literal", "(`qg`.`c`.`age` = '`qg`.`c`.`level` = 1')", []string{"age"}},
		{"escaped quotes", `(` + "`qg`.`c`.`age`" + ` = 'it''s \' ` + "`qg`.`c`.`level`" + `')`, []string{"age"}},
	}
	columns := cols("level", "age", "a", "b")
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, filterColumns(tc.condition, "c", columns)); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestFilterColumnsSkipsTypesNeedingAPrefix(t *testing.T) {
	columns := cols("level")
	columns["notes"] = map[string]any{"data_type": "text"}
	got := filterColumns("((`qg`.`c`.`notes` = 'x') and (`qg`.`c`.`level` = 'y'))", "c", columns)
	if diff := cmp.Diff([]string{"level"}, got); diff != "" {
		t.Fatal(diff)
	}
}

// A value shaped like a real column reference inside a condition must not make
// its way out, even when the condition is unbalanced enough to fool the parser.
func TestFilterColumnsNeverReturnsValues(t *testing.T) {
	const secret = "13812345678"
	for _, condition := range []string{
		"(`qg`.`c`.`level` = '" + secret + "')",
		"(`qg`.`c`.`" + secret + "` = 1)",
		"(`qg`.`c`.`level` = 'unterminated `qg`.`c`.`" + secret + "` = 1)",
	} {
		for _, name := range filterColumns(condition, "c", cols("level")) {
			if strings.Contains(name, secret) {
				t.Fatalf("value returned from %q", condition)
			}
		}
	}
}

func TestDigestable(t *testing.T) {
	got := digestable("SELECT a FROM t WHERE a = ? AND b = 'why?' AND c IN (?, ?)")
	if want := "SELECT a FROM t WHERE a = 0 AND b = 'why?' AND c IN (0, 0)"; got != want {
		t.Fatalf("got %s", got)
	}
}

func TestTableAliases(t *testing.T) {
	got := tableAliases("SELECT * FROM customers c JOIN `orders` AS o ON o.a = c.b LEFT JOIN items WHERE x = 1")
	want := map[string]string{"c": "customers", "o": "orders"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
}

func TestIndexDDL(t *testing.T) {
	add, drop := indexDDL("orders", []string{"customer_id", "status"})
	if want := "ALTER TABLE `orders` ADD INDEX `idx_orders_customer_id_status` (`customer_id`, `status`), ALGORITHM=INPLACE, LOCK=NONE;"; add != want {
		t.Errorf("add = %s", add)
	}
	if want := "ALTER TABLE `orders` DROP INDEX `idx_orders_customer_id_status`, ALGORITHM=INPLACE, LOCK=NONE;"; drop != want {
		t.Errorf("drop = %s", drop)
	}
}

func TestIndexDDLQuotesAndTruncates(t *testing.T) {
	add, _ := indexDDL(strings.Repeat("t", 70), []string{"a`b"})
	if !strings.Contains(add, "(`a``b`)") {
		t.Errorf("column not quoted: %s", add)
	}
	if strings.Contains(add, strings.Repeat("t", 65)+"`  ") {
		t.Errorf("name too long: %s", add)
	}
}

// fakeSource answers by statement and records all of them.
type fakeSource struct {
	sources.Source
	plan    string
	rows    map[string][]any
	failing string
	seen    []string
}

func (f *fakeSource) MySQLPool() *sql.DB { return nil }

func (f *fakeSource) RunSQL(_ context.Context, statement string, _ []any) (any, error) {
	f.seen = append(f.seen, statement)
	if f.failing != "" && strings.Contains(statement, f.failing) {
		return nil, errors.New("denied")
	}
	if strings.HasPrefix(statement, "EXPLAIN") {
		var r orderedmap.Row
		r.Add("EXPLAIN", f.plan)
		return []any{r}, nil
	}
	for key, rows := range f.rows {
		if strings.Contains(statement, key) {
			return rows, nil
		}
	}
	return []any{}, nil
}

func row(kv ...any) orderedmap.Row {
	var r orderedmap.Row
	for i := 0; i < len(kv); i += 2 {
		r.Add(kv[i].(string), kv[i+1])
	}
	return r
}

func planWith(node string) string {
	return fmt.Sprintf(`{"query_block":{"select_id":1,"table":%s}}`, node)
}

const scan = `{"table_name":"c","access_type":"ALL","rows_examined_per_scan":50000,"attached_condition":"(` + "`qg`.`c`.`phone`" + ` = '13812345678')"}`

func baseRows() map[string][]any {
	return map[string][]any{
		"TABLES WHERE":     {row("table_rows", int64(50000))},
		"COLUMN_NAME":      {row("name", "phone", "data_type", "varchar", "octets", int64(80))},
		"COUNT(DISTINCT":   {row("indexes", int64(2))},
		"STATEMENT_DIGEST": {row("digest", "abc123")},
		"COUNT_STAR":       {row("calls", int64(7), "total_ms", "12.50", "p95_ms", "3.00", "rows_examined", int64(350000))},
	}
}

func newReportFor(t *testing.T, f *fakeSource) Report {
	t.Helper()
	tool := Tool{BaseTool: tools.NewBaseTool(
		Config{Tool: "q", target: &mysqlsql.Config{Statement: "SELECT id FROM customers c WHERE phone = ?"}},
		nil, tools.Manifest{}, nil,
	)}
	res, err := tool.Invoke(context.Background(), f, parameters.ParamValues{}, "")
	if err != nil {
		t.Fatal(err)
	}
	return res.(Report)
}

func TestRecommendsIndexWithEvidence(t *testing.T) {
	f := &fakeSource{plan: planWith(scan), rows: baseRows()}
	got := newReportFor(t, f)
	want := Report{
		Digest:  "abc123",
		Runtime: &Runtime{MeasuredBy: "server", Calls: 7, TotalMs: "12.50", P95Ms: "3.00", RowsExamined: 350000},
		Recommendations: []Recommendation{{
			Table: "customers", AccessType: "ALL", TableRows: 50000, RowsExaminedPerScan: 50000,
			ExistingIndexes: 2, Columns: []string{"phone"}, EstimatedIndexBytes: 50000 * 100,
			SuggestedDDL: "ALTER TABLE `customers` ADD INDEX `idx_customers_phone` (`phone`), ALGORITHM=INPLACE, LOCK=NONE;",
			RollbackDDL:  "ALTER TABLE `customers` DROP INDEX `idx_customers_phone`, ALGORITHM=INPLACE, LOCK=NONE;",
			Status:       statusNeedsApproval,
		}},
		Notes:      []string{},
		Disclaimer: disclaimer,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "13812345678") {
		t.Fatalf("value leaked: %s", b)
	}
}

func TestNeverExecutesDDL(t *testing.T) {
	f := &fakeSource{plan: planWith(scan), rows: baseRows()}
	newReportFor(t, f)
	for _, s := range f.seen {
		if !strings.HasPrefix(s, "EXPLAIN") && !strings.HasPrefix(s, "SELECT") {
			t.Fatalf("unexpected statement: %s", s)
		}
	}
}

func TestNoRecommendation(t *testing.T) {
	smallRows := baseRows()
	smallRows["TABLES WHERE"] = []any{row("table_rows", int64(10))}
	unresolved := baseRows()
	delete(unresolved, "TABLES WHERE")
	tests := []struct {
		name string
		node string
		rows map[string][]any
		note string
	}{
		{"already indexed", `{"table_name":"c","access_type":"ref","key":"k"}`, baseRows(), ""},
		{"small table", `{"table_name":"c","access_type":"ALL","rows_examined_per_scan":10,"attached_condition":"x"}`, smallRows, "too few"},
		{"unresolved", `{"table_name":"<derived2>","access_type":"ALL"}`, unresolved, "could not be resolved"},
		{"usable index ignored", `{"table_name":"c","access_type":"ALL","possible_keys":["k"],"rows_examined_per_scan":50000}`, baseRows(), "ANALYZE TABLE"},
		{"no filter", `{"table_name":"c","access_type":"ALL","rows_examined_per_scan":50000}`, baseRows(), "no filter"},
		{"unusable filter", `{"table_name":"c","access_type":"ALL","rows_examined_per_scan":50000,"attached_condition":"(lower(` + "`qg`.`c`.`phone`" + `) = 'x')"}`, baseRows(), "cannot serve"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newReportFor(t, &fakeSource{plan: planWith(tc.node), rows: tc.rows})
			if len(got.Recommendations) != 0 {
				t.Fatalf("got %+v", got.Recommendations)
			}
			if tc.note != "" && !strings.Contains(strings.Join(got.Notes, " "), tc.note) {
				t.Fatalf("notes = %v", got.Notes)
			}
		})
	}
}

func TestRuntimeIsOptional(t *testing.T) {
	got := newReportFor(t, &fakeSource{plan: planWith(scan), rows: baseRows(), failing: "performance_schema"})
	if got.Runtime != nil || len(got.Recommendations) != 1 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(strings.Join(got.Notes, " "), "unavailable") {
		t.Fatalf("notes = %v", got.Notes)
	}
}

func TestRequiredLookupsFail(t *testing.T) {
	f := &fakeSource{plan: planWith(scan), rows: baseRows(), failing: "COLUMN_NAME"}
	tool := Tool{BaseTool: tools.NewBaseTool(
		Config{Tool: "q", target: &mysqlsql.Config{Statement: "SELECT id FROM customers c"}},
		nil, tools.Manifest{}, nil,
	)}
	if _, err := tool.Invoke(context.Background(), f, parameters.ParamValues{}, ""); err == nil {
		t.Fatal("want error")
	}
}
