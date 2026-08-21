# roca-firstmate

Public normative La Roca plugin example. It ships `firstmate.db` as an external federated database. The La Roca product repo stays untouched. Firstmate is mirrored, not modified.

## Contract

- Payload the installer verifies is exactly `plugin.json` and `firstmate.db`. Source of the database is `schema/schema.sql`.
- Five markdown families are versioned: every rewrite is a new row, `is_current` marks the latest file. Schema: `schema/schema.sql`.
- `wakeups` and Scribe's insert triggers ship now (Nerve consumer/routing later). v1 destinations are `machine` and `companion` only. `chart_cache` holds the on-demand AXI chart and its watermark. Distiller is deleted.
- Do not install this plugin onto a live captain La Roca. Prove the payload with `make check`.
- `roca-firstmate chart` is get-or-create: generate, serve cached, or regenerate. Bounded TOON, `help[]`, `--json`. Nothing runs when a session opens.
- Dresser (`skills/dresser/SKILL.md`) is a skill, not a hook: chart first, SQL for history, firstmate only through its conversation door.
- Scribe mirrors every Markdown file under `home/data`: FSEvents first on macOS, polling fallback, one-shot fingerprint scan as initial backfill and nightly safety net. The transactional chain is file event -> version row -> SQL trigger -> wakeup. Implementation: `internal/scribe/`, `internal/watch/`.
- Backlog and per-task Markdown are mirrored for history and cross-references only. `tasks-axi` stays the query layer.
- `state/` telemetry is out of v1. Leave `status_events`, `task_meta`, and `wake_queue` free for an additive later schema.
- Tests use fabricated homes only (`testdata/homes/`). Never copy real homes, real paths, live ids, corpus counts, or live-install details into this repo.
- Scribe imports La Roca's public parser, provenance, incrementality, and corpus-writer contracts. Nerve routing remains later.
- Teach the plugin pattern in `README.md`. Point at La Roca `docs/plugins.md` for the minimal quickstart.

## Commands

```sh
make check      # gofmt, go vet, go test
make sync-db    # rebuild firstmate.db and checksums.txt from schema.sql
```

## Maintaining this file

Keep this file for knowledge useful to almost every future agent session in this project.
Do not repeat what the codebase already shows; point to the authoritative file or command instead.
Prefer rewriting or pruning existing entries over appending new ones.
When updating this file, preserve this bar for all agents and keep entries concise.
