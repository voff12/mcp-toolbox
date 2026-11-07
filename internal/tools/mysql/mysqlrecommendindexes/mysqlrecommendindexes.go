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

// Package mysqlrecommendindexes turns the plan of a configured mysql-sql tool
// into index suggestions for a DBA. It only ever reads: it has no way to run the
// DDL it prints, and it must stay that way, because an index that looks right
// for one query can cost more in writes and storage than it saves.
//
// Plans contain parameter values, so nothing from a plan's text is returned.
// Column names found in a condition are only used if they exist on the table,
// which leaves nothing that could carry a value, whatever the parser got wrong.
package mysqlrecommendindexes

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	yaml "github.com/goccy/go-yaml"
	"github.com/googleapis/mcp-toolbox/internal/queryguard"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlexplain"
	"github.com/googleapis/mcp-toolbox/internal/tools/mysql/mysqlsql"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

const (
	resourceType string = "mysql-recommend-indexes"

	statusNeedsApproval = "requires_dba_approval"
	disclaimer          = "Suggestions are hypotheses from one plan. Test on a production-sized copy, compare query time and write load, and apply through a reviewed migration."

	// minTableRows is below the size where a scan is usually a problem.
	minTableRows = 1000
	maxIndexCols = 3
	maxIdentLen  = 64
	// indexEntryOverhead approximates a row's primary key and page overhead.
	indexEntryOverhead = 20
	defaultColumnBytes = 8
)

const (
	tableStatsQuery = `SELECT TABLE_ROWS AS table_rows FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`
	columnsQuery    = `SELECT COLUMN_NAME AS name, DATA_TYPE AS data_type, CHARACTER_OCTET_LENGTH AS octets FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`
	indexCountQuery = `SELECT COUNT(DISTINCT INDEX_NAME) AS indexes FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`
	digestQuery     = `SELECT STATEMENT_DIGEST(?) AS digest`
	runtimeQuery    = `SELECT COUNT_STAR AS calls, ROUND(SUM_TIMER_WAIT / 1000000000, 2) AS total_ms, ROUND(QUANTILE_95 / 1000000000, 2) AS p95_ms, SUM_ROWS_EXAMINED AS rows_examined FROM performance_schema.events_statements_summary_by_digest WHERE DIGEST = ? AND SCHEMA_NAME <=> DATABASE()`
)

var (
	// A literal is either `'...'` or `"..."`, with doubled or escaped quotes.
	literal = regexp.MustCompile(`'(?:[^'\\]|\\.|'')*'|"(?:[^"\\]|\\.|"")*"`)
	ident   = regexp.MustCompile("`[^`]+`(?:\\.`[^`]+`)*")
	// funcCall matches text ending in a function's opening parenthesis. An index
	// on a column cannot serve a filter on a function of it.
	funcCall  = regexp.MustCompile(`[a-zA-Z_]\($`)
	fromTable = regexp.MustCompile("(?i)\\b(?:FROM|JOIN)\\s+`?([a-zA-Z0-9_$]+)`?(?:\\s+(?:AS\\s+)?`?([a-zA-Z0-9_$]+)`?)?")
	// clauseWord keeps a following keyword from being read as an alias.
	clauseWord = regexp.MustCompile(`(?i)^(where|join|inner|left|right|cross|straight_join|on|using|group|order|having|limit|union|for|lock|natural)$`)
)

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
	// Tool names the mysql-sql tool to analyze.
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
	target, err := mysqlexplain.Target(all, cfg.Tool)
	if err != nil {
		return nil, err
	}
	cfg.target = target
	if len(cfg.AuthRequired) == 0 {
		cfg.AuthRequired = target.AuthRequired
	}
	if len(cfg.ScopesRequired) == 0 {
		cfg.ScopesRequired = target.ScopesRequired
	}
	return cfg, nil
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

