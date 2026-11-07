---
title: "Query Guard"
type: docs
weight: 3
description: >
  Limit the concurrency, duration and result size of queries on a source.
---

Query Guard stops a single tool call from exhausting a database or Toolbox
itself. Add a `queryGuard` block to a source and every tool on that source runs
under its limits.

It is supported by the `mysql`, `postgres` and `cockroachdb` source types. Any
other source type rejects the block at startup, rather than ignoring it.

{{< notice note >}}
Query Guard limits resource use. It does not hide data or restrict which tables
a query can read. Use [Result Masking](./result-masking.md) for sensitive columns,
and a read-only database user with minimal grants as the access boundary.
{{< /notice >}}

## Example

```yaml
kind: source
name: shop
type: mysql
host: 127.0.0.1
port: "3306"
database: shop
user: toolbox_readonly
password: ${DB_PASSWORD}
queryGuard:
  maxConcurrent: 4
  maxQueue: 20
  queueTimeout: 500ms
  queryTimeout: 6s
  statementTimeout: 4s
  lockTimeout: 1s
  maxRows: 500
  maxResultBytes: 2MiB
  slowQueryThreshold: 1s
---
kind: tool
name: find_customers
type: mysql-sql
source: shop
description: Find customers by tier.
statement: SELECT id, name FROM customers WHERE tier = ?
parameters:
  - name: tier
    type: string
    description: Customer tier.
queryGuard:
  queryTimeout: 5s
  maxRows: 200
  fallbackTool: customer_summary
```

## Source fields

| **field**          | **type** | **required** | **description**                                                                                                                 |
|--------------------|:--------:|:------------:|---------------------------------------------------------------------------------------------------------------------------------|
| maxConcurrent      | integer  | true         | Queries that can run at once on this source.                                                                                    |
| maxQueue           | integer  | false        | Queries that can wait for a slot. Beyond this, callers get `QUERY_BUSY`. Defaults to 0, which means no waiting.                 |
| queueTimeout       | string   | false        | Longest wait for a slot. Required when `maxQueue` is set.                                                                       |
| queryTimeout       | string   | true         | Overall deadline for a tool call, including queueing and reading results.                                                       |
| statementTimeout   | string   | true         | Limit enforced by the database server. `statementTimeout` + `queueTimeout` must be less than `queryTimeout`.                    |
| lockTimeout        | string   | false        | Longest wait for a lock. MySQL only supports whole seconds, so the value is rounded up.                                         |
| maxRows            | integer  | true         | Rows a tool call can read. Exceeding it fails the call.                                                                         |
| maxResultBytes     | string   | true         | Bytes a tool call can read, for example `2MiB`. Exceeding it fails the call.                                                    |
| slowQueryThreshold | string   | false        | Duration at which a successful query is logged as slow. Defaults to `1s`.                                                       |

## Tool fields

A tool's `queryGuard` block can only tighten its source's limits. Anything else
fails at startup.

| **field**          | **type** | **required** | **description**                                                      |
|--------------------|:--------:|:------------:|----------------------------------------------------------------------|
| queryTimeout       | string   | false        | Lower overall deadline. It must still exceed the database limit.     |
| maxRows            | integer  | false        | Lower row limit.                                                     |
| maxResultBytes     | string   | false        | Lower byte limit.                                                    |
| slowQueryThreshold | string   | false        | Slow query threshold for this tool.                                  |
| fallbackTool       | string   | false        | Tool to suggest to callers in error responses. It must exist.        |

## Behavior

Results are never truncated: a call that exceeds a limit fails and returns no
rows. Reading stops as soon as the limit is reached, and the query is stopped on
the server.

Database error text is not returned to callers on guarded sources, because it
can quote stored or parameter values. Callers receive a stable error with the
database's own error code instead:

```json
{
  "errorCode": "QUERY_TIMEOUT",
  "retryable": false,
  "queryId": "8bb3121f99987968",
  "message": "The query exceeded its execution budget. Narrow the scope or contact an administrator for analysis.",
  "dbErrorCode": "3024",
  "fallbackTool": "customer_summary"
}
```

| **errorCode**              | **meaning**                                                                           |
|----------------------------|---------------------------------------------------------------------------------------|
| `QUERY_TIMEOUT`            | The call exceeded `statementTimeout` or `queryTimeout`.                               |
| `QUERY_LOCK_TIMEOUT`       | The query exceeded `lockTimeout`.                                                     |
| `QUERY_BUSY`               | No slot became available.                                                             |
| `QUERY_CANCELED`           | The client went away.                                                                 |
| `QUERY_ROW_LIMIT_EXCEEDED` | The result exceeded `maxRows`.                                                        |
| `QUERY_RESULT_TOO_LARGE`   | The result exceeded `maxResultBytes`.                                                 |
| `QUERY_FAILED`             | The database returned another error. See `dbErrorCode`.                               |
| `QUERY_GUARD_UNENFORCED`   | The tool reached the database without using the budget, so its result was withheld.   |
| `QUERY_MASKING_FAILED`     | [Result masking](./result-masking.md) could not vouch for a column, so the result was withheld. |

Agents should treat all of these as a signal to stop or narrow the request. None
of them are retryable.

## How queries are stopped

| **source**    | **server limit**               | **when the client goes away**                  |
|---------------|--------------------------------|------------------------------------------------|
| `mysql`       | `max_execution_time`           | Toolbox sends `KILL QUERY` to the session.     |
| `postgres`    | `statement_timeout`            | The driver sends a cancel request.             |
| `cockroachdb` | `statement_timeout`            | The driver sends a cancel request.             |

On MySQL, `max_execution_time` only covers read-only `SELECT` statements. Other
statements are stopped by `KILL QUERY` when `queryTimeout` expires, so
`queryTimeout` is their effective limit. This has three consequences:

- Each query takes one extra round trip to read `CONNECTION_ID()`.
- A proxy that multiplexes sessions, placed between Toolbox and MySQL, can defeat
  `KILL QUERY`, because the id may not identify the session on the server.
- MariaDB does not have `max_execution_time`, so connecting fails.

On CockroachDB, `queryGuard` replaces `maxRowLimit`: instead of a `LIMIT` being
added to the statement, exceeding `maxRows` fails the call.

## Monitoring

Toolbox writes a `query guard event` log entry for every failed or slow call,
and at debug level for every other call. It contains the query fingerprint,
timings, row and byte counts, error codes and a request id, but never SQL text,
parameter values or results. Use `--logging-format json` to get field names; the
standard format prints the values only.

Query Guard also exports these OpenTelemetry metrics:

| **metric**                     | **description**                                        |
|--------------------------------|--------------------------------------------------------|
| `toolbox.query.outcomes`       | Guarded calls by outcome.                              |
| `toolbox.query.active`         | Guarded calls running now.                             |
| `toolbox.query.queue.duration` | Time spent waiting for a slot.                         |
| `toolbox.query.rows`           | Rows read per call.                                    |
| `toolbox.query.result.size`    | Bytes read per call.                                   |
| `toolbox.query.events.dropped` | Log events dropped because the backlog was full.       |
