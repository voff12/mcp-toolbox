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

package mysqlexplain

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlsql"
	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// The fixtures in testdata are real MySQL 8.4 output.
func plan(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSummarizeJoin(t *testing.T) {
	got, err := summarize(plan(t, "join_order"))
	if err != nil {
		t.Fatal(err)
	}
	want := Summary{
		EstimatedCost:  "1.03",
		UsingFilesort:  true,
		UsingTemporary: true,
		Tables: []TableAccess{
			{
				Table: "c", AccessType: "ref", PossibleKeys: []string{"PRIMARY", "idx_phone"}, Key: "idx_phone",
				UsedKeyParts: []string{"phone"}, RowsExaminedPerScan: 1, RowsProducedPerJoin: 1,
				FilteredPercent: "100.00", Columns: []string{"id", "phone"},
			},
			{
				Table: "o", AccessType: "ref", PossibleKeys: []string{"idx_c"}, Key: "idx_c",
				UsedKeyParts: []string{"customer_id"}, RowsExaminedPerScan: 1,
				FilteredPercent: "33.33", Columns: []string{"id", "customer_id", "total"},
			},
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatal(diff)
	}
}

func TestSummaryNeverContainsFilterValues(t *testing.T) {
	const secret = "13812345678"
	raw := plan(t, "group_literal")
	if !strings.Contains(raw, secret) {
		t.Fatal("the fixture must contain the literal for this test to mean anything")
	}
	got, err := summarize(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("value leaked: %s", b)
	}
	if len(got.Tables) != 1 || got.Tables[0].AccessType != "index" {
		t.Fatalf("unexpected summary: %s", b)
	}
}

func TestSummarizeFindsNestedTables(t *testing.T) {
	got, err := summarize(plan(t, "union_subquery"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tbl := range got.Tables {
		names = append(names, tbl.Table)
	}
	if !strings.Contains(strings.Join(names, ","), "customers") || !strings.Contains(strings.Join(names, ","), "orders") {
		t.Fatalf("tables = %v", names)
	}
	if !got.UsingTemporary {
		t.Error("union temporary table not reported")
	}
}

func TestSummarizePlanWithoutTables(t *testing.T) {
	got, err := summarize(plan(t, "no_table"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	if want := `{"usingFilesort":false,"usingTemporary":false,"tables":[]}`; string(b) != want {
		t.Fatalf("got %s, want %s", b, want)
	}
}

func TestSummarizeRejectsUnknownPlans(t *testing.T) {
	for name, in := range map[string]string{
		"not json":  "secret value",
		"version 2": `{"query":"select 'secret'","operation":"Table scan"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := summarize(in)
			if err == nil {
				t.Fatal("want error")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("error contains plan text: %v", err)
			}
		})
	}
}

func TestSummaryFromRowsRejectsOtherResults(t *testing.T) {
	var numeric orderedmap.Row
	numeric.Add("EXPLAIN", 1)
	for name, in := range map[string]any{
		"nil":       nil,
		"empty":     []any{},
		"map row":   []any{map[string]any{"EXPLAIN": "{}"}},
		"empty row": []any{orderedmap.Row{}},
		"number":    []any{numeric},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := summaryFromRows(in); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func target() mysqlsql.Config {
	return mysqlsql.Config{
		ConfigBase: tools.ConfigBase{Name: "query", AuthRequired: []string{"google"}, ScopesRequired: []string{"read"}},
		Type:       "mysql-sql",
		Source:     "db",
		Statement:  "SELECT id FROM t WHERE a = ?",
		Parameters: parameters.Parameters{parameters.NewStringParameter("a", "a value")},
	}
}

func TestBindTools(t *testing.T) {
	cfg := Config{ConfigBase: tools.ConfigBase{Name: "explain", Description: "d"}, Type: resourceType, Tool: "query"}
	bound, err := cfg.BindTools(map[string]tools.ToolConfig{"query": target()})
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bound.Initialize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := tool.GetSourceName(); got != "db" {
		t.Errorf("source = %q", got)
	}
	if got := tool.GetAuthRequired(); !cmp.Equal(got, []string{"google"}) {
		t.Errorf("auth must be inherited, got %v", got)
	}
	if got := tool.GetScopesRequired(); !cmp.Equal(got, []string{"read"}) {
		t.Errorf("scopes must be inherited, got %v", got)
	}
	m, err := tool.Manifest(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Parameters) != 1 || m.Parameters[0].Name != "a" {
		t.Errorf("parameters = %+v", m.Parameters)
	}
	if !*tool.GetAnnotations(nil).ReadOnlyHint {
		t.Error("tool must be annotated read-only")
	}
}

func TestBindToolsKeepsExplicitAuth(t *testing.T) {
	cfg := Config{ConfigBase: tools.ConfigBase{Name: "explain", AuthRequired: []string{"dba"}}, Tool: "query"}
	bound, err := cfg.BindTools(map[string]tools.ToolConfig{"query": target()})
	if err != nil {
		t.Fatal(err)
	}
	if got := bound.(Config).AuthRequired; !cmp.Equal(got, []string{"dba"}) {
		t.Errorf("auth = %v", got)
	}
}

type otherConfig struct{ tools.ToolConfig }

func TestBindToolsRejects(t *testing.T) {
	templated := target()
	templated.TemplateParameters = parameters.Parameters{parameters.NewStringParameter("t", "table")}
	tests := []struct {
		name string
		all  map[string]tools.ToolConfig
		want string
	}{
		{"missing", map[string]tools.ToolConfig{}, "must be a configured mysql-sql tool"},
		{"other type", map[string]tools.ToolConfig{"query": otherConfig{}}, "must be a configured mysql-sql tool"},
		{"templated", map[string]tools.ToolConfig{"query": templated}, "templateParameters"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Config{Tool: "query"}.BindTools(tc.all)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestInitializeRequiresBinding(t *testing.T) {
	cfg := Config{ConfigBase: tools.ConfigBase{Name: "explain", Description: "d"}, Tool: "query"}
	if _, err := cfg.Initialize(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
