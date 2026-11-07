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
	"fmt"
	"testing"

	"github.com/googleapis/mcp-toolbox/internal/util"
)

func statGuard() *Guard { return &Guard{} }

func call(tool, id string, dbMs int64) event {
	return event{Call: Call{Tool: tool}, QueryID: id, Outcome: outcomeSuccess, DBMs: dbMs, Rows: 2}
}

func TestStatsAggregateByToolAndQuery(t *testing.T) {
	g := statGuard()
	g.stats.record(call("a", "q1", 10))
	g.stats.record(call("a", "q1", 30))
	g.stats.record(call("a", "q2", 500))
	g.stats.record(call("b", "q1", 1))
	failed := call("a", "q1", 40)
	failed.Outcome, failed.ErrorCode = util.QueryTimeout, util.QueryTimeout
	g.stats.record(failed)

	got, dropped := g.Stats()
	if dropped != 0 || len(got) != 3 {
		t.Fatalf("got %+v, dropped %d", got, dropped)
	}
	if got[0].QueryID != "q2" {
		t.Errorf("not sorted by total time: %+v", got)
	}
	q1 := got[1]
	if q1.Tool != "a" || q1.Calls != 3 || q1.Failures != 1 || q1.Timeouts != 1 || q1.TotalDBMs != 80 || q1.MaxDBMs != 40 || q1.RowsReturned != 6 {
		t.Errorf("got %+v", q1)
	}
}

func TestStatsP95(t *testing.T) {
	g := statGuard()
	for range 94 {
		g.stats.record(call("a", "q", 3))
	}
	for range 6 {
		g.stats.record(call("a", "q", 400))
	}
	got, _ := g.Stats()
	if got[0].P95DBMs != 400 {
		t.Errorf("p95 = %d: capped at max within the 500 bucket", got[0].P95DBMs)
	}

	g = statGuard()
	for range 100 {
		g.stats.record(call("a", "q", 3))
	}
	if got, _ := g.Stats(); got[0].P95DBMs != 3 {
		t.Errorf("p95 = %d", got[0].P95DBMs)
	}

	g = statGuard()
	g.stats.record(call("a", "q", 120000))
	if got, _ := g.Stats(); got[0].P95DBMs != 120000 {
		t.Errorf("past the last bucket: p95 = %d", got[0].P95DBMs)
	}
}

func TestStatsAreBounded(t *testing.T) {
	g := statGuard()
	for i := range maxStatEntries + 5 {
		g.stats.record(call("a", fmt.Sprint(i), 1))
	}
	got, dropped := g.Stats()
	if len(got) != maxStatEntries || dropped != 5 {
		t.Fatalf("entries %d, dropped %d", len(got), dropped)
	}
	// Known keys keep counting when the table is full.
	g.stats.record(call("a", "0", 1))
	if _, dropped := g.Stats(); dropped != 5 {
		t.Fatalf("dropped = %d", dropped)
	}
}

func TestStatsSkipCallsThatNeverRan(t *testing.T) {
	g := statGuard()
	g.stats.record(call("a", "", 0))
	if got, _ := g.Stats(); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
	var none *Guard
	if got, _ := none.Stats(); got != nil {
		t.Fatalf("nil guard: %+v", got)
	}
}
