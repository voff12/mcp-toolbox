---
title: "mysql-slow-query-report"
type: docs
weight: 1
description: >
  A "mysql-slow-query-report" tool ranks statements by total time, from what
  Toolbox measured and what MySQL recorded.
---

## About

A `mysql-slow-query-report` tool shows where database time goes, so tuning can
start with the statements that cost the most overall rather than the slowest
single call.

It reports two views, because neither is complete:

| **Section** | **Covers**                                   | **Misses**                                           |
|-------------|----------------------------------------------|------------------------------------------------------|
| `toolbox`   | Tools on this source, while it has `queryGuard`. | Other clients and Toolbox instances; data resets when the source reloads. |
| `server`    | All clients, from MySQL's statement digests. | Statements with bound parameters.                    |

On MySQL 8.4, digests do not count statements run through the binary protocol, and
that is how any tool with `parameters` runs. Those tools only appear in `toolbox`.
[`mysql-show-query-stats`](./mysql-show-query-stats.md) has the same gap.

Neither section contains parameter values. `toolbox` has no SQL at all, only a
fingerprint, and digests have literals replaced with `?`. Digests do reveal the
shape of every client's statements, so give this tool to administrators.

## Compatible Sources

{{< compatible-sources >}}

## Requirements

`performance_schema` must be enabled, and the account needs:

```sql
GRANT SELECT ON performance_schema.events_statements_summary_by_digest TO 'toolbox_ro'@'10.0.1.10';
```

## Parameters

| **parameter** | **type** | **required** | **description**                                       |
|---------------|:--------:|:------------:|-------------------------------------------------------|
| database      | string   |    false     | Only list `server` statements run in this database.   |
| limit         | integer  |    false     | Maximum entries in each section. Defaults to 20.      |

## Example

```yaml
kind: tool
name: slow_query_report
type: mysql-slow-query-report
source: my-mysql-instance
authRequired:
  - my-auth-service
```

## Output Format

```json
{
  "toolbox": [
    {
      "tool": "query_customers",
      "queryId": "1f129a9ea53b631d",
      "calls": 1520,
      "failures": 3,
      "timeouts": 2,
      "totalDbMs": 48211,
      "maxDbMs": 4001,
      "rowsReturned": 90211,
      "p95DbMs": 100
    }
  ],
  "server": [
    {
      "digest": "63af6025...",
      "db": "shop",
      "query": "SELECT COUNT ( * ) FROM `orders` WHERE `status` = ?",
      "calls": 310,
      "total_ms": "8120.50",
      "p95_ms": "41.20",
      "errors": 0,
      "rows_examined": 9300000,
      "findings": "full_scan,low_row_efficiency"
    }
  ],
  "notes": []
}
```

`p95DbMs` is the upper bound of a histogram bucket, so it is an approximation.

| **finding**          | **meaning**                                      |
|----------------------|--------------------------------------------------|
| `full_scan`          | At least one execution used no index.            |
| `no_good_index`      | MySQL found no suitable index.                   |
| `low_row_efficiency` | Over 100 rows examined for each row sent.        |
| `disk_temp_table`    | Temporary tables went to disk.                   |
| `disk_sort`          | Sorting needed merge passes.                     |
| `errors`             | At least one execution failed.                   |

## Reference

| **field**   | **type** | **required** | **description**                                     |
|-------------|:--------:|:------------:|-----------------------------------------------------|
| type        | string   |     true     | Must be "mysql-slow-query-report".                  |
| source      | string   |     true     | Name of the source to report on.                    |
| description | string   |    false     | Description of the tool that is passed to the LLM.  |
