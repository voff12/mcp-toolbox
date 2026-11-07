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

// Package mysqlexplain plans the statement of a configured mysql-sql tool and
// returns a structured summary. The plan text itself is never returned: it
// shows bound parameters as literals, so a filter on a phone number would echo
// the number. Summary fields are copied individually, so anything MySQL adds to
// its plan output in future stays out by default.
package mysqlexplain

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlsql"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const resourceType string = "mysql-explain"

func init() {
	if !tools.Register(resourceType, newConfig) {
		panic(fmt.Sprintf("tool type %q already registered", resourceType))
	}
}

func newConfig(ctx context.Context, name string, decoder *yaml.Decoder) (tools.ToolConfig, error) {
	actual := Config{ConfigBase: tools.ConfigBase{Name: name}}
	if err := decoder.DecodeContext(ctx, &actual); err != nil {
		return nil, err
	}
	return actual, nil
}

type compatibleSource interface {
	MySQLPool() *sql.DB
	RunSQL(context.Context, string, []any) (any, error)
}

type Config struct {
	tools.ConfigBase `yaml:",inline"`
	Type             string `yaml:"type" validate:"required"`
	// Tool names the mysql-sql tool to plan. Its source and parameters are used,
	// so the plan cannot drift from the statement agents actually run.
	Tool string `yaml:"tool" validate:"required"`

	target *mysqlsql.Config
}

var (
	_ tools.ToolConfig           = Config{}
	_ tools.ToolBinder           = Config{}
	_ tools.GovernedOutputConfig = Config{}
)

func (cfg Config) ToolConfigType() string {
	return resourceType
}

// GovernedOutput implements tools.GovernedOutputConfig.
func (cfg Config) GovernedOutput() {}

// BindTools implements tools.ToolBinder.
func (cfg Config) BindTools(all map[string]tools.ToolConfig) (tools.ToolConfig, error) {
	target, err := Target(all, cfg.Tool)
	if err != nil {
		return nil, err
	}
	cfg.target = target
	// A plan reveals schema, so by default it needs what the statement needs.
	if len(cfg.AuthRequired) == 0 {
		cfg.AuthRequired = target.AuthRequired
	}
	if len(cfg.ScopesRequired) == 0 {
		cfg.ScopesRequired = target.ScopesRequired
	}
	return cfg, nil
}

// Target resolves the mysql-sql tool a plan-based tool is bound to. Its
// statement must be fixed, or a plan would not describe what agents run.
func Target(all map[string]tools.ToolConfig, name string) (*mysqlsql.Config, error) {
	target, ok := all[name].(mysqlsql.Config)
	if !ok {
		return nil, fmt.Errorf("tool %q must be a configured mysql-sql tool", name)
	}
	if len(target.TemplateParameters) > 0 {
		return nil, fmt.Errorf("tool %q uses templateParameters, so its statement is not fixed", name)
	}
	return &target, nil
}

func (cfg Config) Initialize(context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		return nil, fmt.Errorf("description is required for tool %q", cfg.Name)
	}
	if cfg.target == nil {
		return nil, fmt.Errorf("tool %q is not bound to a mysql-sql tool", cfg.Name)
	}
	allParameters, paramManifest, err := parameters.ProcessParameters(nil, cfg.target.Parameters)
	if err != nil {
		return nil, err
	}
	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewReadOnlyAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: paramManifest, AuthRequired: cfg.AuthRequired},
			allParameters,
		),
	}, nil
}

var _ tools.Tool = Tool{}

type Tool struct {
	tools.BaseTool[Config]
}

func (t Tool) Invoke(ctx context.Context, s sources.Source, params parameters.ParamValues, accessToken tools.AccessToken) (any, util.ToolboxError) {
	source, ok := s.(compatibleSource)
	if !ok {
		return nil, util.NewClientServerError("source used is not compatible with the tool", http.StatusInternalServerError, nil)
	}
	newParams, err := parameters.GetParams(t.Cfg.target.Parameters, params.AsMap())
	if err != nil {
		return nil, util.NewAgentError("unable to extract standard params", err)
	}
	// The statement comes from server config and parameters stay bound. EXPLAIN
	// without ANALYZE plans the statement and does not run it.
	result, err := source.RunSQL(ctx, "EXPLAIN FORMAT=JSON "+t.Cfg.target.Statement, newParams.AsSlice())
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	summary, err := summaryFromRows(result)
	if err != nil {
		return nil, util.NewClientServerError(err.Error(), http.StatusInternalServerError, nil)
	}
	return summary, nil
}

func (t Tool) GetSourceName() string {
	return t.Cfg.target.Source
}

func (t Tool) ToConfig() tools.ToolConfig {
	return t.Cfg
}

