---
title: "mysql-explain"
type: docs
weight: 1
description: >
  A "mysql-explain" tool returns a summary of the execution plan of a configured
  mysql-sql tool.
---

## About

A `mysql-explain` tool plans the statement of a configured
[`mysql-sql`](./mysql-sql.md) tool and returns a structured summary. Use it to
let an agent see whether a query is indexed without letting it run SQL of its
own.

The tool takes the same parameters as the tool it plans, and runs against the
same source. It runs `EXPLAIN FORMAT=JSON`, which plans the statement without
executing it. `EXPLAIN ANALYZE`, which does execute it, is not available.

Unless it sets its own, the tool inherits `authRequired` and `scopesRequired`
from the planned tool, because a plan reveals schema details.

**How this differs from `mysql-get-query-plan`**

| | **`mysql-explain`** | **`mysql-get-query-plan`** |
|---|---|---|
| Statement | Fixed by configuration | Supplied by the caller |
| Output | Allowlisted summary | Full plan |

The full plan can show parameter values as literals in filter conditions, so a
search by phone number would echo the number. The summary leaves out conditions,
and lists the columns each table access reads instead. `mysql-get-query-plan`
should stay with administrators.

## Compatible Sources

{{< compatible-sources >}}

## Example

```yaml
kind: tool
name: find_customer
type: mysql-sql
source: my-mysql-instance
description: Find a customer by phone number.
statement: SELECT id FROM customers WHERE phone = ?
parameters:
  - name: phone
    type: string
    description: Phone number
---
kind: tool
name: explain_find_customer
type: mysql-explain
tool: find_customer
description: Show how find_customer would read the database.
```

## Output Format

```json
{
  "estimatedCost": "0.35",
  "usingFilesort": false,
  "usingTemporary": false,
  "tables": [
    {
      "table": "customers",
      "accessType": "ref",
      "possibleKeys": ["idx_phone"],
      "key": "idx_phone",
      "usedKeyParts": ["phone"],
      "rowsExaminedPerScan": 1,
      "rowsProducedPerJoin": 1,
      "filteredPercent": "100.00",
      "columns": ["id", "phone"]
    }
  ]
}
```

`estimatedCost` is absent for a union. `tables` is empty when MySQL needs no
table access, for example when it can tell that no row matches. The numbers are
the optimizer's estimates, not measurements.

## Reference

| **field**   | **type** | **required** | **description**                                               |
|-------------|:--------:|:------------:|---------------------------------------------------------------|
| type        | string   |     true     | Must be "mysql-explain".                                      |
| tool        | string   |     true     | Name of a `mysql-sql` tool without `templateParameters`.      |
| description | string   |     true     | Description of the tool that is passed to the LLM.            |

## Troubleshooting

The tool reads format version 1 of MySQL's JSON plans. If the server has
`explain_json_format_version` set to `2`, every call fails: with
`QUERY_FAILED` and no `dbErrorCode` on a source with `queryGuard`, otherwise with
`unsupported query plan format`. Set the version for the connection in the
source's `queryParams`:

```yaml
queryParams:
  explain_json_format_version: "1"
```
