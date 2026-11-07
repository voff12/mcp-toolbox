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

package server

import (
	"context"
	"strings"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/queryguard"
	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/resultmask"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/testutils"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

type guardFakeSource struct {
	sources.Source
	guard *queryguard.Guard
}

func (s guardFakeSource) GetQueryGuard() *queryguard.Guard { return s.guard }

type guardFakeTool struct {
	tools.Tool
	resources []resources.Resource
}

func (guardFakeTool) GetName() string       { return "fake" }
func (guardFakeTool) GetSourceName() string { return "src" }
func (guardFakeTool) Invoke(ctx context.Context, _ sources.Source, _ parameters.ParamValues, _ tools.AccessToken) (any, util.ToolboxError) {
	_, st := queryguard.Begin(ctx, "select 1")
	defer st.End()
	return "ok", nil
}
func (t guardFakeTool) GetResources() []resources.Resource { return t.resources }

type guardFakeConfig struct {
	tools.ToolConfig
	cfg *queryguard.ToolConfig
}

func (c guardFakeConfig) GetQueryGuardConfig() *queryguard.ToolConfig { return c.cfg }

func testGuard(t *testing.T) *queryguard.Guard {
	t.Helper()
	g, err := queryguard.New(&queryguard.Config{
		MaxConcurrent: 1, QueryTimeout: "6s", StatementTimeout: "4s", MaxRows: 10, MaxResultBytes: "1MiB",
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestGuardToolLeavesUnguardedToolsAlone(t *testing.T) {
	tool := guardFakeTool{}
	got, err := guardTool(tool, guardFakeConfig{}, guardFakeSource{}, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := got.(guardedTool); wrapped {
		t.Error("tool on an unguarded source must not be wrapped")
	}
}

func TestGuardToolRunsUnderGuardAndForwardsResources(t *testing.T) {
	var res []resources.Resource
	res = append(res, nil)
	got, err := guardTool(guardFakeTool{resources: res}, guardFakeConfig{}, guardFakeSource{guard: testGuard(t)}, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := got.(guardedTool); !wrapped {
		t.Fatal("tool must be wrapped")
	}
	out, tbErr := got.Invoke(context.Background(), guardFakeSource{}, nil, "")
	if tbErr != nil || out != "ok" {
		t.Fatalf("Invoke = %v, %v", out, tbErr)
	}
	rp, ok := got.(resources.ResourceProvider)
	if !ok || len(rp.GetResources()) != 1 {
		t.Error("wrapper must forward resources.ResourceProvider, or dynamic UI resources disappear")
	}
}

func TestGuardToolRejectsBadConfig(t *testing.T) {
	cfg := func(c queryguard.ToolConfig) tools.ToolConfig { return guardFakeConfig{cfg: &c} }
	all := ServerConfig{ToolConfigs: ToolConfigs{"summary": guardFakeConfig{}}}

	tests := []struct {
		name string
		cfg  tools.ToolConfig
		src  sources.Source
		want string
	}{
		{"source has no guard", cfg(queryguard.ToolConfig{MaxRows: 5}), guardFakeSource{}, "no queryGuard limits"},
		{"loosens source", cfg(queryguard.ToolConfig{MaxRows: 11}), guardFakeSource{guard: testGuard(t)}, "must not exceed"},
		{"missing fallback", cfg(queryguard.ToolConfig{FallbackTool: "nope"}), guardFakeSource{guard: testGuard(t)}, "does not exist"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := guardTool(guardFakeTool{}, tc.cfg, tc.src, all); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	if _, err := guardTool(guardFakeTool{}, cfg(queryguard.ToolConfig{FallbackTool: "summary"}), guardFakeSource{guard: testGuard(t)}, all); err != nil {
		t.Errorf("valid fallback rejected: %v", err)
	}
}

// maskFakeConfig is a tool config that supports resultMasking.
type maskFakeConfig struct {
	guardFakeConfig
	mask     *resultmask.Config
	checkErr error
}

func (c maskFakeConfig) GetResultMaskingConfig() *resultmask.Config { return c.mask }
func (c maskFakeConfig) CheckResultMasking() error                  { return c.checkErr }

// unmaskableConfig has the field every tool inherits, without the capability.
type unmaskableConfig struct {
	guardFakeConfig
	mask *resultmask.Config
}

func (c unmaskableConfig) GetResultMaskingConfig() *resultmask.Config { return c.mask }

type governedOutputConfig struct{ guardFakeConfig }

func (governedOutputConfig) GovernedOutput() {}

// rowTool returns fixed rows through the guard's accounting.
type rowTool struct {
	guardFakeTool
	result any
	auth   []string
	scopes []string
}

func (t rowTool) GetAuthRequired() []string   { return t.auth }
func (t rowTool) GetScopesRequired() []string { return t.scopes }
func (t rowTool) Invoke(ctx context.Context, _ sources.Source, _ parameters.ParamValues, _ tools.AccessToken) (any, util.ToolboxError) {
	_, st := queryguard.Begin(ctx, "select 1")
	defer st.End()
	return t.result, nil
}

func phoneMask() *resultmask.Config {
	return &resultmask.Config{
		Allow: []string{"id"},
		Rules: []resultmask.Rule{{Column: "phone", Action: resultmask.ActionKeep, First: 3, Last: 4, Replacement: "****"}},
	}
}

func phoneRow(extra ...string) []any {
	var r orderedmap.Row
	r.Add("id", 1)
	r.Add("phone", "13812345678")
	for _, c := range extra {
		r.Add(c, "13812345678")
	}
	return []any{r}
}

func TestMaskingAppliesInsideTheGuard(t *testing.T) {
	src := guardFakeSource{guard: testGuard(t)}
	got, err := guardTool(rowTool{result: phoneRow()}, maskFakeConfig{mask: phoneMask()}, src, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	out, tbErr := got.Invoke(initGuardContext(t), src, nil, "")
	if tbErr != nil {
		t.Fatal(tbErr)
	}
	if v := out.([]any)[0].(orderedmap.Row).Columns[1].Value; v != "138****5678" {
		t.Fatalf("got %v", v)
	}
}

func TestMaskingFailureWithholdsResult(t *testing.T) {
	src := guardFakeSource{guard: testGuard(t)}
	got, err := guardTool(rowTool{result: phoneRow("address")}, maskFakeConfig{mask: phoneMask()}, src, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	out, tbErr := got.Invoke(initGuardContext(t), src, nil, "")
	if out != nil {
		t.Fatalf("result returned: %v", out)
	}
	if util.ErrorType(tbErr) != util.QueryMaskingFailed {
		t.Fatalf("got %v", tbErr)
	}
	if !strings.Contains(tbErr.Error(), "address") || strings.Contains(tbErr.Error(), "13812345678") {
		t.Fatalf("error must name the column, not the value: %v", tbErr)
	}
}

func TestMaskingWithholdsStructuredResults(t *testing.T) {
	src := guardFakeSource{guard: testGuard(t)}
	res := tools.StructuredToolResult{Content: phoneRow(), StructuredContent: map[string]any{"phone": "13812345678"}}
	got, err := guardTool(rowTool{result: res}, maskFakeConfig{mask: phoneMask()}, src, ServerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	out, tbErr := got.Invoke(initGuardContext(t), src, nil, "")
	if out != nil || util.ErrorType(tbErr) != util.QueryMaskingFailed {
		t.Fatalf("Invoke = %v, %v", out, tbErr)
	}
}

func TestMaskingConfigValidation(t *testing.T) {
	guarded := guardFakeSource{guard: testGuard(t)}
	tests := []struct {
		name string
		cfg  tools.ToolConfig
		src  sources.Source
		want string
	}{
		{"tool type without capability", unmaskableConfig{mask: phoneMask()}, guarded, "not supported"},
		{"caller controls sql", maskFakeConfig{mask: phoneMask(), checkErr: errFake("templateParameters")}, guarded, "templateParameters"},
		{"source without guard", maskFakeConfig{mask: phoneMask()}, guardFakeSource{}, "requires source"},
		{"invalid rules", maskFakeConfig{mask: &resultmask.Config{}}, guarded, "at least one column"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := guardTool(guardFakeTool{}, tc.cfg, tc.src, ServerConfig{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestStrictGovernance(t *testing.T) {
	guarded := guardFakeSource{guard: testGuard(t)}
	strict := ServerConfig{StrictGovernance: true}
	tests := []struct {
		name string
		tool tools.Tool
		cfg  tools.ToolConfig
		src  sources.Source
		sc   ServerConfig
		want string
	}{
		{name: "unguarded source", tool: rowTool{auth: []string{"a"}}, cfg: guardFakeConfig{}, src: guardFakeSource{}, sc: strict, want: "no queryGuard limits"},
		{name: "source without guard support", tool: rowTool{auth: []string{"a"}}, cfg: guardFakeConfig{}, sc: strict, want: "no queryGuard limits"},
		{name: "open and unmasked", tool: rowTool{}, cfg: guardFakeConfig{}, src: guarded, sc: strict, want: "without resultMasking"},
		{name: "masked", tool: rowTool{}, cfg: maskFakeConfig{mask: phoneMask()}, src: guarded, sc: strict},
		{name: "authenticated", tool: rowTool{auth: []string{"a"}}, cfg: guardFakeConfig{}, src: guarded, sc: strict},
		{name: "scoped", tool: rowTool{scopes: []string{"a"}}, cfg: guardFakeConfig{}, src: guarded, sc: strict},
		{name: "summary only", tool: rowTool{}, cfg: governedOutputConfig{}, src: guarded, sc: strict},
		{name: "offline validation has no sources", tool: rowTool{auth: []string{"a"}}, cfg: guardFakeConfig{}, sc: ServerConfig{StrictGovernance: true, SkipSourceValidation: true}},
		{name: "not strict", tool: rowTool{}, cfg: guardFakeConfig{}, src: guarded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := guardTool(tc.tool, tc.cfg, tc.src, tc.sc)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

func initGuardContext(t *testing.T) context.Context {
	t.Helper()
	ctx, err := testutils.ContextWithNewLogger()
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}