func (t Tool) ValidateSource(source sources.Source) error {
	if _, ok := source.(compatibleSource); !ok {
		return fmt.Errorf("invalid source for %q tool: source %q is not a compatible type", t.Cfg.Type, t.Cfg.target.Source)
	}
	return nil
}

// Summary is everything returned about a plan.
type Summary struct {
	EstimatedCost  string        `json:"estimatedCost,omitempty"`
	UsingFilesort  bool          `json:"usingFilesort"`
	UsingTemporary bool          `json:"usingTemporary"`
	Tables         []TableAccess `json:"tables"`

	nodes []map[string]any
}

// TableAccess describes how one table is read. Filter conditions are omitted
// because they contain values; Columns lists what the query reads instead.
type TableAccess struct {
	Table               string   `json:"table"`
	AccessType          string   `json:"accessType,omitempty"`
	PossibleKeys        []string `json:"possibleKeys,omitempty"`
	Key                 string   `json:"key,omitempty"`
	UsedKeyParts        []string `json:"usedKeyParts,omitempty"`
	RowsExaminedPerScan int64    `json:"rowsExaminedPerScan"`
	RowsProducedPerJoin int64    `json:"rowsProducedPerJoin"`
	FilteredPercent     string   `json:"filteredPercent,omitempty"`
	Columns             []string `json:"columns,omitempty"`
}

func summaryFromRows(result any) (Summary, error) {
	plan, err := PlanText(result)
	if err != nil {
		return Summary{}, err
	}
	return summarize(plan)
}

// PlanText extracts the plan from the rows of an EXPLAIN FORMAT=JSON statement.
// The text can contain parameter values and must not reach the caller.
func PlanText(result any) (string, error) {
	rows, ok := result.([]any)
	if !ok || len(rows) == 0 {
		return "", fmt.Errorf("no query plan returned")
	}
	row, ok := rows[0].(orderedmap.Row)
	if !ok || len(row.Columns) == 0 {
		return "", fmt.Errorf("no query plan returned in row")
	}
	plan, ok := row.Columns[0].Value.(string)
	if !ok {
		return "", fmt.Errorf("unexpected query plan type")
	}
	return plan, nil
}

// TableNodes returns the raw node of each table in a plan. Unlike Summary,
// nodes include conditions, so they can contain parameter values.
func TableNodes(plan string) ([]map[string]any, error) {
	block, err := parseBlock(plan)
	if err != nil {
		return nil, err
	}
	s := Summary{}
	s.walk(block)
	return s.nodes, nil
}

func parseBlock(plan string) (map[string]any, error) {
	var root map[string]any
	if err := json.Unmarshal([]byte(plan), &root); err != nil {
		return nil, fmt.Errorf("unable to parse query plan")
	}
	block, ok := root["query_block"].(map[string]any)
	if !ok {
		// Version 2, selected by explain_json_format_version, has another shape.
		return nil, fmt.Errorf("unsupported query plan format")
	}
	return block, nil
}

// summarize reads format version 1 of EXPLAIN FORMAT=JSON. Errors must not
// include the plan.
func summarize(plan string) (Summary, error) {
	block, err := parseBlock(plan)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{Tables: []TableAccess{}}
	if cost, ok := block["cost_info"].(map[string]any); ok {
		s.EstimatedCost = str(cost["query_cost"])
	}
	s.walk(block)
	return s, nil
}

// walk finds tables wherever the plan nests them: joins, ordering, grouping,
// subqueries and unions. Keys are sorted to keep the output stable.
func (s *Summary) walk(node any) {
	switch n := node.(type) {
	case []any:
		for _, item := range n {
			s.walk(item)
		}
	case map[string]any:
		if b, _ := n["using_filesort"].(bool); b {
			s.UsingFilesort = true
		}
		if b, _ := n["using_temporary_table"].(bool); b {
			s.UsingTemporary = true
		}
		if t, ok := n["table"].(map[string]any); ok {
			s.Tables = append(s.Tables, tableAccess(t))
			s.nodes = append(s.nodes, t)
		}
		for _, k := range slices.Sorted(maps.Keys(n)) {
			s.walk(n[k])
		}
	}
}

func tableAccess(t map[string]any) TableAccess {
	return TableAccess{
		Table:               str(t["table_name"]),
		AccessType:          str(t["access_type"]),
		PossibleKeys:        strs(t["possible_keys"]),
		Key:                 str(t["key"]),
		UsedKeyParts:        strs(t["used_key_parts"]),
		RowsExaminedPerScan: num(t["rows_examined_per_scan"]),
		RowsProducedPerJoin: num(t["rows_produced_per_join"]),
		FilteredPercent:     str(t["filtered"]),
		Columns:             strs(t["used_columns"]),
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func num(v any) int64 {
	f, _ := v.(float64)
	return int64(f)
}

func strs(v any) []string {
	items, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
