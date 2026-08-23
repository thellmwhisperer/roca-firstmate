// Package scribe mirrors every Markdown file under a firstmate home's data
// directory into firstmate.db. It keeps the file bytes exact and records each
// observed rewrite as a new version.
package scribe

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/thellmwhisperer/la-roca/pkg/incrementality"
	"github.com/thellmwhisperer/la-roca/pkg/ingestprovenance"
	"github.com/thellmwhisperer/la-roca/pkg/parsers"
)

const parserVersion = "scribe-v1"

var workingSetKinds = map[string]string{
	"captain.md":        "captain",
	"captain-shared.md": "captain-shared",
	"learnings.md":      "learnings",
	"projects.md":       "projects",
	"secondmates.md":    "secondmates",
}

var archiveKinds = map[string]string{
	"captain-archive.md": "captain-archive",
	"memory-archive.md":  "memory-archive",
	"note-archive.md":    "note-archive",
}

var taskStateKinds = map[string]string{
	"backlog.md":      "backlog",
	"done-archive.md": "done-archive",
}

// Config identifies one source home without persisting its absolute path.
type Config struct {
	Home        string
	HomeID      string
	Label       string
	Kind        string
	SourceAgent string
	Now         func() time.Time
	CommitFence func(context.Context, *sql.Tx) error
}

// Summary is the bounded result of one total backfill or safety scan.
type Summary struct {
	Status          string   `json:"status"`
	HomeID          string   `json:"home_id"`
	Scanned         int      `json:"scanned"`
	Inserted        int      `json:"inserted"`
	Unchanged       int      `json:"unchanged"`
	WorkingSet      int      `json:"working_set"`
	Archives        int      `json:"archives"`
	TaskState       int      `json:"task_state"`
	OperationalDocs int      `json:"operational_docs"`
	TaskArtifacts   int      `json:"task_artifacts"`
	Wakeups         int      `json:"wakeups"`
	Help            []string `json:"help"`
}

// Event is the result of ingesting one filesystem event target.
type Event struct {
	Status       string `json:"status"`
	HomeID       string `json:"home_id"`
	RelativePath string `json:"relative_path"`
	Family       string `json:"family"`
	Version      int    `json:"version"`
	Inserted     bool   `json:"inserted"`
	Wakeups      int    `json:"wakeups"`
}

type classification struct {
	table  string
	family string
	kind   string
	taskID string
}

// Ingester owns an incremental view of one home for a backfill followed by
// continuous file events.
type Ingester struct {
	db            *sql.DB
	config        Config
	dataRoot      string
	state         map[string]incrementality.FileState
	sourceSurface string
}

func ValidateConfig(config Config) error {
	_, err := normalizeConfig(config)
	return err
}

