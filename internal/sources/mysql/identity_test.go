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

package mysql

import (
	"strings"
	"testing"
)

const okAccount = "toolbox@10.0.%"

func TestCheckReadOnlyIdentityAccepts(t *testing.T) {
	grants := []string{
		"GRANT USAGE ON *.* TO `toolbox`@`10.0.%`",
		"GRANT SELECT ON `shop`.* TO `toolbox`@`10.0.%`",
		"GRANT SELECT, SHOW VIEW ON `shop`.`customers` TO `toolbox`@`10.0.%`",
		"GRANT SELECT (`id`, `level`) ON `shop`.`orders` TO `toolbox`@`10.0.%`",
		"GRANT SELECT ON `performance_schema`.`events_statements_summary_by_digest` TO `toolbox`@`10.0.%`",
	}
	if err := checkReadOnlyIdentity(okAccount, grants); err != nil {
		t.Fatal(err)
	}
}

func TestCheckReadOnlyIdentityRejects(t *testing.T) {
	tests := []struct {
		name    string
		account string
		grant   string
		want    string
	}{
		{"any host", "toolbox@%", "GRANT USAGE ON *.* TO `toolbox`@`%`", "any host"},
		{"no host", "toolbox", "GRANT USAGE ON *.* TO `toolbox`@`%`", "any host"},
		{"all privileges", okAccount, "GRANT ALL PRIVILEGES ON `shop`.* TO `t`@`h`", "not read-only"},
		{"write", okAccount, "GRANT SELECT, INSERT ON `shop`.* TO `t`@`h`", `"INSERT"`},
		{"ddl", okAccount, "GRANT CREATE ON `shop`.* TO `t`@`h`", `"CREATE"`},
		{"index", okAccount, "GRANT INDEX ON `shop`.* TO `t`@`h`", `"INDEX"`},
		{"session admin", okAccount, "GRANT CONNECTION_ADMIN ON *.* TO `t`@`h`", `"CONNECTION_ADMIN"`},
		{"sees other sessions", okAccount, "GRANT PROCESS ON *.* TO `t`@`h`", `"PROCESS"`},
		{"file", okAccount, "GRANT FILE ON *.* TO `t`@`h`", `"FILE"`},
		{"routines", okAccount, "GRANT EXECUTE ON `shop`.* TO `t`@`h`", `"EXECUTE"`},
		{"column write", okAccount, "GRANT SELECT (`a`), UPDATE (`a`) ON `shop`.`t` TO `t`@`h`", `"UPDATE"`},
		{"global select", okAccount, "GRANT SELECT ON *.* TO `t`@`h`", "application schemas"},
		{"system schema", okAccount, "GRANT SELECT ON `mysql`.* TO `t`@`h`", "application schemas"},
		{"system table", okAccount, "GRANT SELECT ON `mysql`.`user` TO `t`@`h`", "application schemas"},
		{"grant option", okAccount, "GRANT SELECT ON `shop`.* TO `t`@`h` WITH GRANT OPTION", "GRANT OPTION"},
		{"role", okAccount, "GRANT `admin`@`%` TO `t`@`h`", "unrecognized grant"},
		{"partial revoke", okAccount, "REVOKE SELECT ON `mysql`.* FROM `t`@`h`", "unrecognized grant"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := checkReadOnlyIdentity(tc.account, []string{tc.grant})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}
