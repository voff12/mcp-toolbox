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

// Package resultmask rewrites tool results by column name before they leave the
// server. It denies by default: every returned column must be masked or allowed,
// so a column added to a table or a `SELECT *` fails the call rather than
// exposing data.
//
// Matching on names is only meaningful when the SQL is fixed. A caller who can
// write SQL can rename any column, so tools must opt in, and only those with a
// fixed statement can.
package resultmask

import (
	"fmt"
	"slices"
	"strings"

	"github.com/googleapis/mcp-toolbox/internal/util/orderedmap"
)

const (
	ActionRedact = "redact"
	ActionKeep   = "keep"
	ActionEmail  = "email"

	defaultReplacement = "***"
)

// Rule masks one column.
type Rule struct {
	Column      string `yaml:"column"`
	Action      string `yaml:"action"`
	First       int    `yaml:"first"`
	Last        int    `yaml:"last"`
	Replacement string `yaml:"replacement"`
}

// Config is the `resultMasking` block of a tool.
type Config struct {
	Allow []string `yaml:"allow"`
	Rules []Rule   `yaml:"rules"`
}

// Masker applies a Config to results. It is safe for concurrent use.
type Masker struct {
	rules map[string]Rule
	// known lists every configured column, which must appear in each row.
	known []string
}

// New validates c. A nil config returns a nil Masker.
func New(c *Config) (*Masker, error) {
	if c == nil {
		return nil, nil
	}
	m := &Masker{rules: make(map[string]Rule, len(c.Rules))}
	for _, r := range c.Rules {
		if r.Column == "" {
			return nil, fmt.Errorf("resultMasking: rule without a column")
		}
		if _, dup := m.rules[r.Column]; dup {
			return nil, fmt.Errorf("resultMasking: column %q has more than one rule", r.Column)
		}
		switch r.Action {
		case ActionRedact, ActionEmail:
			if r.First != 0 || r.Last != 0 {
				return nil, fmt.Errorf("resultMasking: column %q: first and last only apply to the %q action", r.Column, ActionKeep)
			}
		case ActionKeep:
			if r.First < 0 || r.Last < 0 || r.First+r.Last == 0 {
				return nil, fmt.Errorf("resultMasking: column %q: %q needs a positive first or last; use %q to hide the whole value", r.Column, ActionKeep, ActionRedact)
			}
		default:
			return nil, fmt.Errorf("resultMasking: column %q: unknown action %q", r.Column, r.Action)
		}
		if r.Replacement == "" {
			r.Replacement = defaultReplacement
		}
		m.rules[r.Column] = r
		m.known = append(m.known, r.Column)
	}
	for _, name := range c.Allow {
		if name == "" {
			return nil, fmt.Errorf("resultMasking: empty column name in allow")
		}
		if _, ruled := m.rules[name]; ruled {
			return nil, fmt.Errorf("resultMasking: column %q is in both allow and rules", name)
		}
		if slices.Contains(m.known, name) {
			return nil, fmt.Errorf("resultMasking: column %q is listed twice in allow", name)
		}
		m.known = append(m.known, name)
	}
	if len(m.known) == 0 {
		return nil, fmt.Errorf("resultMasking: at least one column must be allowed or masked")
	}
	return m, nil
}

// Apply returns a masked copy of result, which is the []any of rows sources
// return. It fails on anything it cannot prove safe. Its errors name columns
// only, never values.
func (m *Masker) Apply(result any) (any, error) {
	if result == nil {
		return nil, nil
	}
	rows, ok := result.([]any)
	if !ok {
		return nil, fmt.Errorf("unsupported result type %T", result)
	}
	out := make([]any, len(rows))
	for i, row := range rows {
		var err error
		switch r := row.(type) {
		case orderedmap.Row:
			out[i], err = m.maskRow(r)
		case map[string]any:
			out[i], err = m.maskMap(r)
		default:
			err = fmt.Errorf("unsupported row type %T", row)
		}
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (m *Masker) maskRow(r orderedmap.Row) (orderedmap.Row, error) {
	out := orderedmap.Row{Columns: make([]orderedmap.Column, len(r.Columns))}
	seen := make(map[string]struct{}, len(r.Columns))
	// Columns may repeat, as in a join, so each one is masked by position.
	for i, c := range r.Columns {
		v, err := m.maskValue(c.Name, c.Value)
		if err != nil {
			return orderedmap.Row{}, err
		}
		out.Columns[i] = orderedmap.Column{Name: c.Name, Value: v}
		seen[c.Name] = struct{}{}
	}
	return out, m.checkMissing(seen)
}

func (m *Masker) maskMap(r map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(r))
	seen := make(map[string]struct{}, len(r))
	for name, value := range r {
		v, err := m.maskValue(name, value)
		if err != nil {
			return nil, err
		}
		out[name] = v
		seen[name] = struct{}{}
	}
	return out, m.checkMissing(seen)
}

// checkMissing stops a changed SELECT or alias from silently disabling a rule.
func (m *Masker) checkMissing(seen map[string]struct{}) error {
	for _, name := range m.known {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("configured column %q is not in the result", name)
		}
	}
	return nil
}

func (m *Masker) maskValue(column string, value any) (any, error) {
	rule, ruled := m.rules[column]
	if !ruled {
		if !slices.Contains(m.known, column) {
			return nil, fmt.Errorf("column %q has no masking rule", column)
		}
		return value, nil
	}
	if value == nil {
		return nil, nil
	}
	if rule.Action == ActionRedact {
		return rule.Replacement, nil
	}

	var text string
	switch v := value.(type) {
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		// Formatting a number or time to keep part of it is a guess about what
		// is sensitive; the operator should cast it or use redact.
		return nil, fmt.Errorf("column %q: action %q needs a string, got %T", column, rule.Action, value)
	}
	if text == "" {
		return "", nil
	}
	if rule.Action == ActionEmail {
		return maskEmail(text, rule.Replacement), nil
	}
	return keep(text, rule), nil
}

// keep works on characters, not bytes, so names in any script stay valid UTF-8.
// A value too short to hide anything is replaced entirely.
func keep(text string, r Rule) string {
	chars := []rune(text)
	if len(chars) <= r.First+r.Last {
		return r.Replacement
	}
	return string(chars[:r.First]) + r.Replacement + string(chars[len(chars)-r.Last:])
}

// maskEmail keeps the domain and the first character of the local part. Text
// that does not look like an address could be anything, so none of it is kept.
func maskEmail(text, replacement string) string {
	at := strings.LastIndex(text, "@")
	if at <= 0 || at == len(text)-1 {
		return replacement
	}
	local := []rune(text[:at])
	if len(local) == 1 {
		return replacement + text[at:]
	}
	return string(local[0]) + replacement + text[at:]
}