// Report is the output of the tool.
type Report struct {
	Digest          string           `json:"digest,omitempty"`
	Runtime         *Runtime         `json:"runtime,omitempty"`
	Recommendations []Recommendation `json:"recommendations"`
	Notes           []string         `json:"notes"`
	Disclaimer      string           `json:"disclaimer"`
}

// Runtime is what was measured for the statement. MeasuredBy is "toolbox" for
// calls of the analyzed tool, or "server" for digests, which cover all clients
// but not statements with bound parameters.
type Runtime struct {
	MeasuredBy   string `json:"measuredBy"`
	Calls        int64  `json:"calls"`
	TotalMs      string `json:"totalMs"`
	P95Ms        string `json:"p95Ms"`
	Timeouts     int64  `json:"timeouts,omitempty"`
	RowsExamined int64  `json:"rowsExamined,omitempty"`
}

// Recommendation is one suggested index and the evidence for it.
type Recommendation struct {
	Table               string   `json:"table"`
	AccessType          string   `json:"accessType"`
	TableRows           int64    `json:"tableRows"`
	RowsExaminedPerScan int64    `json:"rowsExaminedPerScan"`
	ExistingIndexes     int64    `json:"existingIndexes"`
	Columns             []string `json:"columns"`
	EstimatedIndexBytes int64    `json:"estimatedIndexBytes"`
	SuggestedDDL        string   `json:"suggestedDdl"`
	RollbackDDL         string   `json:"rollbackDdl"`
	Status              string   `json:"status"`
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
	statement := t.Cfg.target.Statement
	result, err := source.RunSQL(ctx, "EXPLAIN FORMAT=JSON "+statement, newParams.AsSlice())
	if err != nil {
		return nil, util.ProcessGeneralError(err)
	}
	plan, err := mysqlexplain.PlanText(result)
	if err != nil {
		return nil, util.NewClientServerError(err.Error(), http.StatusInternalServerError, nil)
	}
	nodes, err := mysqlexplain.TableNodes(plan)
	if err != nil {
		return nil, util.NewClientServerError(err.Error(), http.StatusInternalServerError, nil)
	}

	report := Report{Recommendations: []Recommendation{}, Notes: []string{}, Disclaimer: disclaimer}
	aliases := tableAliases(statement)
	for _, node := range nodes {
		if err := report.assess(ctx, source, node, aliases); err != nil {
			return nil, util.ProcessGeneralError(err)
		}
	}
	report.addRuntime(ctx, source, s, t.Cfg.Tool, statement)
	return report, nil
}

// assess adds either a recommendation or a note saying why there is none.
func (r *Report) assess(ctx context.Context, source compatibleSource, node map[string]any, aliases map[string]string) error {
	alias, _ := node["table_name"].(string)
	access, _ := node["access_type"].(string)
	if access != "ALL" && access != "index" {
		return nil
	}
	table := alias
	if real, ok := aliases[alias]; ok {
		table = real
	}

	rows, err := queryRows(ctx, source, tableStatsQuery, table)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		r.note("Table %q is a derived table or could not be resolved, so it was skipped.", alias)
		return nil
	}
	examined := toInt(node["rows_examined_per_scan"])
	tableRows := max(toInt(rows[0]["table_rows"]), examined)
	if tableRows < minTableRows {
		r.note("Table %q has about %d rows, too few for a scan to be a problem.", table, tableRows)
		return nil
	}
	if keys, _ := node["possible_keys"].([]any); len(keys) > 0 {
		r.note("Table %q has a usable index that the optimizer did not choose. Check statistics with ANALYZE TABLE before adding another.", table)
		return nil
	}
	condition, _ := node["attached_condition"].(string)
	if condition == "" {
		r.note("Table %q is scanned with no filter on it, so an index would not help. Consider adding one to the statement.", table)
		return nil
	}

	columnRows, err := queryRows(ctx, source, columnsQuery, table)
	if err != nil {
		return err
	}
	columns := make(map[string]map[string]any, len(columnRows))
	for _, c := range columnRows {
		columns[toString(c["name"])] = c
	}
	candidates := filterColumns(condition, alias, columns)
	if len(candidates) == 0 {
		r.note("Table %q is filtered in a way an index cannot serve, such as a function or a leading wildcard. Consider rewriting the filter.", table)
		return nil
	}

	counts, err := queryRows(ctx, source, indexCountQuery, table)
	if err != nil {
		return err
	}
	var existing int64
	if len(counts) > 0 {
		existing = toInt(counts[0]["indexes"])
	}
	entryBytes := int64(indexEntryOverhead)
	for _, c := range candidates {
		size := toInt(columns[c]["octets"])
		if size == 0 {
			size = defaultColumnBytes
		}
		entryBytes += size
	}
	add, drop := indexDDL(table, candidates)
	r.Recommendations = append(r.Recommendations, Recommendation{
		Table:               table,
		AccessType:          access,
		TableRows:           tableRows,
		RowsExaminedPerScan: examined,
		ExistingIndexes:     existing,
		Columns:             candidates,
		EstimatedIndexBytes: tableRows * entryBytes,
		SuggestedDDL:        add,
		RollbackDDL:         drop,
		Status:              statusNeedsApproval,
	})
	return nil
}

