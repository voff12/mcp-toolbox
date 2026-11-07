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

// Package mysqlslowqueryreport ranks statements from two views, because neither
// is complete. Toolbox measures its own tools. The server's digests cover every
// client, but on MySQL 8.4 they do not count statements run through the binary
// protocol, which is how any tool with bound parameters runs.
//
// Digests have literals replaced with placeholders. QUERY_SAMPLE_TEXT, which
// keeps them, is never selected.
package mysqlslowqueryreport

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/queryguard"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const resourceType string = "mysql-slow-query-report"

// Timers are in picoseconds. Findings are computed here rather than by the
// caller, so they are based on exact values and not on rounded ones.
const reportStatement = `
SELECT
    DIGEST AS digest,
    SCHEMA_NAME AS db,
    DIGEST_TEXT AS query,
    COUNT_STAR AS calls,
    ROUND(SUM_TIMER_WAIT / 1000000000, 2) AS total_ms,
    ROUND(AVG_TIMER_WAIT / 1000000000, 2) AS avg_ms,
    ROUND(QUANTILE_95 / 1000000000, 2) AS p95_ms,
    ROUND(MAX_TIMER_WAIT / 1000000000, 2) AS max_ms,
    SUM_ERRORS AS errors,
    SUM_ROWS_SENT AS rows_sent,
    SUM_ROWS_EXAMINED AS rows_examined,
    SUM_CREATED_TMP_DISK_TABLES AS tmp_disk_tables,
    SUM_SORT_MERGE_PASSES AS sort_merge_passes,
    FIRST_SEEN AS first_seen,
    LAST_SEEN AS last_seen,
    CONCAT_WS(',',
        IF(SUM_NO_INDEX_USED > 0, 'full_scan', NULL),
        IF(SUM_NO_GOOD_INDEX_USED > 0, 'no_good_index', NULL),
        IF(SUM_ROWS_EXAMINED > 100 * GREATEST(SUM_ROWS_SENT, 1), 'low_row_efficiency', NULL),
        IF(SUM_CREATED_TMP_DISK_TABLES > 0, 'disk_temp_table', NULL),
        IF(SUM_SORT_MERGE_PASSES > 0, 'disk_sort', NULL),
        IF(SUM_ERRORS > 0, 'errors', NULL)
    ) AS findings
FROM performance_schema.events_statements_summary_by_digest
WHERE DIGEST IS NOT NULL
  AND SCHEMA_NAME NOT IN ('information_schema', 'performance_schema', 'mysql', 'sys')
  AND (? = '' OR SCHEMA_NAME = ?)
ORDER BY SUM_TIMER_WAIT DESC
LIMIT ?`

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
	PerformanceSchemaEnabled(context.Context) (bool, error)
}

type Config struct {
	tools.ConfigBase `yaml:",inline"`
	Type             string `yaml:"type" validate:"required"`
	Source           string `yaml:"source" validate:"required"`
}

var _ tools.ToolConfig = Config{}

func (cfg Config) ToolConfigType() string {
	return resourceType
}

func (cfg Config) Initialize(context.Context) (tools.Tool, error) {
	if cfg.Description == "" {
		cfg.Description = "Ranks statements by total time: what Toolbox measured for its own tools, and what the server recorded for all clients, with findings such as full scans and disk sorts."
	}
	allParameters := parameters.Parameters{
		parameters.NewStringParameter("database", "Optional: only report statements run against this database.", parameters.WithStringDefault("")),
		parameters.NewIntParameter("limit", "Optional: the maximum number of statements to return. Defaults to 20.", parameters.WithIntDefault(20)),
	}
	return Tool{
		BaseTool: tools.NewBaseTool(
			cfg,
			tools.GetAnnotationsOrDefault(cfg.Annotations, tools.NewReadOnlyAnnotations),
			tools.Manifest{Description: cfg.Description, Parameters: allParameters.Manifest(), AuthRequired: cfg.AuthRequired},
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
	enabled, err := source.PerformanceSchemaEnabled(ctx)
	if err != nil {
		return nil, util.NewClientServerError("failed to check performance_schema", http.StatusInternalServerError, err)
	}
	if !enabled {
		return nil, util.NewClientServerError("enable performance_schema to run this tool", http.StatusInternalServerError, nil)
	}
	paramsMap := params.AsMap()
	database, ok := paramsMap["database"].(string)
	if !ok {
		return nil, util.NewAgentError("invalid 'database' parameter; expected a string", nil)
	}
	limit, ok := paramsMap["limit"].(int)
	if !ok || limit <= 0 {
		return nil, util.NewAgentError("invalid 'limit' parameter; expected a positive integer", nil)
	}
	resp, err := source.RunSQL(ctx, reportStatement, []any{database, database, limit})
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	rows, ok := resp.([]any)
	if !ok {
		return nil, util.NewClientServerError("unexpected result type", http.StatusInternalServerError, nil)
	}
	report := Report{Toolbox: []queryguard.ToolStat{}, Server: rows, Notes: []string{serverNote}}
	if p, ok := s.(queryguard.Provider); ok && p.GetQueryGuard() != nil {
		stats, dropped := p.GetQueryGuard().Stats()
		report.Toolbox = stats[:min(limit, len(stats))]
		report.Notes = append(report.Notes, toolboxNote)
		if dropped > 0 {
			report.Notes = append(report.Notes, fmt.Sprintf("%d calls were not counted because the statistics table is full.", dropped))
		}
	} else {
		report.Notes = append(report.Notes, "Add queryGuard to the source to measure Toolbox's own tools.")
	}
	return report, nil
}

const (
	serverNote  = "server lists digests from all clients. It does not count statements with bound parameters, so most Toolbox tools are missing from it."
	toolboxNote = "toolbox is measured by this Toolbox instance since the source was last loaded. Other instances are not included."
)

// Report is the output of the tool.
type Report struct {
	Toolbox []queryguard.ToolStat `json:"toolbox"`
	Server  []any                 `json:"server"`
	Notes   []string              `json:"notes"`
}

func (t Tool) GetSourceName() string {
	return t.Cfg.Source
}

func (t Tool) ToConfig() tools.ToolConfig {
	return t.Cfg
}

func (t Tool) ValidateSource(source sources.Source) error {
	if _, ok := source.(compatibleSource); !ok {
		return fmt.Errorf("invalid source for %q tool: source %q is not a compatible type", t.Cfg.Type, t.Cfg.Source)
	}
	return nil
}
