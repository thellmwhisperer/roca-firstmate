# roca-firstmate

La Roca plugin that mirrors a firstmate home into its own federated, queryable SQLite database.

This repository is the public, worked full-size example of a La Roca plugin: a `plugin.json` manifest with a semantic fragment, a custodial `firstmate.db` whose schema is a versioned mirror, Scribe's incremental writer and continuous watcher, an on-demand AXI chart command, and the Dresser operating skill. The five-minute walk (three files, one install, one query) lives in the pinned La Roca v1.64 [docs/plugins.md](https://github.com/thellmwhisperer/la-roca/blob/v1.64.0/docs/plugins.md). Start there. Grow from that shape rather than inventing a packaging of your own.

La Roca the product is untouched. Firstmate is not modified. This plugin writes `firstmate.db` as an external federated database.

## Code public, data local

The plugin code is generic: read a firstmate home, mirror it into `firstmate.db`. Mirrored fleet records never leave the operator's machine. The public package ships an empty schema, not anyone's home.

Tests use fabricated homes only (see `testdata/homes/northwind-harbor`). They do not copy real homes, real paths, live task ids, or live-install details.

## Frozen decisions

- `firstmate.db` is an external federated database. No fleet features land in the La Roca repo.
- Firstmate stays the writer of its files. This plugin mirrors those files; it does not migrate Firstmate off them.
- The backlog does not migrate as a query layer. `tasks-axi` stays that layer. Scribe mirrors `backlog.md` and per-task Markdown only for durable history and cross-references.
- Scribe ingests every Markdown file under the configured home's `data/` tree. Selectivity belongs to vector search, never ingest.
- Distiller is deleted: the chart is an on-demand AXI get-or-create, never a scheduled file, and nothing runs when a session opens.

## Five inventory families

Scribe classifies every Markdown file under a home's `data/` into five inventory families:

1. **Startup working set:** `captain.md`, `captain-shared.md`, `learnings.md`, `projects.md`, `secondmates.md`.
2. **Archives:** `captain-archive.md`, `memory-archive.md`, `note-archive.md`.
3. **Task state:** `backlog.md`, `done-archive.md` (cross-references only).
4. **Root operational docs:** every other Markdown file at the `data/` root; dated filenames encode type in their prefix.
5. **Per-task artifacts** under `data/<task-id>/` (`brief.md`, `acceptance.md`, and the rest), keyed to a mirrored task id.

These families are classification, not selection: Scribe keeps all root Markdown as operational docs and all nested Markdown as task artifacts, including previously unseen names and deeper artifact paths.

