# roca-firstmate

La Roca plugin that mirrors one or more firstmate homes into its own federated SQLite database and routes deterministic wakeups to attached agent seats.

This is the public normative full-size plugin example: `plugin.json`, a custodial federated SQLite database, Scribe's versioned Markdown mirror, an on-demand AXI chart, Nerve v1, and the Dresser operating skill. La Roca's minimal plugin quickstart lives in the pinned v1.68 [docs/plugins.md](https://github.com/thellmwhisperer/la-roca/blob/v1.68.0/docs/plugins.md).

La Roca the product stays untouched. Firstmate is mirrored, not modified. Tests use only the fabricated homes under `testdata/homes/`; public code and text contain no live home data or install details.

## Frozen contract

- `firstmate.db` is an external federated database. Its payload and first-run contract are defined in [Install and verify](#install-and-verify).
- Firstmate remains the writer of its Markdown. Scribe mirrors it; `tasks-axi` remains the backlog query layer.
- Every rewrite becomes a new version row, with `is_current = 1` on the latest file.
- Distiller is deleted. The chart is an on-demand get-or-create with a database watermark.
- Nerve is deterministic and inference-free. Destinations are `captain`, `companion`, and `machine`; mobile is later.
- There is no default daemon and no KeepAlive service. `attach` and `follow` are foreground subscriptions. `tick` is an ephemeral cron process. `watch` is the session-owned ear: a parent that owns stdin/stdout (no port, no pid file) can raise it as a child that dies when the session ends. Seat leases keep a single holder per home.
- `state/` telemetry is outside v1. The reserved later tables remain free.
- Vector retrieval is embeddings-only over the five family `content` columns (home prose plus task text). The plugin never declares ingest.

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

The home-aware verbs accept repeated `--home PATH --home-id ID` positional pairs to cover more than one fabricated home in a single process; unbalanced flags exit 2. `--label` and `--kind` are also repeatable alongside those pairs. A single `--kind` broadcasts to every pair, and an omitted kind defaults to `primary`. Flags only, no config file. All homes and concurrent session-owned watchers coordinate through one `firstmate.db`. Existing single-home commands keep their byte-identical output.

```sh
roca-firstmate scribe --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca-firstmate watch --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca-firstmate watch \
  --home '<fabricated primary home>' --home-id northwind-harbor --kind primary \
  --home '<fabricated second-mate home>' --home-id skiff-secondmate --kind secondmate \
  --db firstmate.db
roca-firstmate place --dir '<plugin directory>'
```

`watch` opens one recursive FSEvents subscription per home on macOS when cgo is available, and polling elsewhere. On rise it fingerprint-sweeps first so writes made while nobody listened are absorbed, then it stays on live events. Concurrent `watch` processes compete for the existing `seats` lease of `watch-<home-id>` (destination `machine`): the holder watches, the others stand down, and a released or expired lease is inherited on the next retry. `scribe` ingests each home in sequence. Nerve adds ingest-on-read: `attach`, `chart`, `follow`, and `tick` run Scribe's fingerprint sweep for each supplied pair before answering. For `chart` and `follow`, unpaired `--home-id` values filter already-registered homes, while omitting the filter reads every registered home. Supplying paired `--home PATH --home-id ID` values refreshes those homes but does not filter output.

Watch telemetry is JSONL next to `firstmate.db` (`logs/watch-YYYY-MM-DD.jsonl`), never a database table. Lines record raise, lease-acquired, lease-lost, sweep counts, and crash-retry. A crash inside the child is logged and retried with backoff; it does not require a daemon.

`place` only copies this independently versioned executable into the plugin directory (default: the directory of `ROCA_FIRSTMATE_DB`) as `roca-firstmate`, so a session parent resolves it from that directory instead of PATH luck. [Install and verify](#install-and-verify) owns the payload and first-run database contract. This plugin does not add unknown manifest fields: current La Roca rejects them. The watch child is ready for a generic session-companion declaration any plugin could name; that kernel seam, if added, must not mention firstmate.

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
2. Registers one workspace seat per supplied home. Only a SHA-256 fingerprint and opaque default label are stored, never the path; `--label` opts into a human-readable label.
3. Prints the chart get-or-create, including its database watermark, and the latest mirrored handoff.
4. Drains pending wakeups for the selected destination.
5. Subscribes to database/WAL changes in the same foreground gesture.

Attaching is subscribing. There is no init ceremony.

## Primitive wakeups

`follow` observes `firstmate.db-wal`. Native macOS builds use FSEvents; portable builds use a polling fallback.

```sh
roca-firstmate follow --destination companion
roca-firstmate follow --destination companion --home-id skiff-secondmate
```

Across multiple homes, `follow` interleaves wakeups in global generation order and includes `home_id` in every JSON line.

For each delivery attempt, follow writes one JSON line to stdout before committing `handled = 1` and `handled_generation = generation`. Output failure rolls the claim back. Process death or commit failure after a successful write can emit the same line again, so delivery across the stdout/SQLite boundary is at-least-once, never cross-system exactly-once. Adapters deduplicate retries by `(destination, generation)`.

Direct `follow` derives and heartbeats one opaque seat per selected home from the current workspace unless `--workspace` or `--seat-id` is supplied. An explicit `--seat-id` is valid only when one home is selected. During an adapter handoff, `attach` and direct `follow` are mutually exclusive subscription owners for the same workspace and destination: stop one before starting the other so they cannot race to consume the queue.

Dresser documents three adapter recipes and their intended latency:

- Native wake APIs: milliseconds. Claude Monitor or Claude `asyncRewake` as separate alternatives, OpenCode `promptAsync` without `--pure`, Pi TypeScript extensions, Grok `background-notify`, and probable Hermes hooks after verification.
- Injection: seconds. Codex CLI and Cursor CLI pipe `follow` to `fm-send`.
- Passive desktop sync: on open. Attach, consume the chart/handoff and pending lines, then close with the app.

## Ephemeral tick

Cron may run a process that is born, reconciles fingerprints, advances the silence clock, drains orphan wakeups, and dies:

```sh
roca-firstmate tick --silence-after 5m
```

Seats keep leases in SQLite. The silence clock records durable generations per expired seat, and each silence wakeup keeps that seat's `home_id`. A wakeup is orphaned when its destination has no unexpired seat lease; tick delivers it with the same one-line and handled-generation contract as follow. No tick state survives in memory.

## Chart and SQL history

`chart` is a bounded AXI get-or-create: generate, serve cached, or regenerate when its watermark moves.

```sh
roca-firstmate chart --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca-firstmate chart --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db --json
roca-firstmate chart --db firstmate.db --home-id skiff-secondmate
```

The cache lives in `chart_cache`, not Markdown. Default output is bounded TOON with `help[]`; `--json` returns the complete envelope. Use gated SQL for history:

```sh
roca exec 'SELECT home_id, relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions WHERE is_current = 1 ORDER BY home_id, relative_path'
roca exec 'SELECT id, home_id, destination, generation, handled_generation, kind, created_at FROM plugin_roca_firstmate.wakeups ORDER BY generation'
```

Schema source: [`schema/schema.sql`](schema/schema.sql). The visible tables are the five `*_versions` families, `homes`, `tasks`, `ingest_file_state`, `seats`, `wakeups`, and `chart_cache`. La Roca hides `plugin_schema` bookkeeping. [`plugin.json`](plugin.json) owns the semantic and vector declarations; `schema/package_test.go` enforces their parity and selectivity.

## Install and verify

Obtain the independently versioned executable, then install the data-only package:

```sh
go install github.com/thellmwhisperer/roca-firstmate/cmd/roca-firstmate@main
export ROCA_FIRSTMATE_DB="$(
  roca plugin install thellmwhisperer/roca-firstmate --yes --json |
  jq -r '.directory + "/firstmate.db"'
)"
roca-firstmate place
```

The experimental plugin surface must be enabled in La Roca. `--yes` accepts the displayed DATA-ONLY risk for non-interactive JSON installation. The installer verifies exactly `plugin.json`; no database file is committed or shipped. The first database-backed verb (`attach`, `chart`, `follow`, `tick`, `scribe`, or `watch`) creates the empty `firstmate.db` at that path from the embedded `schema/schema.sql` with the current `plugin_schema` marker; a second run is idempotent. Because the manifest declares `custody: true`, uninstall archives the database instead of deleting operator-owned mirror history.

Do not install this repository onto a live captain La Roca while developing or testing it. Prove the exact payload locally:

```sh
make check
```

After changing `plugin.json`, refresh checksums:

```sh
make sync-db
```

## Layout

```text
plugin.json              # identity, database, semantic, and vector declarations
schema/schema.sql        # source of truth for database-backed first-run creation
cmd/roca-firstmate/      # attach, follow, tick, chart, place, and Scribe verbs
internal/nerve/          # WAL subscription, seats, watch leases, silence clock, orphan drain
internal/scribe/         # total versioned Markdown mirror
internal/watch/          # Scribe FSEvents and polling backends
internal/watchlog/       # JSONL telemetry for the session-owned watcher
skills/dresser/SKILL.md  # companion operating skill and adapter recipes
testdata/homes/          # fabricated homes only
```

Mobile routing, `state/` telemetry tables, and an optional resident daemon remain additive later work. Session-owned `watch` is not a daemon. None blocks Nerve v1. Do not add Distiller or edit the La Roca product repository from this example.