func (r *Report) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}

// addRuntime adds what the server measured. Plans work without this data, and
// it needs a separate grant, so failures only leave a note.
func (r *Report) addRuntime(ctx context.Context, source compatibleSource, s sources.Source, tool, statement string) {
	r.addMeasuredRuntime(s, tool)
	const unavailable = "Runtime statistics are unavailable. They need SELECT on performance_schema.events_statements_summary_by_digest."
	digests, err := queryRows(ctx, source, digestQuery, digestable(statement))
	if err != nil || len(digests) == 0 {
		r.note("%s", unavailable)
		return
	}
	r.Digest = toString(digests[0]["digest"])
	if r.Runtime != nil {
		return
	}
	stats, err := queryRows(ctx, source, runtimeQuery, r.Digest)
	if err != nil {
		r.note("%s", unavailable)
		return
	}
	if len(stats) == 0 {
		r.note("No runtime statistics yet: the tool has not run under queryGuard since the source loaded, and the server does not record statements with bound parameters.")
		return
	}
	r.Runtime = &Runtime{
		MeasuredBy:   "server",
		Calls:        toInt(stats[0]["calls"]),
		TotalMs:      toString(stats[0]["total_ms"]),
		P95Ms:        toString(stats[0]["p95_ms"]),
		RowsExamined: toInt(stats[0]["rows_examined"]),
	}
}

// addMeasuredRuntime sums what Toolbox measured for the tool. A tool has one
// fingerprint unless its statement changed, and the sum is right either way.
func (r *Report) addMeasuredRuntime(s sources.Source, tool string) {
	p, ok := s.(queryguard.Provider)
	if !ok {
		return
	}
	stats, _ := p.GetQueryGuard().Stats()
	var measured Runtime
	var total, p95 int64
	for _, st := range stats {
		if st.Tool == tool {
			measured.Calls += st.Calls
			measured.Timeouts += st.Timeouts
			total += st.TotalDBMs
			p95 = max(p95, st.P95DBMs)
		}
	}
	if measured.Calls == 0 {
		return
	}
	measured.MeasuredBy = "toolbox"
	measured.TotalMs = strconv.FormatInt(total, 10)
	measured.P95Ms = strconv.FormatInt(p95, 10)
	r.Runtime = &measured
}

