DROP TRIGGER working_set_version_wakeup;
DROP TRIGGER archive_version_wakeup;
DROP TRIGGER task_state_version_wakeup;
DROP TRIGGER operational_doc_version_wakeup;
DROP TRIGGER task_artifact_version_wakeup;

DROP INDEX wakeups_unhandled;
DROP INDEX wakeups_generation;

ALTER TABLE wakeups RENAME TO wakeups_v2;

CREATE TABLE wakeups (
  id INTEGER PRIMARY KEY,
  destination TEXT NOT NULL CHECK (destination IN ('captain', 'companion', 'machine')),
  handled INTEGER NOT NULL DEFAULT 0 CHECK (handled IN (0, 1)),
  generation INTEGER NOT NULL,
  handled_generation INTEGER,
  kind TEXT NOT NULL DEFAULT '',
  home_id TEXT REFERENCES homes(home_id),
  task_id TEXT,
  payload TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  CHECK (
    (handled = 0 AND handled_generation IS NULL)
    OR (handled = 1 AND handled_generation IS NOT NULL AND handled_generation = generation)
  )
);

INSERT INTO wakeups (
  id, destination, handled, generation, handled_generation,
  kind, home_id, task_id, payload, created_at
)
SELECT id, destination, handled, generation,
  CASE WHEN handled = 1 THEN generation END,
  kind, home_id, task_id, payload, created_at
FROM wakeups_v2;

DROP TABLE wakeups_v2;

CREATE INDEX wakeups_unhandled
  ON wakeups(handled, destination) WHERE handled = 0;

CREATE INDEX wakeups_generation
  ON wakeups(generation);

CREATE TABLE seats (
  seat_id TEXT PRIMARY KEY,
  home_id TEXT NOT NULL REFERENCES homes(home_id),
  workspace_fingerprint TEXT NOT NULL,
  label TEXT NOT NULL,
  destination TEXT NOT NULL CHECK (destination IN ('captain', 'companion', 'machine')),
  attached_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  lease_until TEXT NOT NULL,
  silence_generation INTEGER NOT NULL DEFAULT 0 CHECK (silence_generation >= 0),
  UNIQUE (home_id, workspace_fingerprint)
);

CREATE INDEX seats_destination_lease
  ON seats(destination, lease_until);

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

UPDATE plugin_schema SET schema_version = 3 WHERE singleton = 1;
