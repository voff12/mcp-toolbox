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
	"fmt"

	"github.com/googleapis/mcp-toolbox/internal/queryguard"
	"github.com/googleapis/mcp-toolbox/internal/resources"
	"github.com/googleapis/mcp-toolbox/internal/resultmask"
	"github.com/googleapis/mcp-toolbox/internal/sources"
	"github.com/googleapis/mcp-toolbox/internal/tools"
	"github.com/googleapis/mcp-toolbox/internal/util"
	"github.com/googleapis/mcp-toolbox/internal/util/parameters"
)

// guardedTool runs a tool under its source's query guard.
type guardedTool struct {
	tools.Tool
	guard  *queryguard.Guard
	policy queryguard.Policy
	// masker runs inside the guard so that a withheld result is audited.
	masker *resultmask.Masker
}

// Embedding hides optional interfaces of the wrapped tool, so each one the
// server asserts on must be forwarded here.
var _ resources.ResourceProvider = guardedTool{}

func (g guardedTool) Invoke(ctx context.Context, src sources.Source, params parameters.ParamValues, token tools.AccessToken) (any, util.ToolboxError) {
	call := queryguard.Call{Tool: g.GetName(), Source: g.GetSourceName()}
	return g.guard.Invoke(ctx, call, g.policy, func(ctx context.Context) (any, util.ToolboxError) {
		res, err := g.Tool.Invoke(ctx, src, params, token)
		if err != nil || g.masker == nil {
			return res, err
		}
		masked, maskErr := g.masker.Apply(res)
		if maskErr != nil {
			return nil, &util.QueryError{
				Code:    util.QueryMaskingFailed,
				Message: fmt.Sprintf("The result was withheld: %v. Contact an administrator to update the tool's resultMasking.", maskErr),
			}
		}
		return masked, nil
	})
}

// GetResources forwards resources.ResourceProvider.
func (g guardedTool) GetResources() []resources.Resource {
	if rp, ok := g.Tool.(resources.ResourceProvider); ok {
		return rp.GetResources()
	}
	return nil
}

// guardTool applies the governance configured for t. A nil guard leaves t as is,
// so tools without any governance config are unaffected.
func guardTool(t tools.Tool, tc tools.ToolConfig, src sources.Source, cfg ServerConfig) (tools.Tool, error) {
	var toolCfg *queryguard.ToolConfig
	if c, ok := tc.(interface {
		GetQueryGuardConfig() *queryguard.ToolConfig
	}); ok {
		toolCfg = c.GetQueryGuardConfig()
	}

	masker, err := newMasker(tc)
	if err != nil {
		return nil, err
	}

	var guard *queryguard.Guard
	if p, ok := src.(queryguard.Provider); ok {
		guard = p.GetQueryGuard()
	}
	// Offline validation has no sources, so guard presence cannot be judged.
	guardKnown := src != nil || !cfg.SkipSourceValidation
	if cfg.StrictGovernance {
		if err := checkStrict(t, tc, guard, masker, guardKnown); err != nil {
			return nil, err
		}
	}
	if guard == nil {
		if !guardKnown {
			return t, nil
		}
		if toolCfg != nil {
			return nil, fmt.Errorf("queryGuard is set but source %q has no queryGuard limits to tighten", t.GetSourceName())
		}
		if masker != nil {
			// Database errors can quote row values. Without a guard they would
			// reach the caller unfiltered, around the masking.
			return nil, fmt.Errorf("resultMasking requires source %q to have queryGuard limits", t.GetSourceName())
		}
		return t, nil
	}

	policy, err := guard.Policy(toolCfg)
	if err != nil {
		return nil, err
	}
	if policy.FallbackTool != "" {
		if _, ok := cfg.ToolConfigs[policy.FallbackTool]; !ok {
			return nil, fmt.Errorf("queryGuard.fallbackTool %q does not exist", policy.FallbackTool)
		}
	}
	return guardedTool{Tool: t, guard: guard, policy: policy, masker: masker}, nil
}

func newMasker(tc tools.ToolConfig) (*resultmask.Masker, error) {
	c, ok := tc.(interface {
		GetResultMaskingConfig() *resultmask.Config
	})
	if !ok || c.GetResultMaskingConfig() == nil {
		return nil, nil
	}
	maskable, ok := tc.(tools.MaskableConfig)
	if !ok {
		return nil, fmt.Errorf("resultMasking is not supported by this tool type")
	}
	if err := maskable.CheckResultMasking(); err != nil {
		return nil, err
	}
	return resultmask.New(c.GetResultMaskingConfig())
}

func checkStrict(t tools.Tool, tc tools.ToolConfig, guard *queryguard.Guard, masker *resultmask.Masker, guardKnown bool) error {
	if guard == nil && guardKnown {
		return fmt.Errorf("strict governance: source %q has no queryGuard limits", t.GetSourceName())
	}
	if masker != nil || len(t.GetAuthRequired()) > 0 || len(t.GetScopesRequired()) > 0 {
		return nil
	}
	if _, ok := tc.(tools.GovernedOutputConfig); ok {
		return nil
	}
	return fmt.Errorf("strict governance: the tool returns row data without resultMasking, authRequired or scopesRequired")
}
