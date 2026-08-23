# roca-firstmate

Public normative La Roca plugin example. The La Roca product repo stays untouched. Firstmate is mirrored, not modified.

## Contract

- Never commit a `.db` file. `make package` / `make dist` generate empty `firstmate.db` at pack time.
- Five markdown families are versioned: every rewrite is a new row, `is_current` marks the latest file. Schema: `schema/schema.sql`.
- `wakeups`, Scribe's insert triggers, and Nerve v1 ship now. Destinations are `captain`, `companion`, and `machine`; mobile is later. `attach` is the default foreground subscription, `follow` listens to the WAL, and ephemeral `tick` owns silence/orphan recovery. No default daemon or KeepAlive. `README.md` owns the plugin-author example, installer payload, custodial update rules, versioning, session-owned `watch`, and `place` contract. `chart_cache` holds the on-demand AXI chart and its watermark. Distiller is deleted.
- Do not install this plugin onto a live captain La Roca. Prove the payload with `make check` and `make e2e`.
- The installable package is the GitHub release archive, not the git tree. `plugin.json` `binary` is `roca-firstmate`. Packaged `checksums.txt` covers exactly `firstmate.db`, `plugin.json`, and `roca-firstmate`. Source-tree `checksums.txt` fingerprints `plugin.json` only (`make sync-db`).
- Tags are `vMAJOR.MINOR.PATCH` matching `plugin.json` `version`. `.github/workflows/release.yml` publishes darwin-arm64 and linux-amd64 archives.
- `roca-firstmate chart` is get-or-create: generate, serve cached, or regenerate. Bounded TOON, `help[]`, `--json`. With supplied home pairs, verbs perform Scribe's fingerprint sweep before answering. `README.md` owns the CLI's multi-home and filter contract.
- Dresser (`skills/dresser/SKILL.md`) is a skill, not a hook: attach first, choose the harness latency recipe, SQL for history, firstmate only through its conversation door.
- Scribe mirrors every Markdown file under `home/data`: FSEvents first on macOS, polling fallback, one-shot fingerprint scan as initial backfill and nightly safety net. The transactional chain is file event -> version row -> SQL trigger -> wakeup. Implementation: `internal/scribe/`, `internal/watch/`.
- Backlog and per-task Markdown are mirrored for history and cross-references only. `tasks-axi` stays the query layer.
- `state/` telemetry is out of v1. Leave `status_events`, `task_meta`, and `wake_queue` free for an additive later schema.
- Tests use fabricated homes only (`testdata/homes/`). Never copy real homes, real paths, live ids, corpus counts, or live-install details into this repo.
- Public La Roca package compatibility is enforced by `internal/scribe/public_packages_test.go`; Nerve routing remains later.
- Teach the plugin pattern in `README.md`. Point at La Roca `docs/plugins.md` for the minimal quickstart. Manifest/database parity and embeddings-only vector selectivity are enforced by `schema/package_test.go`.

## Commands

```sh
make check      # gofmt, go vet, go test
make package    # host-platform install directory
make dist       # darwin-arm64 and linux-amd64 release archives
make e2e        # scratch-home install and release-to-release update
make sync-db    # refresh source-tree checksums.txt for plugin.json
```

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
