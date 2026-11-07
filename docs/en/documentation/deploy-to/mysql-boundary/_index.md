---
title: "MySQL Deployment Boundary"
type: docs
weight: 4
description: >
  Keep agents from reaching MySQL except through Toolbox, and test that they can't.
---

[Query Guard](../../configuration/security/query-guard.md) and
[Result Masking](../../configuration/security/result-masking.md) only apply to
traffic that goes through Toolbox. An agent that can open a connection to MySQL
skips both. Telling the model not to is not a control, and neither is removing a
driver: anything that can run code can bring its own.

The boundary has four layers, and each one holds on its own.

| **Layer**   | **Control**                                                          | **Stops**                                  |
|-------------|----------------------------------------------------------------------|--------------------------------------------|
| Credentials | Only Toolbox holds the database password.                            | An agent logging in with what it can read. |
| Network     | Agents can reach Toolbox. Only Toolbox can reach MySQL.              | An agent connecting to port 3306.          |
| Database    | A read-only account accepted from the Toolbox address alone.         | Use of a leaked password from elsewhere.   |
| Runtime     | Agents run without Docker sockets, cloud credentials or open egress. | An agent going around the other layers.    |

## Try it

The repository has a runnable stack, with a script that attempts each bypass from
inside an agent container that has a shell. It needs Docker with Compose.

```bash
./deploy/mysql-boundary/verify.sh
```

The script exits non-zero if any control fails. It covers:

- The agent gets masked rows through Toolbox.
- The agent cannot resolve or connect to MySQL, or reach the internet.
- No password is in the agent's environment or files, or in Toolbox's container
  metadata.
- The correct password is rejected from any address but Toolbox's.
- Toolbox refuses to start with an account that can write, or log in from anywhere.
- With Toolbox stopped, the agent fails and no other path to MySQL exists.

## Database account

Create one account for Toolbox, limited to its address and to what its tools read:

```sql
CREATE USER 'toolbox_ro'@'10.0.1.10' IDENTIFIED BY '...';
GRANT SELECT ON shop.* TO 'toolbox_ro'@'10.0.1.10';
```

Then have Toolbox verify it. With `identityCheck: read-only` on the source, Toolbox
refuses to start, or to apply a reloaded configuration, unless the account:

- Is not `'user'@'%'`.
- Has only `USAGE`, `SELECT` and `SHOW VIEW`, with no `GRANT OPTION`.
- Has no `SELECT` on `*.*`, `mysql` or `sys`. Global `SELECT` includes password
  hashes.
- Has no roles or partial revokes, which `SHOW GRANTS` cannot fully describe.

`PROCESS` is refused too, as it exposes the SQL of other sessions. Tools that need
it, such as `mysql-list-active-queries`, belong on a separate source for
administrators, without this check.

## Network

Translate the two networks in `compose.yaml` to your platform. On Kubernetes, deny
by default and allow only Toolbox to reach MySQL. This example is a starting point
and is not exercised by `verify.sh`, which uses Docker networks:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: mysql-only-from-toolbox
spec:
  podSelector:
    matchLabels:
      app: mysql
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app: toolbox
      ports:
        - port: 3306
```

Network policies need a network plugin that enforces them, so confirm with a real
connection attempt from an agent pod. Read replicas need the same rules, or they
become the way around this one.

## Runtime

Treat an agent that can run code as untrusted. Do not give it a Docker socket,
host directories, cloud credentials or a general network proxy, and do not copy
Toolbox's configuration into its workspace.

## Detection

Check for logins by anything other than Toolbox. This needs no extra software, and
any row other than your Toolbox account deserves a look:

```sql
SELECT USER, HOST, TOTAL_CONNECTIONS, CURRENT_CONNECTIONS
FROM performance_schema.accounts
WHERE USER IS NOT NULL
ORDER BY TOTAL_CONNECTIONS DESC;
```

For alerts, enable MySQL's audit log and connect it to your monitoring. Toolbox
cannot see a connection that does not go through it.

If the password leaks, rotate it, then review login history for use from other
addresses.
