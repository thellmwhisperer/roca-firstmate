CREATE TABLE ingest_file_state (
  path TEXT NOT NULL PRIMARY KEY,
  source_kind TEXT NOT NULL,
  source_agent TEXT,
  project TEXT,
  fingerprint TEXT,
  last_synced_at TEXT,
  last_error TEXT,
  metadata TEXT NOT NULL DEFAULT '{}'
);

CREATE INDEX ingest_file_state_project
  ON ingest_file_state(project);

CREATE INDEX ingest_file_state_source_agent
  ON ingest_file_state(source_agent);

ALTER TABLE operational_doc_versions RENAME TO operational_doc_versions_v1;

CREATE TABLE operational_doc_versions (
  id INTEGER PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  relative_path TEXT NOT NULL CHECK (
    relative_path NOT GLOB '*/*'
    AND relative_path NOT GLOB '/*'
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

INSERT INTO operational_doc_versions (
  id, home_id, relative_path, document_kind, version, is_current,
  content, content_sha256, observed_at, source_mtime
)
SELECT
  id, home_id, relative_path, document_kind, version, is_current,
  content, content_sha256, observed_at, source_mtime
FROM operational_doc_versions_v1;

DROP TABLE operational_doc_versions_v1;

CREATE UNIQUE INDEX operational_doc_current
  ON operational_doc_versions(home_id, relative_path) WHERE is_current = 1;

ALTER TABLE task_artifact_versions RENAME TO task_artifact_versions_v1;

CREATE TABLE task_artifact_versions (
  id INTEGER PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  task_id TEXT NOT NULL,
  relative_path TEXT NOT NULL,
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

INSERT INTO task_artifact_versions (
  id, home_id, task_id, relative_path, document_kind, version, is_current,
  content, content_sha256, observed_at, source_mtime
)
SELECT
  id, home_id, task_id, relative_path, document_kind, version, is_current,
  content, content_sha256, observed_at, source_mtime
FROM task_artifact_versions_v1;

DROP TABLE task_artifact_versions_v1;

CREATE UNIQUE INDEX task_artifact_current
  ON task_artifact_versions(home_id, relative_path) WHERE is_current = 1;

CREATE INDEX task_artifact_task
  ON task_artifact_versions(home_id, task_id);

CREATE TRIGGER task_artifact_path_insert
BEFORE INSERT ON task_artifact_versions
WHEN substr(NEW.relative_path, 1, length(NEW.task_id) + 1) != NEW.task_id || '/'
  OR instr('/' || NEW.relative_path || '/', '/../') > 0
BEGIN
  SELECT RAISE(ABORT, 'task artifact path must start with its literal task id');
END;

CREATE TRIGGER task_artifact_path_update
BEFORE UPDATE OF task_id, relative_path ON task_artifact_versions
WHEN substr(NEW.relative_path, 1, length(NEW.task_id) + 1) != NEW.task_id || '/'
  OR instr('/' || NEW.relative_path || '/', '/../') > 0
BEGIN
  SELECT RAISE(ABORT, 'task artifact path must start with its literal task id');
END;

CREATE TRIGGER working_set_version_wakeup
AFTER INSERT ON working_set_versions
BEGIN
  INSERT INTO wakeups (
    destination, generation, kind, home_id, payload, created_at
  ) VALUES (
    'companion',
    (SELECT COALESCE(MAX(generation), 0) + 1 FROM wakeups),
    'document-mirrored',
    NEW.home_id,
    json_object(
      'family', 'working_set',
      'row_id', NEW.id,
      'relative_path', NEW.relative_path,
      'version', NEW.version
    ),
    NEW.observed_at
  );
END;

CREATE TRIGGER archive_version_wakeup
AFTER INSERT ON archive_versions
BEGIN
  INSERT INTO wakeups (
    destination, generation, kind, home_id, payload, created_at
  ) VALUES (
    'companion',
    (SELECT COALESCE(MAX(generation), 0) + 1 FROM wakeups),
    'document-mirrored',
    NEW.home_id,
    json_object(
      'family', 'archive',
      'row_id', NEW.id,
      'relative_path', NEW.relative_path,
      'version', NEW.version
    ),
    NEW.observed_at
  );
END;

CREATE TRIGGER task_state_version_wakeup
AFTER INSERT ON task_state_versions
BEGIN
  INSERT INTO wakeups (
    destination, generation, kind, home_id, payload, created_at
  ) VALUES (
    'companion',
    (SELECT COALESCE(MAX(generation), 0) + 1 FROM wakeups),
    'document-mirrored',
    NEW.home_id,
    json_object(
      'family', 'task_state',
      'row_id', NEW.id,
      'relative_path', NEW.relative_path,
      'version', NEW.version
    ),
    NEW.observed_at
  );
END;

CREATE TRIGGER operational_doc_version_wakeup
AFTER INSERT ON operational_doc_versions
BEGIN
  INSERT INTO wakeups (
    destination, generation, kind, home_id, payload, created_at
  ) VALUES (
    'companion',
    (SELECT COALESCE(MAX(generation), 0) + 1 FROM wakeups),
    'document-mirrored',
    NEW.home_id,
    json_object(
      'family', 'operational_doc',
      'row_id', NEW.id,
      'relative_path', NEW.relative_path,
      'version', NEW.version
    ),
    NEW.observed_at
  );
END;

CREATE TRIGGER task_artifact_version_wakeup
AFTER INSERT ON task_artifact_versions
BEGIN
  INSERT INTO wakeups (
    destination, generation, kind, home_id, task_id, payload, created_at
  ) VALUES (
    'companion',
    (SELECT COALESCE(MAX(generation), 0) + 1 FROM wakeups),
    'document-mirrored',
    NEW.home_id,
    NEW.task_id,
    json_object(
      'family', 'task_artifact',
      'row_id', NEW.id,
      'relative_path', NEW.relative_path,
      'version', NEW.version
    ),
    NEW.observed_at
  );
END;

UPDATE plugin_schema SET schema_version = 2 WHERE singleton = 1;
