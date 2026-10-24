# Operating Nube as an AI agent

This guide is for AI agents (Claude Code, Claude Desktop, agents running inside Nube, scripts)
that operate a Nube cluster, and for the people who set them up.

## Connect

Give the agent its own token with the `agent` role, limited to the namespaces it should touch:

```bash
nube token create claude --role agent -n staging
```

**MCP (recommended).** Nube is an MCP server. Over stdio, through the CLI's saved login:

```bash
claude mcp add nube -- nube mcp
```

Over HTTP: `POST https://SERVER:7070/api/v1/mcp` with `Authorization: Bearer TOKEN`.

**CLI.** Every command takes `-o json`, and then errors are JSON on stderr too.
Set `NUBE_OUTPUT=json` to make that the default.

## Tools (MCP)

| Tool | Use it to |
|---|---|
| `cluster_overview` | start: nodes, workload health, problems |
| `diagnose` | find what is broken, with evidence (last log lines of crashing containers) and a fix per finding |
| `list_resources`, `get_resource` | read objects (secret values are masked) |
| `get_logs`, `get_events` | look closer |
| `top_workloads` | requested vs. recommended CPU/memory per workload |
| `explain` | fields and an example of a kind, before writing YAML |
| `apply_manifest` | create or update; `dry_run: true` returns the field-by-field diff without changing anything |
| `scale`, `set_image`, `rollout` | everyday changes (a failing deployment rollout rolls back by itself) |
| `delete_resource` | delete (held for approval with the agent role) |
| `list_approvals` | follow requests waiting for a human |
| `run_agent` | hand a task to one of the cluster's own agents |

## Rules of the road

1. **Look before you change.** `diagnose` first, then `explain` the kind, then `apply_manifest` with `dry_run`.
2. **Approvals are normal.** With the `agent` role, deletes, volume moves, scaling to zero and changes to
   Secrets, Volumes and NetworkPolicies return `code: approval_required` with an `approval` id. They are
   not errors on your side: tell the user the id and what you asked for; an admin runs
   `nube approve ID` (or uses the dashboard), and the call then runs exactly as you sent it.
   Check the result with `list_approvals` (status `Executed` and the response, or `Rejected` with a note).
3. **Branch on codes, not on text.** API errors are `{"error", "code", "hint"}`; CLI exit codes:

   | Exit | Code | Meaning |
   |---|---|---|
   | 0 | | success |
   | 1 | `error` | other error |
   | 2 | | usage |
   | 3 | `not_found` | no such object |
   | 4 | `forbidden` / `unauthenticated` | role or namespace does not allow it |
   | 5 | `invalid` | fix the manifest or request (the hint says how) |
   | 6 | `conflict` | read again and retry |
   | 7 | `unavailable` | leader election or startup: retry in a few seconds |
   | 8 | `approval_required` | a human must approve; the approval id is in the output |
4. **Secret values are never shown to the agent role**, and every change is in the audit log
   (`nube audit`, admins only).

## Nube's own agents

Nube also runs AI agents itself (`kind: Agent`): a model (Claude, or any OpenAI-compatible server),
instructions, and tools: HTTP services, MCP servers, a sandbox container for shell commands, other
agents, and Nube's own tools (`nube: {role: view}`). Each task is an `AgentRun` in its own pod, with a
trace (`nube trace RUN`), budgets, session memory and approvals for tools marked `requireApproval`.
See `examples/agents/` and `nube explain agent`.