// digestable replaces placeholders with a literal. STATEMENT_DIGEST cannot parse
// them, and since digests normalize literals the result is the same either way.
func digestable(statement string) string {
	var b strings.Builder
	last := 0
	for _, loc := range literal.FindAllStringIndex(statement, -1) {
		b.WriteString(strings.ReplaceAll(statement[last:loc[0]], "?", "0"))
		b.WriteString(statement[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(strings.ReplaceAll(statement[last:], "?", "0"))
	return b.String()
}

func queryRows(ctx context.Context, source compatibleSource, statement string, arg string) ([]map[string]any, error) {
	result, err := source.RunSQL(ctx, statement, []any{arg})
	if err != nil {
		return nil, err
	}
	rows, ok := result.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected result type %T", result)
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		r, ok := row.(orderedmap.Row)
		if !ok {
			return nil, fmt.Errorf("unexpected row type %T", row)
		}
		m := make(map[string]any, len(r.Columns))
		for _, c := range r.Columns {
			m[c.Name] = c.Value
		}
		out = append(out, m)
	}
	return out, nil
}

// tableAliases maps aliases to table names. A wrong match is harmless: names are
// looked up in information_schema, and an unknown one only produces a note.
func tableAliases(statement string) map[string]string {
	out := map[string]string{}
	for _, m := range fromTable.FindAllStringSubmatch(statement, -1) {
		if alias := m[2]; alias != "" && !clauseWord.MatchString(alias) {
			out[alias] = m[1]
		}
	}
	return out
}

// filterColumns returns the columns of alias that a filter could use through an
// index, equality matches first, as they go first in a composite index.
func filterColumns(condition, alias string, columns map[string]map[string]any) []string {
	// Quotes are the only way a value can look like an identifier, so literals
	// are reduced to whether they start with a wildcard before anything else.
	text := literal.ReplaceAllStringFunc(condition, func(l string) string {
		if len(l) > 1 && l[1] == '%' {
			return "'%'"
		}
		return "'?'"
	})

	var equal, other []string
	for _, loc := range ident.FindAllStringIndex(text, -1) {
		parts := strings.Split(strings.Trim(text[loc[0]:loc[1]], "`"), "`.`")
		if len(parts) < 2 || parts[len(parts)-2] != alias {
			continue
		}
		name := parts[len(parts)-1]
		column, ok := columns[name]
		if !ok || !indexable(toString(column["data_type"])) || funcCall.MatchString(text[:loc[0]]) {
			continue
		}
		rest := strings.ToLower(strings.TrimSpace(text[loc[1]:]))
		switch {
		case strings.HasPrefix(rest, "like"):
			if !strings.HasPrefix(strings.TrimSpace(rest[len("like"):]), "'?'") {
				continue
			}
			other = append(other, name)
		case strings.HasPrefix(rest, "="):
			equal = append(equal, name)
		default:
			other = append(other, name)
		}
	}
	var out []string
	for _, name := range append(equal, other...) {
		if !slices.Contains(out, name) && len(out) < maxIndexCols {
			out = append(out, name)
		}
	}
	return out
}

// indexable rejects types that cannot be indexed without a prefix length, which
// would need a decision about selectivity this tool cannot make.
func indexable(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "tinytext", "text", "mediumtext", "longtext", "tinyblob", "blob", "mediumblob", "longblob", "json", "geometry":
		return false
	}
	return true
}

func indexDDL(table string, columns []string) (add, drop string) {
	name := "idx_" + table + "_" + strings.Join(columns, "_")
	if chars := []rune(name); len(chars) > maxIdentLen {
		name = string(chars[:maxIdentLen])
	}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = quote(c)
	}
	add = fmt.Sprintf("ALTER TABLE %s ADD INDEX %s (%s), ALGORITHM=INPLACE, LOCK=NONE;", quote(table), quote(name), strings.Join(quoted, ", "))
	drop = fmt.Sprintf("ALTER TABLE %s DROP INDEX %s, ALGORITHM=INPLACE, LOCK=NONE;", quote(table), quote(name))
	return add, drop
}

func quote(identifier string) string {
	return "`" + strings.ReplaceAll(identifier, "`", "``") + "`"
}

func toString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case uint64:
		return int64(x)
	case float64:
		return int64(x)
	default:
		n, _ := strconv.ParseInt(toString(v), 10, 64)
		return n
	}
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
