# roca-firstmate

La Roca plugin that mirrors a firstmate home into its own federated, queryable SQLite database.

This repository is the public, worked full-size example of a La Roca plugin: a `plugin.json` manifest with a semantic fragment, a custodial `firstmate.db` whose schema is a versioned mirror, an on-demand AXI chart command, and the Dresser operating skill. The five-minute walk (three files, one install, one query) lives in La Roca's [docs/plugins.md](https://github.com/thellmwhisperer/la-roca/blob/main/docs/plugins.md). Start there. Grow from that shape rather than inventing a packaging of your own.

La Roca the product is untouched. Firstmate is not modified. This plugin writes `firstmate.db` as an external federated database.

## Code public, data local

The plugin code is generic: read a firstmate home, mirror it into `firstmate.db`. Mirrored fleet records never leave the operator's machine. The public package ships an empty schema, not anyone's home.

Tests use fabricated homes only (see `testdata/homes/northwind-harbor`). They do not copy real homes, real paths, live task ids, or live-install details.

## Frozen decisions

- `firstmate.db` is an external federated database. No fleet features land in the La Roca repo.
- Firstmate stays the writer of its files. This plugin mirrors those files; it does not migrate Firstmate off them.
- The backlog does not migrate as a query layer. `tasks-axi` stays that layer. `backlog.md` is mirrored for cross-references only.
- Scribe (the home reader) is a later PR. Distiller is deleted: the chart is an on-demand AXI get-or-create, never a scheduled file, and nothing runs when a session opens.

## Five inventory families

Scribe's closed source list, all under a home's `data/`:

1. **Startup working set:** `captain.md`, `captain-shared.md`, `learnings.md`, `projects.md`, `secondmates.md`.
2. **Archives:** `captain-archive.md`, `memory-archive.md`, `note-archive.md`.
3. **Task state:** `backlog.md`, `done-archive.md` (cross-references only).
4. **One-shot dated operational docs** at the `data/` root. Type is encoded in the filename prefix.
5. **Per-task artifacts** under `data/<task-id>/` (`brief.md`, `acceptance.md`, and the rest), keyed to a mirrored task id.

`state/` telemetry (`*.status`, `*.meta`, firstmate's wake queue file) is out of v1. The schema leaves `status_events`, `task_meta`, and `wake_queue` free so a later cursor can add them without rewriting the five families. `wakeups` is Nerve's destination table and ships now; Nerve's writer is later.

## Versioned mirror

Working-set files are rewrite-in-place on disk. Every observed rewrite is a new row. The current file is the row with `is_current = 1`. Without that, the mirror would inherit the same loss `/stow` already has.

The same versioning contract applies to the other four families, so a later rewrite still keeps history.

Schema source: [`schema/schema.sql`](schema/schema.sql). SQL against the attached alias looks like:

```sql
SELECT home_id, relative_path, version, observed_at
FROM plugin_roca_firstmate.working_set_versions
WHERE is_current = 1
ORDER BY home_id, relative_path
```

## Install

The third-party plugin surface is experimental and default-off. Set `features.plugins = true` in the La Roca config, then:

```sh
roca plugin install thellmwhisperer/roca-firstmate
```

Or install from a local checkout of this repository. The installer verifies `checksums.txt` (exactly `plugin.json` and `firstmate.db`), shows a DATA-ONLY consent screen, and copies the package under the operator's plugin directory. Because the database declares `custody: true`, uninstall archives it instead of deleting it.

This repository's tests and agents do not install the plugin onto a live captain La Roca. Prove the payload with `make check`; use `make sync-db` only to rebuild the database and checksums after changing their sources.

Prove the empty schema with gated SQL (zero rows until a later Scribe writes a home):

```sh
roca exec 'SELECT COUNT(*) AS homes FROM plugin_roca_firstmate.homes'
```

The visible tables are the five `*_versions` tables, `homes`, `tasks`, `wakeups`, and `chart_cache`. La Roca hides `plugin_schema` as bookkeeping.

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
cmd/roca-firstmate/      # on-demand AXI chart
skills/dresser/SKILL.md  # companion operating skill
testdata/homes/          # fabricated firstmate homes only
```

Rebuild the shipped database and checksums after a schema change:

```sh
make sync-db
```

## Later PRs

This skeleton does not ingest a home yet. When La Roca publishes the public packages (parsers, provenance, incrementality, corpus writer), later PRs import them here. Scribe will cursor-read a firstmate home into these tables. Nerve will write `wakeups`. Do not add Distiller.

Do not edit the La Roca repository from this example.
