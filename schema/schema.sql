-- firstmate.db schema version 1.
--
-- Five markdown inventory families, each stored as a versioned mirror:
-- every observed rewrite of a file is a new row, and the current file is
-- the row with is_current = 1.
--
-- state/ telemetry is out of v1. Later additive tables such as
-- status_events, task_meta, and wake_queue can land without rewriting
-- these family tables.
--
-- plugin_schema is La Roca hidden bookkeeping. It is omitted from the
-- semantic fragment on purpose.

CREATE TABLE plugin_schema (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  plugin_name TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  index_version INTEGER NOT NULL
);

INSERT INTO plugin_schema (singleton, plugin_name, schema_version, index_version)
VALUES (1, 'roca-firstmate', 1, 1);

CREATE TABLE homes (
  home_id TEXT PRIMARY KEY,
  label TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('primary', 'secondmate')),
  recorded_at TEXT NOT NULL
);

-- Task identity for cross-references only. tasks-axi stays the query layer
-- for backlog work; this table does not migrate that layer.
CREATE TABLE tasks (
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  task_id TEXT NOT NULL,
  PRIMARY KEY (home_id, task_id)
);

-- 1. Startup working set: captain.md, captain-shared.md, learnings.md,
-- projects.md, secondmates.md. Rewrite-in-place on disk; versioned here.
CREATE TABLE working_set_versions (
  id INTEGER PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  relative_path TEXT NOT NULL CHECK (relative_path IN (
    'captain.md',
    'captain-shared.md',
    'learnings.md',
    'projects.md',
    'secondmates.md'
  )),
  document_kind TEXT NOT NULL CHECK (document_kind IN (
    'captain',
    'captain-shared',
    'learnings',
    'projects',
    'secondmates'
  )),
  version INTEGER NOT NULL CHECK (version >= 1),
  is_current INTEGER NOT NULL CHECK (is_current IN (0, 1)),
  content TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  source_mtime TEXT,
  UNIQUE (home_id, relative_path, version)
);

CREATE UNIQUE INDEX working_set_current
  ON working_set_versions(home_id, relative_path) WHERE is_current = 1;

-- 2. Archives: lossy residue of stow reductions. Once the mirror keeps
-- versions, this family is redundant going forward.
CREATE TABLE archive_versions (
  id INTEGER PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  relative_path TEXT NOT NULL CHECK (relative_path IN (
    'captain-archive.md',
    'memory-archive.md',
    'note-archive.md'
  )),
  document_kind TEXT NOT NULL CHECK (document_kind IN (
    'captain-archive',
    'memory-archive',
    'note-archive'
  )),
  version INTEGER NOT NULL CHECK (version >= 1),
  is_current INTEGER NOT NULL CHECK (is_current IN (0, 1)),
  content TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  source_mtime TEXT,
  UNIQUE (home_id, relative_path, version)
);

CREATE UNIQUE INDEX archive_current
  ON archive_versions(home_id, relative_path) WHERE is_current = 1;

-- 3. Task state, mirrored for cross-references only.
CREATE TABLE task_state_versions (
  id INTEGER PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  relative_path TEXT NOT NULL CHECK (relative_path IN (
    'backlog.md',
    'done-archive.md'
  )),
  document_kind TEXT NOT NULL CHECK (document_kind IN (
    'backlog',
    'done-archive'
  )),
  version INTEGER NOT NULL CHECK (version >= 1),
  is_current INTEGER NOT NULL CHECK (is_current IN (0, 1)),
  content TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  source_mtime TEXT,
  UNIQUE (home_id, relative_path, version)
);

CREATE UNIQUE INDEX task_state_current
  ON task_state_versions(home_id, relative_path) WHERE is_current = 1;

-- 4. One-shot dated operational docs at the data/ root. Type is encoded
-- in the filename prefix (decision, order, brief, status, planning,
-- handover, postmortem, and later prefixes). Immutable once written;
-- a rewrite is still stored as a further version.
CREATE TABLE operational_doc_versions (
  id INTEGER PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  relative_path TEXT NOT NULL CHECK (
    relative_path NOT GLOB '*/*'
    AND relative_path NOT GLOB '/*'
    AND instr(relative_path, '..') = 0
    AND relative_path NOT IN (
      'captain.md',
      'captain-shared.md',
      'learnings.md',
      'projects.md',
      'secondmates.md',
      'captain-archive.md',
      'memory-archive.md',
      'note-archive.md',
      'backlog.md',
      'done-archive.md'
    )
  ),
  document_kind TEXT NOT NULL,
  version INTEGER NOT NULL CHECK (version >= 1),
  is_current INTEGER NOT NULL CHECK (is_current IN (0, 1)),
  content TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  source_mtime TEXT,
  UNIQUE (home_id, relative_path, version)
);

CREATE UNIQUE INDEX operational_doc_current
  ON operational_doc_versions(home_id, relative_path) WHERE is_current = 1;

-- 5. Per-task artifacts under data/<task-id>/, keyed to tasks.task_id.
CREATE TABLE task_artifact_versions (
  id INTEGER PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  task_id TEXT NOT NULL,
  relative_path TEXT NOT NULL CHECK (
    instr(relative_path, '..') = 0
    AND relative_path GLOB (task_id || '/*')
    AND relative_path NOT GLOB (task_id || '/*/*')
  ),
  document_kind TEXT NOT NULL,
  version INTEGER NOT NULL CHECK (version >= 1),
  is_current INTEGER NOT NULL CHECK (is_current IN (0, 1)),
  content TEXT NOT NULL,
  content_sha256 TEXT NOT NULL,
  observed_at TEXT NOT NULL,
  source_mtime TEXT,
  UNIQUE (home_id, relative_path, version),
  FOREIGN KEY (home_id, task_id) REFERENCES tasks(home_id, task_id)
);

CREATE UNIQUE INDEX task_artifact_current
  ON task_artifact_versions(home_id, relative_path) WHERE is_current = 1;

CREATE INDEX task_artifact_task
  ON task_artifact_versions(home_id, task_id);
