# roca-firstmate

La Roca plugin that mirrors one or more firstmate homes into its own federated SQLite database and routes deterministic wakeups to attached agent seats.

This repository is the public normative full-size plugin example. Use it as the shape to copy: a `plugin.json` manifest, a custodial federated SQLite database with a semantic (and embeddings-only vector) fragment, a shipped executable, package checksums, and a tag-driven GitHub release. La Roca the product stays untouched. Firstmate is mirrored, not modified. Tests use only the fabricated homes under `testdata/homes/`; public code and text contain no live home data or install details.

The minimal three-file walk (data only, no executable) lives in La Roca's [docs/plugins.md](https://github.com/thellmwhisperer/la-roca/blob/v1.74.2/docs/plugins.md). This repo is the next step: a federated plugin that also ships code.

## Plugin shape

A La Roca plugin is a verified package, not a git checkout. The installer accepts a directory, a `.tar.gz` whose files sit at the archive root, a Git URL, or `owner/repo`. This plugin's **supported** install path is the GitHub release archive. The git tree is the source that *builds* that archive. It does not contain `firstmate.db` or the executable, so `roca plugin install thellmwhisperer/roca-firstmate` (a git clone) is not an installable package.

After a successful install the plugin directory contains:

```text
plugin.json         # identity, database, semantic, and vector declarations
firstmate.db        # empty schema on first install; operator data after that
roca-firstmate      # plugin executable
checksums.txt       # SHA-256 of the three payload files above
.roca-plugin.json   # local inventory written by the installer, never shipped
```

The Dresser skill (`skills/dresser/SKILL.md`) and `schema/schema.sql` live in this repository. Schema is embedded in the executable. Dresser is a companion skill, not a package-root payload (La Roca extracts only regular files at the archive root).

### Manifest

[`plugin.json`](plugin.json) is schema 1. Required parts:

- Identity: `name` (`roca-firstmate`), `version`, `binary`.
- `binary` is `roca-firstmate` because this package ships an executable. A data-only package names `roca` (the host) and ships no binary.
- `databases`: one custodial SQLite file, `firstmate.db`, alias `plugin_roca_firstmate`, attachment `on-demand`.
- `semantic`: every visible table and its ordered columns, plus questions the model can ask.
- `vector`: embeddings-only over the five family `content` columns. No `ingest` verb.

The engine rejects unknown fields. Do not add keys La Roca does not know. Manifest/database parity and vector selectivity are enforced by `schema/package_test.go`.

### Federated database

`databases[].path` is a file **shipped in the package**. La Roca's installer requires `checksums.txt` to name that file (together with `plugin.json` and, when present, the executable). A custodial database is still a payload on first install. On update the installer preserves the installed file and does not overwrite operator data with the empty schema from the new archive.

This repository never commits a `.db` file. `make package` / `make dist` generate an empty `firstmate.db` from `schema/schema.sql` at pack time. The first database-backed verb (`attach`, `chart`, `follow`, `tick`, `scribe`, `watch`) runs `schema.Ensure` and is idempotent on that file.

`custody: true` means uninstall archives the plugin directory instead of deleting it.

### Verb dispatch

Third-party verbs do not take a `roca` CLI seat in current La Roca. Dispatch is the neighbor executable on `PATH`:

```text
roca firstmate <verb>  ->  $ROCA_PREFIX/roca-firstmate <verb>
```

(default `$ROCA_PREFIX` is `~/.local/bin`). Install copies the packaged `roca-firstmate` into the plugin directory and into that prefix. `place` remains for developers who built the binary some other way.

### Checksums

`checksums.txt` is one SHA-256 per immutable payload file, two spaces, then the filename. The installable set for this plugin is exactly:

```text
<sha256>  firstmate.db
<sha256>  plugin.json
<sha256>  roca-firstmate
```

The source-tree `checksums.txt` fingerprints `plugin.json` only (`make sync-db`). That file is not the install payload. A source whose checksums declare only `plugin.json` is refused with `checksums.txt declares [plugin.json], want exactly [firstmate.db plugin.json]`. That is the federated installer contract, not a reason to omit the database from the package.

