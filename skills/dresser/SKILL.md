---
name: dresser
description: >
  Operating skill for a roca-firstmate companion. Attach first: ingest the
  mirror, register workspace seats, print chart and handoff, and subscribe.
  Then route deterministic wakeup lines with the native, injection, or passive
  recipe supported by the current agent surface.
---

# Dresser

You are a companion sitting on `firstmate.db`, not firstmate. Firstmate stays the engine room. All commands here are deterministic AXI; none asks a model to classify, summarize, or route.

## 1. Attach first

Set the local inputs, then attach from the workspace the thinker occupies:

```sh
export ROCA_FIRSTMATE_DB='<plugin directory>/firstmate.db'
export FIRSTMATE_HOME='<firstmate home>'
export FIRSTMATE_HOME_ID='local-primary'
roca-firstmate attach
```

This is the complete default gesture. It fingerprint-sweeps Markdown and ingests changes before answering, registers one opaque workspace seat per supplied home without storing the workspace path, prints the chart get-or-create plus the latest handoff, includes the chart's database watermark, drains pending `companion` wakeups, and follows the database WAL. Seat labels stay opaque unless the operator explicitly supplies `--label`. Attaching is subscribing. No init, resident daemon, or KeepAlive service is implied.

Every plugin verb supplied with paired `--home PATH --home-id ID` values performs the fingerprint sweep before it answers. `roca-firstmate chart` is still available when only the current chart is needed. Use `--json` for its complete envelope. [`README.md`](../../README.md) owns the complete multi-home and filter contract.

## 2. Wakeups follow: choose one latency recipe

`roca-firstmate follow --destination companion` derives, registers, and heartbeats one opaque seat per selected home for the current workspace, then listens to `firstmate.db-wal`. Repeat `--home PATH --home-id ID` when one follow process refreshes more than one home; pass unpaired `--home-id` to follow one registered home. Each delivery attempt writes the README's home-tagged JSON line before Nerve commits the handled generation. If the process dies or the commit fails after stdout accepts the line, that `(destination, generation)` can appear again: delivery across stdout and SQLite is at-least-once, never cross-system exactly-once. The adapter must deduplicate by destination plus generation before carrying the line into the harness wake mechanism.

`attach` and direct `follow` are mutually exclusive subscription owners during adapter handoff. Stop attach before starting a follow adapter for the same workspace and destination, and stop follow before returning ownership to attach, so two processes never race to consume that destination.

### Native: milliseconds

Use a native asynchronous wake surface when the harness has one:

- Claude Monitor: native millisecond wake delivery.
- Claude `asyncRewake`: a separate native millisecond alternative.
- OpenCode: `client.session.promptAsync`; never launch this recipe with `--pure`, because pure mode removes the plugin surface it needs.
- Pi: a project-local TypeScript extension that waits for the follow line and prompts the active session.
- Grok: its `background-notify` cycle.
- Hermes: hooks are probable, not yet a guaranteed adapter; verify the hook surface before relying on it.

The adapter owns re-arming. Nerve owns only the deterministic line and generation acknowledgement.

### Injection: seconds

Codex CLI and Cursor CLI do not need a daemon. Pipe the foreground follow stream to firstmate's existing injection door:

```sh
roca-firstmate follow --destination companion | fm-send
```

The harness receives the wakeup at its next safe injection point, normally seconds rather than milliseconds. Do not add an inference step between `follow` and `fm-send`.

### Passive: on open

For desktop apps without a background wake API, run the attach gesture when the workspace opens. Consume its chart, latest handoff, and pending wakeup lines; close the subscription when the app closes. Freshness and delivery happen on open, with no promise of background latency while the app is absent.

The three supported latency classes are therefore: native in milliseconds, injection in seconds, and passive on open.

## 3. Ephemeral recovery tick

Schedule a process that is born, reconciles fingerprints, advances the persisted silence clock, drains wakeups whose destination has no live seat lease, prints its bounded result, and dies:

```sh
roca-firstmate tick --silence-after 5m
```

Tick keeps no in-memory state. Seat leases, silence generations, wakeup generations, and handled generations live in `firstmate.db`. A resident daemon subcommand may be added later, but it is not part of the default v1 recipe.

`watch` is the session-owned ear, not a daemon: a parent that owns stdin can raise `roca-firstmate watch` as a child that dies when the session ends. Place the independently versioned executable in the plugin directory with `roca-firstmate place` so the parent resolves it there. Concurrent sessions share the existing `seats` lease; only the holder listens. Telemetry is JSONL next to `firstmate.db`, never a new table.

## 4. History through SQL

The chart is a bounded window. Pull history with gated SQL against alias `plugin_roca_firstmate`:

```sh
roca exec 'SELECT home_id, relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions WHERE is_current = 1 ORDER BY home_id, relative_path'
roca exec 'SELECT id, home_id, destination, kind, generation, handled_generation, created_at FROM plugin_roca_firstmate.wakeups ORDER BY generation'
```

Schema: `schema/schema.sql`. Do not guess tables. Do not `LIKE '%term%'`.

## 5. Firstmate's conversation door

Command firstmate only through its single conversation door: the captain talks to firstmate; you do not. Do not spawn crewmates, do not address the captain as firstmate, and do not become the fleet supervisor. If work needs the engine room, say so in this companion chat so the captain can take it to firstmate.
