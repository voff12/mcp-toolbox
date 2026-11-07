---
title: "mysql-recommend-indexes"
type: docs
weight: 1
description: >
  A "mysql-recommend-indexes" tool suggests indexes for a configured mysql-sql
  tool, with evidence, for review by a DBA.
---

## About

A `mysql-recommend-indexes` tool plans the statement of a configured
[`mysql-sql`](./mysql-sql.md) tool and, where it scans a table, suggests an index
with the evidence behind it.

**The tool never changes the database.** It prints DDL for a person to review, and
has no way to run it. An index that looks right for one query can cost more in
writes and storage than it saves, so a model must not apply one on its own.

It takes the same parameters as the tool it analyzes, so call it with
representative values: a plan can depend on them. It does not execute the
statement, and its output never contains parameter values. Column names are only
reported if they exist on the table, so a value cannot pass as one.

It inherits `authRequired` and `scopesRequired` from the analyzed tool unless it
sets its own.

## Compatible Sources

{{< compatible-sources >}}

## Example

```yaml
kind: tool
name: recommend_indexes
type: mysql-recommend-indexes
tool: query_customers
description: Suggest indexes for query_customers, for review by a DBA.
```

## Output Format

```json
{
  "digest": "2c6e915a...",
  "runtime": {"measuredBy": "toolbox", "calls": 1520, "totalMs": "48211", "p95Ms": "100"},
  "recommendations": [
    {
      "table": "customers",
      "accessType": "ALL",
      "tableRows": 812000,
      "rowsExaminedPerScan": 812000,
      "existingIndexes": 1,
      "columns": ["level"],
      "estimatedIndexBytes": 40600000,
      "suggestedDdl": "ALTER TABLE `customers` ADD INDEX `idx_customers_level` (`level`), ALGORITHM=INPLACE, LOCK=NONE;",
      "rollbackDdl": "ALTER TABLE `customers` DROP INDEX `idx_customers_level`, ALGORITHM=INPLACE, LOCK=NONE;",
      "status": "requires_dba_approval"
    }
  ],
  "notes": [],
  "disclaimer": "..."
}
```

`notes` explains tables that got no suggestion: too few rows, no filter to index,
a usable index the optimizer passed over, or a filter an index cannot serve.

## Reference

| **field**   | **type** | **required** | **description**                                               |
|-------------|:--------:|:------------:|---------------------------------------------------------------|
| type        | string   |     true     | Must be "mysql-recommend-indexes".                            |
| tool        | string   |     true     | Name of a `mysql-sql` tool without `templateParameters`.      |
| description | string   |     true     | Description of the tool that is passed to the LLM.            |

## Advanced Usage

### Reviewing a suggestion

Treat each suggestion as a hypothesis:

1. Check `runtime`. A rarely run statement may not be worth a permanent write cost.
2. Consider rewriting the query, pagination or statistics first.
3. Review the column order. Equality filters go first, but the tool cannot judge
   selectivity, and among them the most selective usually belongs first.
4. Check `existingIndexes`, and whether extending an index would do.
5. Compare query time and write load on a production-sized copy.
6. Apply through a reviewed migration, in a maintenance window if the table is busy.
7. Watch query and write metrics afterwards. Keep `rollbackDdl` ready.

`estimatedIndexBytes` is a rough upper estimate based on column sizes.

The tool looks at filters only. It does not suggest covering indexes or indexes
for sorting or joins, and it skips `TEXT`, `BLOB` and `JSON` columns, which need a
prefix length a person should choose.