`state/` telemetry (`*.status`, `*.meta`, firstmate's wake queue file) is out of v1. The schema leaves `status_events`, `task_meta`, and `wake_queue` free so a later cursor can add them without rewriting the five families. `wakeups` is Nerve's destination table and ships now.

## Versioned mirror

Working-set files are rewrite-in-place on disk. Every observed rewrite is a new row. The current file is the row with `is_current = 1`. Without that, the mirror would inherit the same loss `/stow` already has.

The same versioning contract applies to the other four families, so a later rewrite still keeps history.

## Scribe: total backfill and continuous ingest

The data-package installer does not execute Scribe. The first `roca-firstmate watch` launch starts the watcher, runs the total fingerprinted backfill, then handles one changed Markdown path per event:

```sh
roca-firstmate scribe --home /path/to/fabricated-or-local-firstmate --home-id local-primary --db firstmate.db
roca-firstmate watch --home /path/to/fabricated-or-local-firstmate --home-id local-primary --db firstmate.db
```

`watch` uses native FSEvents recursively on macOS when cgo is available and falls back to polling elsewhere or if native startup fails. `make build` preserves that native backend; `make build-static` deliberately produces the polling-fallback binary. The one-shot `scribe` command is the initial backfill and the command a nightly cron may invoke as a fingerprint safety net; cron is not the primary ingest mechanism.

The write path is deterministic and inference-free:

```text
Markdown file event -> version row -> SQL trigger -> companion wakeup
```

The row and its wakeup commit in the same transaction. An unchanged content hash creates neither a duplicate version nor a wakeup. Cursor identities are home-relative; absolute home paths are never persisted in `firstmate.db`.

Scribe directly consumes La Roca's public `parsers`, `ingestprovenance`, and `incrementality` packages. The external-module contract test also pins the public `corpuswriter` facade; Markdown mirroring does not fabricate conversation rows just to exercise it.

Schema source: [`schema/schema.sql`](schema/schema.sql). SQL against the attached alias looks like:

```sql
SELECT home_id, relative_path, version, observed_at
FROM plugin_roca_firstmate.working_set_versions
WHERE is_current = 1
ORDER BY home_id, relative_path
```

## Install

The verified installer payload remains data-only: exactly `plugin.json` and `firstmate.db`. Obtain the independently versioned Scribe executable with Go, ensure Go's bin directory is on `PATH`, and have `jq` available to read the install result, then install the database package:

```sh
go install github.com/thellmwhisperer/roca-firstmate/cmd/roca-firstmate@main
export ROCA_FIRSTMATE_DB="$(
  roca plugin install thellmwhisperer/roca-firstmate --yes --json |
  jq -r '.directory + "/firstmate.db"'
)"
```

The third-party plugin surface is experimental and default-off, so enable `features.plugins` in La Roca before the install command. `--yes` explicitly accepts the displayed DATA-ONLY risk in non-interactive JSON mode; `jq` reads the documented `directory` result and appends the package's `firstmate.db` filename. Start the long-lived watcher with that derived path; this first Scribe launch, not plugin installation, performs the required initial backfill:

```sh
roca-firstmate watch --home '<firstmate home>' --home-id local-primary
```

The executable can also be built from a public checkout with `make build` and run as `.tmp/roca-firstmate`. A nightly safety-net job may run `roca-firstmate scribe` with the same flags, but `watch` remains the primary continuous mechanism. The installer verifies `checksums.txt`, shows a DATA-ONLY consent screen, and copies the package under the operator's plugin directory. Because the database declares `custody: true`, uninstall archives it instead of deleting it.

This repository's tests and agents do not install the plugin onto a live captain La Roca. Prove the payload with `make check`; use `make sync-db` only to rebuild the database and checksums after changing their sources.

Prove the empty schema with gated SQL (zero rows until a later Scribe writes a home):

```sh
roca exec 'SELECT COUNT(*) AS homes FROM plugin_roca_firstmate.homes'
```

The visible tables are the five `*_versions` tables, `homes`, `tasks`, `ingest_file_state`, `wakeups`, and `chart_cache`. La Roca hides `plugin_schema` as bookkeeping.

## Chart command

`roca-firstmate chart` is a classic AXI verb: idempotent get-or-create, inference-free.

```sh
go run ./cmd/roca-firstmate chart --db firstmate.db
go run ./cmd/roca-firstmate chart --db firstmate.db --json
```

If the chart does not exist, it is generated from the database and shown. If it exists and the watermark still covers every inventory and wakeup row, the stored chart is shown. If new rows exist, it regenerates. The cache lives in `chart_cache`, not as a markdown file. Output is bounded TOON with `help[]`; `--json` is the complete envelope. Exit 0 on success, 2 on usage, 1 on error.

Dresser (`skills/dresser/SKILL.md`) teaches any agent to run that command first, pull history through `roca exec`, and command firstmate only through its single conversation door. Dresser is a skill, not a hook.

## Layout

```text
plugin.json              # identity, database declaration, semantic fragment
firstmate.db             # empty schema, operator-owned once installed
checksums.txt            # SHA-256 of plugin.json and firstmate.db
schema/schema.sql        # source of truth for firstmate.db
cmd/roca-firstmate/      # Scribe, watcher, and on-demand AXI chart
internal/scribe/         # total versioned Markdown mirror
internal/watch/          # FSEvents with polling fallback
skills/dresser/SKILL.md  # companion operating skill
testdata/homes/          # fabricated firstmate homes only
```

Rebuild the shipped database and checksums after a schema change:

```sh
make sync-db
```

## Later PRs

Nerve will consume and route the trigger-created `wakeups`; Scribe does not grow routing policy. `state/` telemetry remains an additive later schema. Do not add Distiller.

Do not edit the La Roca repository from this example.
