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

package queryguard

import (
	"cmp"
	"slices"
	"sync"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

// maxStatEntries bounds memory. Tools with fixed SQL produce few keys, but free
// form SQL tools produce one per distinct statement, so the map must not grow
// with the input. Later keys are counted in Dropped rather than recorded.
const maxStatEntries = 1000

// latencyBounds are the upper bounds of the histogram, in milliseconds.
var latencyBounds = []int64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000, 30000, 60000}

// ToolStat aggregates the calls of a tool with one SQL fingerprint since the
// source was initialized. It holds no SQL text or parameters.
type ToolStat struct {
	Tool         string `json:"tool"`
	QueryID      string `json:"queryId"`
	Calls        int64  `json:"calls"`
	Failures     int64  `json:"failures"`
	Timeouts     int64  `json:"timeouts"`
	TotalDBMs    int64  `json:"totalDbMs"`
	MaxDBMs      int64  `json:"maxDbMs"`
	RowsReturned int64  `json:"rowsReturned"`
	// P95DBMs is the upper bound of the histogram bucket holding the 95th
	// percentile, or MaxDBMs past the last bucket.
	P95DBMs int64 `json:"p95DbMs"`
}

type statKey struct{ tool, queryID string }

type statEntry struct {
	stat    ToolStat
	buckets []int64
}

type statTable struct {
	mu      sync.Mutex
	entries map[statKey]*statEntry
	dropped int64
}

func (t *statTable) record(ev event) {
	// Nothing reached the database, so there is no statement to attribute.
	if ev.QueryID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := statKey{ev.Tool, ev.QueryID}
	e, ok := t.entries[key]
	if !ok {
		if t.entries == nil {
			t.entries = make(map[statKey]*statEntry)
		}
		if len(t.entries) >= maxStatEntries {
			t.dropped++
			return
		}
		e = &statEntry{
			stat:    ToolStat{Tool: ev.Tool, QueryID: ev.QueryID},
			buckets: make([]int64, len(latencyBounds)+1),
		}
		t.entries[key] = e
	}
	e.stat.Calls++
	if ev.Outcome != outcomeSuccess {
		e.stat.Failures++
	}
	if ev.ErrorCode == util.QueryTimeout {
		e.stat.Timeouts++
	}
	e.stat.TotalDBMs += ev.DBMs
	e.stat.MaxDBMs = max(e.stat.MaxDBMs, ev.DBMs)
	e.stat.RowsReturned += ev.Rows
	i, _ := slices.BinarySearch(latencyBounds, ev.DBMs)
	e.buckets[i]++
}

func (e *statEntry) p95() int64 {
	target := e.stat.Calls - e.stat.Calls*5/100
	var seen int64
	for i, n := range e.buckets {
		seen += n
		if seen >= target {
			if i < len(latencyBounds) {
				return min(latencyBounds[i], e.stat.MaxDBMs)
			}
			break
		}
	}
	return e.stat.MaxDBMs
}

// Stats returns the aggregates by total database time, highest first, and how
// many calls were left out because the table was full. A nil Guard returns none.
// Counts live in memory: they reset when the source is reloaded or Toolbox
// restarts, and each Toolbox instance has its own.
func (g *Guard) Stats() ([]ToolStat, int64) {
	if g == nil {
		return nil, 0
	}
	g.stats.mu.Lock()
	defer g.stats.mu.Unlock()
	out := make([]ToolStat, 0, len(g.stats.entries))
	for _, e := range g.stats.entries {
		s := e.stat
		s.P95DBMs = e.p95()
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b ToolStat) int {
		return cmp.Or(cmp.Compare(b.TotalDBMs, a.TotalDBMs), cmp.Compare(a.Tool, b.Tool), cmp.Compare(a.QueryID, b.QueryID))
	})
	return out, g.stats.dropped
}
