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
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

const identityReadOnly = "read-only"

var (
	grantLine = regexp.MustCompile("(?i)^GRANT (.+) ON (.+) TO (.+)$")
	// Column lists, as in SELECT (a, b), would otherwise split on the comma.
	columnList = regexp.MustCompile(`\s*\([^)]*\)`)
)

// verifyReadOnlyIdentity is the database half of the bypass boundary: if the
// account Toolbox uses can do little, a leak of its credentials can too.
func verifyReadOnlyIdentity(ctx context.Context, pool *sql.DB) error {
	// One connection, so both statements describe the same session.
	conn, err := pool.Conn(ctx)
	if err != nil {
		return fmt.Errorf("identityCheck: %w", err)
	}
	defer conn.Close()

	var account string
	if err := conn.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&account); err != nil {
		return fmt.Errorf("identityCheck: unable to read the current account: %w", err)
	}
	rows, err := conn.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return fmt.Errorf("identityCheck: unable to read grants: %w", err)
	}
	defer rows.Close()
	var grants []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return fmt.Errorf("identityCheck: unable to read grants: %w", err)
		}
		grants = append(grants, g)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("identityCheck: unable to read grants: %w", err)
	}
	return checkReadOnlyIdentity(account, grants)
}

// checkReadOnlyIdentity allows only what it recognizes, so a privilege MySQL
// adds later, a role or a partial revoke fails instead of passing unnoticed.
func checkReadOnlyIdentity(account string, grants []string) error {
	if _, host, ok := strings.Cut(account, "@"); !ok || host == "" || host == "%" {
		return fmt.Errorf("identityCheck: account %q can log in from any host; restrict it to the Toolbox network", account)
	}
	for _, g := range grants {
		if err := checkGrant(g); err != nil {
			return fmt.Errorf("identityCheck: account %q: %w", account, err)
		}
	}
	return nil
}

func checkGrant(grant string) error {
	if strings.HasSuffix(strings.ToUpper(grant), "WITH GRANT OPTION") {
		return fmt.Errorf("must not have GRANT OPTION: %s", grant)
	}
	m := grantLine.FindStringSubmatch(grant)
	if m == nil {
		return fmt.Errorf("unrecognized grant, such as a role or partial revoke: %s", grant)
	}
	object := strings.ToLower(strings.TrimSpace(m[2]))
	for _, p := range strings.Split(columnList.ReplaceAllString(m[1], ""), ",") {
		switch strings.ToUpper(strings.TrimSpace(p)) {
		case "USAGE":
		case "SELECT", "SHOW VIEW":
			// Global SELECT includes credential hashes in the mysql schema.
			if object == "*.*" || strings.HasPrefix(object, "`mysql`.") || strings.HasPrefix(object, "`sys`.") {
				return fmt.Errorf("must be limited to application schemas: %s", grant)
			}
		default:
			return fmt.Errorf("privilege %q is not read-only: %s", strings.TrimSpace(p), grant)
		}
	}
	return nil
}
