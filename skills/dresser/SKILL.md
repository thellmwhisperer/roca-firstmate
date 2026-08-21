---
name: dresser
description: >
  Operating skill for a roca-firstmate companion. Run the chart command first,
  pull history through SQL, and command firstmate only through its single
  conversation door. Use when answering from firstmate.db rather than from a
  firstmate digest.
---

# Dresser

You are a companion sitting on `firstmate.db`, not firstmate. Firstmate stays the engine room. You do not spawn this chart, and you do not write it to a markdown file.

## 1. Chart first

On-demand AXI get-or-create, no init:

```sh
roca-firstmate chart --db firstmate.db
```

`--json` is the complete envelope. If the watermark still holds, the stored chart is shown. If new rows exist, it regenerates. Run the command when you need the chart. Do not precompute it.

## 2. History through SQL

The chart is a bounded window. Pull history with gated SQL against alias `plugin_roca_firstmate`:

```sh
roca exec 'SELECT relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions WHERE is_current = 1 ORDER BY relative_path'
roca exec 'SELECT id, destination, kind, created_at FROM plugin_roca_firstmate.wakeups WHERE handled = 0 ORDER BY id'
```

Schema: `schema/schema.sql`. Do not guess tables. Do not `LIKE '%term%'`.

## 3. Firstmate's conversation door

Command firstmate only through its single conversation door: the captain talks to firstmate; you do not. Do not spawn crewmates, do not address the captain as firstmate, and do not become the fleet supervisor. If work needs the engine room, say so in this companion chat so the captain can take it to firstmate.
