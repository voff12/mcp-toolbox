# MySQL deployment boundary

A runnable reference for keeping agents from reaching MySQL except through
Toolbox, and a script that tries to get around it from inside an agent container.

```bash
./deploy/mysql-boundary/verify.sh
```

It needs Docker with Compose. It builds Toolbox from this checkout, starts the
stack on two internal networks, runs the bypass attempts, and removes everything.

See the [guide](../../docs/en/documentation/deploy-to/mysql-boundary/_index.md)
for what each control does and how to carry it over to production. The stack is
a test fixture: its passwords are random and its data is fake.