func normalizeConfig(config Config) (Config, error) {
	config.Home = strings.TrimSpace(config.Home)
	config.HomeID = strings.TrimSpace(config.HomeID)
	config.Label = strings.TrimSpace(config.Label)
	config.Kind = strings.TrimSpace(config.Kind)
	config.SourceAgent = strings.TrimSpace(config.SourceAgent)
	if config.Home == "" {
		return Config{}, errors.New("scribe: home is required")
	}
	if err := validateHomeID(config.HomeID); err != nil {
		return Config{}, err
	}
	if config.Label == "" {
		config.Label = config.HomeID
	}
	if config.Kind == "" {
		config.Kind = "primary"
	}
	if config.Kind != "primary" && config.Kind != "secondmate" {
		return Config{}, fmt.Errorf("scribe: home kind %q must be primary or secondmate", config.Kind)
	}
	if config.SourceAgent == "" {
		config.SourceAgent = "firstmate"
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return config, nil
}

func resolveConfig(config Config) (Config, string, error) {
	config, err := normalizeConfig(config)
	if err != nil {
		return Config{}, "", err
	}
	home, err := filepath.Abs(config.Home)
	if err != nil {
		return Config{}, "", fmt.Errorf("scribe: resolve home: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(home); resolveErr == nil {
		home = resolved
	}
	dataRoot := filepath.Join(home, "data")
	info, err := os.Stat(dataRoot)
	if err != nil {
		return Config{}, "", fmt.Errorf("scribe: read data directory: %w", err)
	}
	if !info.IsDir() {
		return Config{}, "", fmt.Errorf("scribe: %s is not a directory", dataRoot)
	}
	return config, dataRoot, nil
}

// EnsureHome validates a source home and inserts its identity only when absent.
func EnsureHome(ctx context.Context, db *sql.DB, config Config) error {
	if db == nil {
		return errors.New("scribe: nil database")
	}
	config, _, err := resolveConfig(config)
	if err != nil {
		return err
	}
	now := config.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(home_id) DO NOTHING`, config.HomeID, config.Label, config.Kind, now); err != nil {
		return fmt.Errorf("scribe: ensure home: %w", err)
	}
	return nil
}

// New validates the configured home, registers its local identity, and loads
// the shared La Roca unchanged-pass cursor.
func New(ctx context.Context, db *sql.DB, config Config) (*Ingester, error) {
	if db == nil {
		return nil, errors.New("scribe: nil database")
	}
	config, dataRoot, err := resolveConfig(config)
	if err != nil {
		return nil, err
	}

	now := config.Now().UTC().Format(time.RFC3339Nano)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("scribe: begin home registration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(home_id) DO UPDATE SET
			label = excluded.label,
			kind = excluded.kind`, config.HomeID, config.Label, config.Kind, now); err != nil {
		return nil, fmt.Errorf("scribe: register home: %w", err)
	}
	if config.CommitFence != nil {
		if err := config.CommitFence(ctx, tx); err != nil {
			return nil, fmt.Errorf("scribe: fence home registration: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("scribe: commit home registration: %w", err)
	}
	state, err := incrementality.LoadState(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("scribe: load cursor: %w", err)
	}
	surface := ingestprovenance.HarnessForSource(config.SourceAgent)
	if surface == "" {
		surface = "Firstmate"
	}
	return &Ingester{
		db: db, config: config, dataRoot: dataRoot, state: state, sourceSurface: surface,
	}, nil
}

// DataRoot is the recursively watched directory.
func (i *Ingester) DataRoot() string { return i.dataRoot }

// Backfill mirrors every regular Markdown file below data/. Directory walking
// is lexical, so first install and nightly safety scans are deterministic.
func (i *Ingester) Backfill(ctx context.Context) (Summary, error) {
	result := Summary{Status: "mirrored", HomeID: i.config.HomeID, Help: HelpLines()}
	err := filepath.WalkDir(i.dataRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		result.Scanned++
		event, err := i.IngestPath(ctx, path)
		if err != nil {
			return err
		}
		if !event.Inserted {
			result.Unchanged++
			return nil
		}
		result.Inserted++
		result.Wakeups += event.Wakeups
		switch event.Family {
		case "working_set":
			result.WorkingSet++
		case "archive":
			result.Archives++
		case "task_state":
			result.TaskState++
		case "operational_doc":
			result.OperationalDocs++
		case "task_artifact":
			result.TaskArtifacts++
		}
		return nil
	})
	if err != nil {
		return Summary{}, fmt.Errorf("scribe: total Markdown scan: %w", err)
	}
	return result, nil
}

// IngestPath mirrors one file event. Non-Markdown paths and vanished files are
// harmless no-ops; directory or dropped-event recovery is handled by Backfill.
func (i *Ingester) IngestPath(ctx context.Context, path string) (Event, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Event{}, fmt.Errorf("scribe: resolve event path: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	rel, err := filepath.Rel(i.dataRoot, abs)
	if err != nil {
		return Event{}, fmt.Errorf("scribe: relate event path: %w", err)
	}
	rel = filepath.ToSlash(rel)
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return Event{}, fmt.Errorf("scribe: event path is outside data/: %s", path)
	}
	event := Event{Status: "unchanged", HomeID: i.config.HomeID, RelativePath: rel}
	if !strings.EqualFold(filepath.Ext(rel), ".md") {
		return event, nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return event, nil
		}
		return Event{}, fmt.Errorf("scribe: stat %s: %w", rel, err)
	}
	if !info.Mode().IsRegular() {
		return event, nil
	}

	cursorPath := "firstmate:" + i.config.HomeID + "/data/" + rel
	if err := ctx.Err(); err != nil {
		return Event{}, err
	}
	fingerprint, err := incrementality.TargetFingerprint(incrementality.Target{
		Path: abs, Kind: "firstmate_markdown", SourceAgent: i.config.SourceAgent,
		Project: i.config.HomeID, ParserVersion: parserVersion,
	})
	if err != nil {
		return Event{}, fmt.Errorf("scribe: fingerprint %s: %w", rel, err)
	}
	if incrementality.Unchanged(i.state, cursorPath, fingerprint) {
		unchanged, err := i.persistedUnchanged(ctx, cursorPath, fingerprint)
		if err != nil {
			return Event{}, fmt.Errorf("scribe: validate cursor %s: %w", rel, err)
		}
		if unchanged {
			return event, nil
		}
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return Event{}, fmt.Errorf("scribe: read %s: %w", rel, err)
	}
	class, err := classify(rel)
	if err != nil {
		return Event{}, err
	}
	event.Family = class.family

	digest := sha256.Sum256(content)
	contentSHA := hex.EncodeToString(digest[:])
	mtime := info.ModTime().UTC().Format(time.RFC3339Nano)
	observed := i.config.Now().UTC().Format(time.RFC3339Nano)

	tx, err := i.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, fmt.Errorf("scribe: begin %s: %w", rel, err)
	}
	defer tx.Rollback()

	var currentSHA string
	version := 0
	err = tx.QueryRowContext(ctx, `SELECT content_sha256, version FROM `+class.table+`
		WHERE home_id = ? AND relative_path = ? AND is_current = 1`, i.config.HomeID, rel).
		Scan(&currentSHA, &version)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Event{}, fmt.Errorf("scribe: read current %s: %w", rel, err)
	}
	if err == nil && currentSHA == contentSHA {
		if err := i.recordState(ctx, tx, cursorPath, fingerprint, rel, class, version, content); err != nil {
			return Event{}, err
		}
		if err := i.fenceCommit(ctx, tx, rel); err != nil {
			return Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return Event{}, fmt.Errorf("scribe: commit unchanged %s: %w", rel, err)
		}
		i.state[cursorPath] = incrementality.FileState{Fingerprint: fingerprint}
		event.Version = version
		return event, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		version = 0
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE `+class.table+` SET is_current = 0
			WHERE home_id = ? AND relative_path = ? AND is_current = 1`, i.config.HomeID, rel); err != nil {
			return Event{}, fmt.Errorf("scribe: retire current %s: %w", rel, err)
		}
	}
	version++
	if class.taskID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO tasks (home_id, task_id) VALUES (?, ?)`,
			i.config.HomeID, class.taskID); err != nil {
			return Event{}, fmt.Errorf("scribe: register task %s: %w", class.taskID, err)
		}
	}
	columns := "home_id, relative_path, document_kind, version, is_current, content, content_sha256, observed_at, source_mtime"
	values := "?, ?, ?, ?, 1, ?, ?, ?, ?"
	args := []any{i.config.HomeID, rel, class.kind, version, string(content), contentSHA, observed, mtime}
	if class.taskID != "" {
		columns = "home_id, task_id, relative_path, document_kind, version, is_current, content, content_sha256, observed_at, source_mtime"
		values = "?, ?, ?, ?, ?, 1, ?, ?, ?, ?"
		args = []any{i.config.HomeID, class.taskID, rel, class.kind, version, string(content), contentSHA, observed, mtime}
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO `+class.table+` (`+columns+`) VALUES (`+values+`)`, args...)
	if err != nil {
		return Event{}, fmt.Errorf("scribe: insert %s version %d: %w", rel, version, err)
	}
	rowID, err := insert.LastInsertId()
	if err != nil {
		return Event{}, fmt.Errorf("scribe: identify %s version %d: %w", rel, version, err)
	}
	var wakeups int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM wakeups
		WHERE kind = 'document-mirrored' AND home_id = ?
		  AND json_extract(payload, '$.family') = ?
		  AND json_extract(payload, '$.row_id') = ?`, i.config.HomeID, class.family, rowID).
		Scan(&wakeups); err != nil {
		return Event{}, fmt.Errorf("scribe: verify wakeup for %s: %w", rel, err)
	}
	if wakeups != 1 {
		return Event{}, fmt.Errorf("scribe: causal wakeup for %s is %d, want 1", rel, wakeups)
	}
	if err := i.recordState(ctx, tx, cursorPath, fingerprint, rel, class, version, content); err != nil {
		return Event{}, err
	}
	if err := i.fenceCommit(ctx, tx, rel); err != nil {
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, fmt.Errorf("scribe: commit %s: %w", rel, err)
	}
	i.state[cursorPath] = incrementality.FileState{Fingerprint: fingerprint}
	event.Status = "mirrored"
	event.Version = version
	event.Inserted = true
	event.Wakeups = wakeups
	return event, nil
}

