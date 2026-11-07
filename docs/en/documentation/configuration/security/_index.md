---
title: "Security"
type: docs
weight: 1
description: >
  Harden your MCP Toolbox agents and tools against prompt injection, jailbreaks,
  and sensitive data leakage.
---

This section covers how to secure MCP Toolbox deployments against common AI
security risks like prompt injection, jailbreaks, unauthorized database modifications,
and sensitive data leakage across the traffic between your users, agents, and tools:

- **[Model Armor](./model-armor.md):** Defend AI applications against prompt injection,
  jailbreaks, and sensitive data leakage by screening traffic with Google Cloud Model Armor.
- **[Query Guard](./query-guard.md):** Limit the concurrency, duration and result
  size of queries on a source.
- **[Result Masking](./result-masking.md):** Mask sensitive columns in tool results,
  and refuse any column nobody reviewed.
- **[MySQL Deployment Boundary](../../deploy-to/mysql-boundary/_index.md):** Keep
  agents from reaching MySQL except through Toolbox, and test that they can't.
- **[Read-Only Tools](./read-only.md):** Enforce deterministic read-only database
  access across custom tools and prebuilt servers using engine-level protocol locks,
  tool suppression, and MCP annotations.
