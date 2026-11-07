---
title: "Result Masking"
type: docs
weight: 4
description: >
  Mask sensitive columns in tool results, and refuse any column nobody reviewed.
---

Result masking rewrites the columns of a tool's result before it leaves Toolbox.
Add a `resultMasking` block to a tool and every returned column must either be
masked or be explicitly allowed. A column nobody listed fails the whole call, so
adding a column to a table or using `SELECT *` cannot expose data by accident.

It is supported by `mysql-sql`, `postgres-sql` and `cockroachdb-sql` tools. The
tool's source must have [Query Guard](./query-guard.md) limits, because database
errors can quote row values and Query Guard is what keeps them from reaching the
caller.

{{< notice warning >}}
Masking matches column names, so it only protects a statement callers cannot
change. It is refused on tools that use `templateParameters` and on free-form
tools such as `mysql-execute-sql`, where `SELECT phone AS x` would rename any
column. Keep those tools away from business agents.
{{< /notice >}}

## Example

```yaml
kind: tool
name: query_customers
type: mysql-sql
source: shop
description: Look up customers by level. Results are masked.
statement: |
  SELECT id, customer_name, phone, email
  FROM customers
  WHERE level = ?
  ORDER BY id
  LIMIT 200
parameters:
  - name: level
    type: string
    description: Customer level
resultMasking:
  allow:
    - id
  rules:
    - column: customer_name
      action: keep
      first: 1
    - column: phone
      action: keep
      first: 3
      last: 4
      replacement: "****"
    - column: email
      action: email
```

| **column** | **value**               | **masked**         |
|------------|-------------------------|--------------------|
| `customer_name` | `张三丰`            | `张***`            |
| `phone`    | `13812345678`           | `138****5678`      |
| `email`    | `zhang.san@example.com` | `z***@example.com` |

## Fields

| **field**            | **type** | **required** | **description**                                                     |
|----------------------|:--------:|:------------:|---------------------------------------------------------------------|
| allow                | []string |    false     | Columns returned unchanged.                                         |
| rules                | []rule   |    false     | Columns to mask.                                                    |
| rules[].column       | string   |     true     | Name of the column as returned, including any alias.                |
| rules[].action       | string   |     true     | `redact`, `keep` or `email`.                                        |
| rules[].first        | integer  |    false     | Characters kept from the start. Only for `keep`.                    |
| rules[].last         | integer  |    false     | Characters kept from the end. Only for `keep`.                      |
| rules[].replacement  | string   |    false     | Text put in place of hidden characters. Defaults to `***`.          |

## Actions

| **action** | **result**                                                                                                   |
|------------|--------------------------------------------------------------------------------------------------------------|
| `redact`   | Replaces the whole value, of any type, with the replacement text.                                            |
| `keep`     | Keeps `first` and `last` characters. A value too short to hide anything is replaced entirely. Strings only.  |
| `email`    | Keeps the first character and the domain. Text that is not an address is replaced entirely. Strings only.    |

`NULL` stays `NULL` and an empty string stays empty. Lengths count characters,
not bytes, so multibyte text is never cut in half.

To use `keep` on a numeric column, cast it in the statement, or use `redact`.

## What fails the call

A failed check returns no rows, not even the ones that passed. The caller gets
`QUERY_MASKING_FAILED`, with a message that names the column and never a value.

- A returned column is in neither `allow` nor `rules`.
- A configured column is missing from the result, which usually means the
  statement or an alias changed.
- A `keep` or `email` column holds something other than a string.
- The tool returned a result that is not a list of rows.

A result with no rows passes, as there is nothing to check.

The following configurations stop Toolbox from starting:

- `resultMasking` on a tool type that does not support it, or with
  `templateParameters`.
- A source without `queryGuard` limits.
- The same column in `allow` and `rules`, or in `rules` twice.
- An unknown action, or a `keep` rule that keeps nothing.

## Strict governance

Start the server with `--strict-governance` to refuse any configuration that
leaves a tool ungoverned. The check also applies when the configuration is
reloaded. Every tool must meet both conditions:

- Its source has `queryGuard` limits. This excludes tools with no source.
- It has `resultMasking`, or `authRequired` or `scopesRequired`, or it never
  returns row data, as with [`mysql-explain`](../../../integrations/mysql/tools/mysql-explain.md).

Authentication is the way through for tools that must return original values.
Use it to separate them from the masked tools that business agents call. Do not
add a parameter that turns masking off: a model could simply set it.

## Limits

Masking controls what a result says, not what a caller can learn from it. A
filter parameter can confirm that a value exists, and aggregates stay exact.
Nested values are not inspected: to hide a JSON column, use `redact`.