func (i *Ingester) persistedUnchanged(ctx context.Context, cursorPath, fingerprint string) (bool, error) {
	var persisted, failure string
	err := i.db.QueryRowContext(ctx, `SELECT COALESCE(fingerprint, ''), COALESCE(last_error, '')
		FROM ingest_file_state WHERE path = ?`, cursorPath).Scan(&persisted, &failure)
	if errors.Is(err, sql.ErrNoRows) {
		delete(i.state, cursorPath)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	i.state[cursorPath] = incrementality.FileState{Fingerprint: persisted, LastError: failure}
	return failure == "" && persisted != "" && persisted == fingerprint, nil
}

func (i *Ingester) fenceCommit(ctx context.Context, tx *sql.Tx, rel string) error {
	if i.config.CommitFence == nil {
		return nil
	}
	if err := i.config.CommitFence(ctx, tx); err != nil {
		return fmt.Errorf("scribe: commit fence for %s: %w", rel, err)
	}
	return nil
}

func (i *Ingester) recordState(ctx context.Context, tx *sql.Tx, cursorPath, fingerprint,
	rel string, class classification, version int, content []byte) error {
	parsed := parsers.ParseMemoryFile(content)
	summary := map[string]any{
		"family":         class.family,
		"relative_path":  rel,
		"table":          class.table,
		"version":        version,
		"source_surface": i.sourceSurface,
	}
	if parsed.Type != "" {
		summary["declared_type"] = parsed.Type
	}
	target := incrementality.Target{
		Path: cursorPath, Kind: "firstmate_markdown", SourceAgent: i.config.SourceAgent,
		Project: i.config.HomeID, ParserVersion: parserVersion,
	}
	if err := incrementality.RecordState(ctx, tx, target, fingerprint, "", summary); err != nil {
		return fmt.Errorf("scribe: record cursor for %s: %w", rel, err)
	}
	return nil
}

func classify(rel string) (classification, error) {
	rel = filepath.ToSlash(filepath.Clean(rel))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || !strings.EqualFold(filepath.Ext(rel), ".md") {
		return classification{}, fmt.Errorf("scribe: %q is not a Markdown path under data/", rel)
	}
	if strings.Contains(rel, "/") {
		taskID, _, _ := strings.Cut(rel, "/")
		if err := validateTaskID(taskID); err != nil {
			return classification{}, err
		}
		return classification{
			table: "task_artifact_versions", family: "task_artifact",
			kind: documentKind(filepath.Base(rel)), taskID: taskID,
		}, nil
	}
	if kind, ok := workingSetKinds[rel]; ok {
		return classification{table: "working_set_versions", family: "working_set", kind: kind}, nil
	}
	if kind, ok := archiveKinds[rel]; ok {
		return classification{table: "archive_versions", family: "archive", kind: kind}, nil
	}
	if kind, ok := taskStateKinds[rel]; ok {
		return classification{table: "task_state_versions", family: "task_state", kind: kind}, nil
	}
	return classification{
		table: "operational_doc_versions", family: "operational_doc", kind: documentKind(rel),
	}, nil
}

func documentKind(name string) string {
	stem := strings.TrimSuffix(strings.ToLower(name), strings.ToLower(filepath.Ext(name)))
	parts := strings.Split(stem, "-")
	if len(parts) >= 4 {
		if _, err := time.Parse("2006-01-02", strings.Join(parts[:3], "-")); err == nil {
			return cleanKind(parts[3])
		}
	}
	if len(parts) > 0 {
		return cleanKind(parts[0])
	}
	return "document"
}

func cleanKind(value string) string {
	value = strings.TrimFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if value == "" {
		return "document"
	}
	return value
}

func validateHomeID(value string) error {
	if value == "" {
		return errors.New("scribe: home-id is required")
	}
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("scribe: invalid home-id %q", value)
	}
	return nil
}

func validateTaskID(value string) error {
	if value == "" || value == "." || value == ".." || strings.Contains(value, "/") {
		return fmt.Errorf("scribe: invalid task id %q", value)
	}
	return nil
}

// HelpLines are deterministic next actions after a one-shot mirror.
func HelpLines() []string {
	return []string{
		"Run `roca-firstmate watch --home <path> --home-id <id> --db <firstmate.db>` for continuous ingest",
		"Run `roca exec 'SELECT home_id, relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions ORDER BY home_id, relative_path, version'` for version history",
		"Run `roca exec 'SELECT id, home_id, payload, created_at FROM plugin_roca_firstmate.wakeups WHERE handled = 0 ORDER BY generation'` for the causal wakeup queue",
	}
}