### Packaging and release

```sh
make check      # gofmt, go vet, go test
make package    # host-platform install directory under .tmp/package
make dist       # darwin-arm64 and linux-amd64 .tar.gz under .tmp/dist
make e2e        # scratch-home install, dispatch, and release-to-release update
```

Each archive contains only package-root files. Nested paths are refused at extract time.

Push a tag `vX.Y.Z` whose `X.Y.Z` matches `plugin.json` `version`. GitHub Actions builds both platforms, checks the packaged checksums, and publishes the GitHub release. Local builds are not official releases.

Published names:

```text
roca-firstmate-vX.Y.Z-darwin-arm64.tar.gz
roca-firstmate-vX.Y.Z-linux-amd64.tar.gz
roca-firstmate-darwin-arm64.tar.gz          # same bytes, stable latest/download name
roca-firstmate-linux-amd64.tar.gz
```

Release binaries are `CGO_ENABLED=0` (portable polling watcher). Native FSEvents is a local `make build` on macOS.

### Install and update

Enable the experimental plugin surface (`features.plugins = true`), then install the archive for this machine:

```sh
# Darwin arm64
roca plugin install https://github.com/thellmwhisperer/roca-firstmate/releases/latest/download/roca-firstmate-darwin-arm64.tar.gz --yes

# Linux amd64
roca plugin install https://github.com/thellmwhisperer/roca-firstmate/releases/latest/download/roca-firstmate-linux-amd64.tar.gz --yes
```

`--yes` accepts EXECUTABLE risk (the package ships code). `--json` also needs `--yes`; JSON never consents by accident.

Prove dispatch and first use from a scratch home, never a live captain La Roca:

```sh
roca firstmate --help
roca firstmate chart --db "$HOME/.roca/plugins/roca-firstmate/firstmate.db" --help
```

Update re-resolves the recorded source. Install from the stable `latest/download` URL so `roca plugin update roca-firstmate --yes` fetches the current release. A versioned download URL stays pinned to that version. Update replaces `plugin.json` and `roca-firstmate` and preserves `firstmate.db`.

If an older install recorded a git `owner/repo` source, `plugin update` still clones the git tree and cannot verify the package. One-time cutover: `roca plugin uninstall roca-firstmate` archives the custodial directory, install from the release archive, then copy `firstmate.db` from the custody archive over the empty shipped file.

Do not install this repository onto a live captain La Roca while developing. Prove the payload with `make check` and `make e2e`.

### Versioning

`plugin.json` `version` is the package version. Git tags are `vMAJOR.MINOR.PATCH` and must match it. The release workflow refuses a tag that does not.

| Bump | When |
| --- | --- |
| MAJOR | Install contract, manifest schema, or the declared database file list changes. |
| MINOR | Compatible features: new verbs, Nerve surfaces, schema that `Ensure` can apply. |
| PATCH | Fixes, docs, packaging. No payload-file or schema-list change. |

Do not retag. Cut the next patch. `0.5.0` is the first installable GitHub release.

## Frozen contract

- `firstmate.db` is an external federated database. Its payload and first-run contract are defined above.
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
roca firstmate scribe --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca firstmate watch --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca firstmate watch \
  --home '<fabricated primary home>' --home-id northwind-harbor --kind primary \
  --home '<fabricated second-mate home>' --home-id skiff-secondmate --kind secondmate \
  --db firstmate.db
