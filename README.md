# roca-firstmate

La Roca plugin that mirrors a firstmate home into its own federated SQLite database and routes deterministic wakeups to attached agent seats.

This is the public normative full-size plugin example: `plugin.json`, a custodial `firstmate.db`, Scribe's versioned Markdown mirror, an on-demand AXI chart, Nerve v1, and the Dresser operating skill. La Roca's minimal plugin quickstart lives in the pinned v1.64 [docs/plugins.md](https://github.com/thellmwhisperer/la-roca/blob/v1.64.0/docs/plugins.md).

La Roca the product stays untouched. Firstmate is mirrored, not modified. Tests use only the fabricated homes under `testdata/homes/`; public code and text contain no live home data or install details.

## Frozen contract

- `firstmate.db` is an external federated database. The installer payload is exactly `plugin.json` and `firstmate.db`.
- Firstmate remains the writer of its Markdown. Scribe mirrors it; `tasks-axi` remains the backlog query layer.
- Every rewrite becomes a new version row, with `is_current = 1` on the latest file.
- Distiller is deleted. The chart is an on-demand get-or-create with a database watermark.
- Nerve is deterministic and inference-free. Destinations are `captain`, `companion`, and `machine`; mobile is later.
- There is no default daemon and no KeepAlive service. `attach` and `follow` are foreground subscriptions. `tick` is an ephemeral cron process.
- `state/` telemetry is outside v1. The reserved later tables remain free.

## Versioned mirror

Scribe classifies every Markdown file under a configured home's `data/` into five versioned families:

1. Startup working set: `captain.md`, `captain-shared.md`, `learnings.md`, `projects.md`, and `secondmates.md`.
2. Archives: `captain-archive.md`, `memory-archive.md`, and `note-archive.md`.
3. Task state: `backlog.md` and `done-archive.md`, mirrored for cross-references only.
4. Every other root Markdown file as an operational document.
5. Every nested Markdown file as a per-task artifact keyed to its task id.

Scribe's causal boundary is a SQLite transaction:

```text
Markdown file -> version row -> SQL trigger -> companion wakeup
```

An unchanged fingerprint creates neither a duplicate version nor a wakeup. Cursor identities are home-relative; absolute home paths are never persisted.

The maintenance verbs remain available and unchanged:

```sh
roca-firstmate scribe --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca-firstmate watch --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
```

`watch` uses recursive FSEvents on macOS when cgo is available and polling elsewhere. It is optional. Nerve adds ingest-on-read: `attach`, `chart`, `follow`, and `tick` run Scribe's total fingerprint sweep before answering.

## Attach: the default gesture

Set local configuration, enter the workspace the thinker occupies, and attach:

```sh
export ROCA_FIRSTMATE_DB='<plugin directory>/firstmate.db'
export FIRSTMATE_HOME='<firstmate home>'
export FIRSTMATE_HOME_ID='local-primary'
roca-firstmate attach
```

Running `roca-firstmate` without a subcommand is equivalent. Attach:

1. Reconciles Markdown fingerprints and ingests changes.
2. Registers the workspace as a firstmate seat. Only a SHA-256 fingerprint and opaque default label are stored, never the path; `--label` opts into a human-readable label.
3. Prints the chart get-or-create, including its database watermark, and the latest mirrored handoff.
4. Drains pending wakeups for the seat destination.
5. Subscribes to database/WAL changes in the same foreground gesture.

Attaching is subscribing. There is no init ceremony.

## Primitive wakeups

`follow` observes `firstmate.db-wal`. Native macOS builds use FSEvents; portable builds use a polling fallback.

```sh
roca-firstmate follow --destination companion
```

For each delivery attempt, follow writes one JSON line to stdout before committing `handled = 1` and `handled_generation = generation`. Output failure rolls the claim back. Process death or commit failure after a successful write can emit the same line again, so delivery across the stdout/SQLite boundary is at-least-once, never cross-system exactly-once. Adapters deduplicate retries by `(destination, generation)`.

Direct `follow` derives an opaque seat from the current workspace unless `--workspace` or `--seat-id` is supplied, registers or refreshes that seat, and heartbeats its lease while connected. During an adapter handoff, `attach` and direct `follow` are mutually exclusive subscription owners for the same workspace and destination: stop one before starting the other so they cannot race to consume the queue.

Dresser documents three adapter recipes and their intended latency:

- Native wake APIs: milliseconds. Claude Monitor or Claude `asyncRewake` as separate alternatives, OpenCode `promptAsync` without `--pure`, Pi TypeScript extensions, Grok `background-notify`, and probable Hermes hooks after verification.
- Injection: seconds. Codex CLI and Cursor CLI pipe `follow` to `fm-send`.
- Passive desktop sync: on open. Attach, consume the chart/handoff and pending lines, then close with the app.

## Ephemeral tick

Cron may run a process that is born, reconciles fingerprints, advances the silence clock, drains orphan wakeups, and dies:

```sh
roca-firstmate tick --silence-after 5m
```

Seats keep leases in SQLite. The silence clock records durable generations for expired seats. A wakeup is orphaned when its destination has no unexpired seat lease; tick delivers it with the same one-line and handled-generation contract as follow. No tick state survives in memory.

## Chart and SQL history

`chart` is a bounded AXI get-or-create: generate, serve cached, or regenerate when its watermark moves.

```sh
roca-firstmate chart --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca-firstmate chart --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db --json
```

The cache lives in `chart_cache`, not Markdown. Default output is bounded TOON with `help[]`; `--json` returns the complete envelope. Use gated SQL for history:

```sh
roca exec 'SELECT home_id, relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions WHERE is_current = 1 ORDER BY home_id, relative_path'
roca exec 'SELECT id, destination, generation, handled_generation, kind, created_at FROM plugin_roca_firstmate.wakeups ORDER BY generation'
```

Schema source: [`schema/schema.sql`](schema/schema.sql). The visible tables are the five `*_versions` families, `homes`, `tasks`, `ingest_file_state`, `seats`, `wakeups`, and `chart_cache`. La Roca hides `plugin_schema` bookkeeping.

## Install and verify

Obtain the independently versioned executable, then install the data-only package:

```sh
go install github.com/thellmwhisperer/roca-firstmate/cmd/roca-firstmate@main
export ROCA_FIRSTMATE_DB="$(
  roca plugin install thellmwhisperer/roca-firstmate --yes --json |
  jq -r '.directory + "/firstmate.db"'
)"
```

The experimental plugin surface must be enabled in La Roca. `--yes` accepts the displayed DATA-ONLY risk for non-interactive JSON installation. Because the manifest declares `custody: true`, uninstall archives the database instead of deleting operator-owned mirror history.

Do not install this repository onto a live captain La Roca while developing or testing it. Prove the exact payload locally:

```sh
make check
```

After changing `schema/schema.sql` or `plugin.json`, rebuild the empty database and checksums:

```sh
make sync-db
```

## Layout

```text
plugin.json              # identity, database declaration, semantic fragment
firstmate.db             # empty schema, operator-owned after installation
schema/schema.sql        # source of truth for firstmate.db
cmd/roca-firstmate/      # attach, follow, tick, chart, and Scribe verbs
internal/nerve/          # WAL subscription, seats, silence clock, orphan drain
internal/scribe/         # total versioned Markdown mirror
internal/watch/          # Scribe FSEvents and polling backends
skills/dresser/SKILL.md  # companion operating skill and adapter recipes
testdata/homes/          # fabricated homes only
```

Mobile routing, `state/` telemetry, and an optional resident daemon remain additive later work. None blocks Nerve v1. Do not add Distiller or edit the La Roca product repository from this example.