roca firstmate place --dir '<plugin directory>'
```

`watch` opens one recursive FSEvents subscription per home on macOS when cgo is available, and polling elsewhere. On rise it fingerprint-sweeps first so writes made while nobody listened are absorbed, then it stays on live events. Concurrent `watch` processes compete for the existing `seats` lease of `watch-<home-id>` (destination `machine`): the holder watches, the others stand down, and a released or expired lease is inherited on the next retry. `scribe` ingests each home in sequence. Nerve adds ingest-on-read: `attach`, `chart`, `follow`, and `tick` run Scribe's fingerprint sweep for each supplied pair before answering. For `chart` and `follow`, unpaired `--home-id` values filter already-registered homes, while omitting the filter reads every registered home. Supplying paired `--home PATH --home-id ID` values refreshes those homes but does not filter output.

Watch telemetry is JSONL next to `firstmate.db` (`logs/watch-YYYY-MM-DD.jsonl`), never a database table. Lines record raise, lease-acquired, lease-lost, sweep counts, and crash-retry. A crash inside the child is logged and retried with backoff; it does not require a daemon.

`place` copies this executable into the plugin directory (default: the directory of `ROCA_FIRSTMATE_DB`) as `roca-firstmate`, so a session parent resolves it from that directory instead of PATH luck. The release installer already places that file. This plugin does not add unknown manifest fields: current La Roca rejects them. The watch child is ready for a generic session-companion declaration any plugin could name; that kernel seam, if added, must not mention firstmate.

## Attach: the default gesture

Set local configuration, enter the workspace the thinker occupies, and attach:

```sh
export ROCA_FIRSTMATE_DB='<plugin directory>/firstmate.db'
export FIRSTMATE_HOME='<firstmate home>'
export FIRSTMATE_HOME_ID='local-primary'
roca firstmate attach
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
roca firstmate follow --destination companion
roca firstmate follow --destination companion --home-id skiff-secondmate
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
roca firstmate tick --silence-after 5m
```

Seats keep leases in SQLite. The silence clock records durable generations per expired seat, and each silence wakeup keeps that seat's `home_id`. A wakeup is orphaned when its destination has no unexpired seat lease; tick delivers it with the same one-line and handled-generation contract as follow. No tick state survives in memory.

## Chart and SQL history

`chart` is a bounded AXI get-or-create: generate, serve cached, or regenerate when its watermark moves.

```sh
roca firstmate chart --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db
roca firstmate chart --home '<fabricated home>' --home-id northwind-harbor --db firstmate.db --json
roca firstmate chart --db firstmate.db --home-id skiff-secondmate
```

The cache lives in `chart_cache`, not Markdown. Default output is bounded TOON with `help[]`; `--json` returns the complete envelope. Use gated SQL for history:

```sh
roca exec 'SELECT home_id, relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions WHERE is_current = 1 ORDER BY home_id, relative_path'
roca exec 'SELECT id, home_id, destination, generation, handled_generation, kind, created_at FROM plugin_roca_firstmate.wakeups ORDER BY generation'
```

Schema source: [`schema/schema.sql`](schema/schema.sql). The visible tables are the five `*_versions` families, `homes`, `tasks`, `ingest_file_state`, `seats`, `wakeups`, and `chart_cache`. La Roca hides `plugin_schema` bookkeeping. [`plugin.json`](plugin.json) owns the semantic and vector declarations; `schema/package_test.go` enforces their parity and selectivity.

## Layout

```text
plugin.json              # identity, database, semantic, and vector declarations
schema/schema.sql        # source of truth for database-backed first-run creation
cmd/roca-firstmate/      # attach, follow, tick, chart, place, and Scribe verbs
cmd/package/             # release packager: empty db, executable, checksums, archive
internal/release/        # packager contract tests and scratch-home e2e
internal/nerve/          # WAL subscription, seats, watch leases, silence clock, orphan drain
internal/scribe/         # total versioned Markdown mirror
internal/watch/          # Scribe FSEvents and polling backends
internal/watchlog/       # JSONL telemetry for the session-owned watcher
skills/dresser/SKILL.md  # companion operating skill and adapter recipes
testdata/homes/          # fabricated homes only
```

Mobile routing, `state/` telemetry tables, and an optional resident daemon remain additive later work. Session-owned `watch` is not a daemon. None blocks Nerve v1. Do not add Distiller or edit the La Roca product repository from this example.
