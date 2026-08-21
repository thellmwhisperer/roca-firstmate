// Package chart is the on-demand AXI get-or-create for the firstmate chart.
// It is inference-free. It never runs at session-start.
package chart

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const maxRows = 20

// Result is the AXI chart envelope.
type Result struct {
	Status           string     `json:"status"`
	Watermark        string     `json:"watermark"`
	GeneratedAt      string     `json:"generated_at"`
	Homes            []Home     `json:"homes"`
	WorkingSet       []Doc      `json:"working_set"`
	Archives         []Doc      `json:"archives"`
	TaskState        []Doc      `json:"task_state"`
	OperationalDocs  []Doc      `json:"operational_docs"`
	Artifacts        []Artifact `json:"artifacts"`
	WakeupsUnhandled int        `json:"wakeups_unhandled"`
	Help             []string   `json:"help"`
}

// Home is one mirrored firstmate home.
type Home struct {
	HomeID string `json:"home_id"`
	Label  string `json:"label"`
	Kind   string `json:"kind"`
}

// Doc is a current markdown file identity row, never the full content.
type Doc struct {
	HomeID       string `json:"home_id"`
	RelativePath string `json:"relative_path"`
	DocumentKind string `json:"document_kind"`
	Version      int    `json:"version"`
	ObservedAt   string `json:"observed_at"`
}

// Artifact is a current per-task file identity row.
type Artifact struct {
	HomeID       string `json:"home_id"`
	TaskID       string `json:"task_id"`
	RelativePath string `json:"relative_path"`
	DocumentKind string `json:"document_kind"`
	Version      int    `json:"version"`
	ObservedAt   string `json:"observed_at"`
}

type cachedBody struct {
	Homes            []Home     `json:"homes"`
	WorkingSet       []Doc      `json:"working_set"`
	Archives         []Doc      `json:"archives"`
	TaskState        []Doc      `json:"task_state"`
	OperationalDocs  []Doc      `json:"operational_docs"`
	Artifacts        []Artifact `json:"artifacts"`
	WakeupsUnhandled int        `json:"wakeups_unhandled"`
}

// HelpLines are the deterministic next commands after a chart.
func HelpLines() []string {
	return []string{
		"Run `roca-firstmate chart --json` for the complete envelope",
		"Run `roca exec 'SELECT home_id, relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions WHERE is_current = 1 ORDER BY home_id, relative_path'` to list the current working set",
		"Run `roca exec 'SELECT id, destination, kind, created_at FROM plugin_roca_firstmate.wakeups WHERE handled = 0 ORDER BY id'` to list unhandled wakeups",
	}
}

// Filter restricts a chart to one registered home. An empty homeID keeps every
// registered home.
func Filter(db *sql.DB, result Result, homeID string) (Result, error) {
	homeID = strings.TrimSpace(homeID)
	if homeID == "" {
		return result, nil
	}
	return FilterHomes(db, result, []string{homeID})
}

// FilterHomes restricts a chart to the given registered home ids. An empty
// list keeps every registered home.
func FilterHomes(db *sql.DB, result Result, homeIDs []string) (Result, error) {
	seen := map[string]struct{}{}
	filteredIDs := make([]string, 0, len(homeIDs))
	for _, id := range homeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM homes WHERE home_id = ?`, id).Scan(&exists); err != nil {
			return Result{}, fmt.Errorf("read home %s: %w", id, err)
		}
		if exists == 0 {
			return Result{}, fmt.Errorf("home-id %q is not registered", id)
		}
		seen[id] = struct{}{}
		filteredIDs = append(filteredIDs, id)
	}
	if len(filteredIDs) == 0 {
		return result, nil
	}
	body, err := snapshot(db, filteredIDs)
	if err != nil {
		return Result{}, err
	}
	return resultOf(result.Status, result.Watermark, result.GeneratedAt, body), nil
}

// GetOrCreate returns the chart for db, creating or regenerating it when the
// watermark no longer matches the stored cache.
func GetOrCreate(db *sql.DB, now time.Time) (Result, error) {
	body, err := snapshot(db, nil)
	if err != nil {
		return Result{}, err
	}
	wm, err := watermark(db, body)
	if err != nil {
		return Result{}, err
	}
	var storedWM, storedBody, storedAt string
	err = db.QueryRow(`SELECT watermark, body, generated_at FROM chart_cache WHERE singleton = 1`).
		Scan(&storedWM, &storedBody, &storedAt)
	switch {
	case err == sql.ErrNoRows:
		return store(db, "created", wm, body, now)
	case err != nil:
		return Result{}, fmt.Errorf("read chart_cache: %w", err)
	case storedWM == wm:
		var body cachedBody
		if unmarshalErr := json.Unmarshal([]byte(storedBody), &body); unmarshalErr != nil {
			return Result{}, fmt.Errorf("parse cached chart: %w", unmarshalErr)
		}
		return resultOf("cached", storedWM, storedAt, body), nil
	default:
		return store(db, "regenerated", wm, body, now)
	}
}

func store(db *sql.DB, status, wm string, body cachedBody, now time.Time) (Result, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Result{}, err
	}
	generatedAt := now.UTC().Format(time.RFC3339)
	_, err = db.Exec(`INSERT INTO chart_cache (singleton, watermark, body, generated_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(singleton) DO UPDATE SET
			watermark = excluded.watermark,
			body = excluded.body,
			generated_at = excluded.generated_at`, wm, string(raw), generatedAt)
	if err != nil {
		return Result{}, fmt.Errorf("store chart_cache: %w", err)
	}
	return resultOf(status, wm, generatedAt, body), nil
}

func resultOf(status, wm, generatedAt string, body cachedBody) Result {
	if body.Homes == nil {
		body.Homes = []Home{}
	}
	if body.WorkingSet == nil {
		body.WorkingSet = []Doc{}
	}
	if body.Archives == nil {
		body.Archives = []Doc{}
	}
	if body.TaskState == nil {
		body.TaskState = []Doc{}
	}
	if body.OperationalDocs == nil {
		body.OperationalDocs = []Doc{}
	}
	if body.Artifacts == nil {
		body.Artifacts = []Artifact{}
	}
	return Result{
		Status:           status,
		Watermark:        wm,
		GeneratedAt:      generatedAt,
		Homes:            body.Homes,
		WorkingSet:       body.WorkingSet,
		Archives:         body.Archives,
		TaskState:        body.TaskState,
		OperationalDocs:  body.OperationalDocs,
		Artifacts:        body.Artifacts,
		WakeupsUnhandled: body.WakeupsUnhandled,
		Help:             HelpLines(),
	}
}

func watermark(db *sql.DB, body cachedBody) (string, error) {
	type part struct {
		name string
		sql  string
	}
	parts := []part{
		{"homes", `SELECT COUNT(*), COALESCE(MAX(recorded_at), '') FROM homes`},
		{"tasks", `SELECT COUNT(*), '' FROM tasks`},
		{"working_set_versions", `SELECT COUNT(*), CAST(COALESCE(MAX(id), 0) AS TEXT) FROM working_set_versions`},
		{"archive_versions", `SELECT COUNT(*), CAST(COALESCE(MAX(id), 0) AS TEXT) FROM archive_versions`},
		{"task_state_versions", `SELECT COUNT(*), CAST(COALESCE(MAX(id), 0) AS TEXT) FROM task_state_versions`},
		{"operational_doc_versions", `SELECT COUNT(*), CAST(COALESCE(MAX(id), 0) AS TEXT) FROM operational_doc_versions`},
		{"task_artifact_versions", `SELECT COUNT(*), CAST(COALESCE(MAX(id), 0) AS TEXT) FROM task_artifact_versions`},
		{"wakeups", `SELECT COUNT(*), CAST(COALESCE(MAX(id), 0) AS TEXT) FROM wakeups`},
		{"wakeups_unhandled", `SELECT COUNT(*), '' FROM wakeups WHERE handled = 0`},
	}
	var b strings.Builder
	for i, part := range parts {
		var count int
		var max string
		if err := db.QueryRow(part.sql).Scan(&count, &max); err != nil {
			return "", fmt.Errorf("watermark %s: %w", part.name, err)
		}
		if i > 0 {
			b.WriteByte('|')
		}
		fmt.Fprintf(&b, "%s:%d:%s", part.name, count, max)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("watermark snapshot: %w", err)
	}
	fmt.Fprintf(&b, "|snapshot:%x", sha256.Sum256(raw))
	return b.String(), nil
}

func snapshot(db *sql.DB, homeIDs []string) (cachedBody, error) {
	var body cachedBody
	homes, err := queryHomes(db, homeIDs)
	if err != nil {
		return cachedBody{}, err
	}
	body.Homes = homes
	body.WorkingSet, err = queryDocs(db, "working_set_versions", homeIDs)
	if err != nil {
		return cachedBody{}, err
	}
	body.Archives, err = queryDocs(db, "archive_versions", homeIDs)
	if err != nil {
		return cachedBody{}, err
	}
	body.TaskState, err = queryDocs(db, "task_state_versions", homeIDs)
	if err != nil {
		return cachedBody{}, err
	}
	body.OperationalDocs, err = queryDocs(db, "operational_doc_versions", homeIDs)
	if err != nil {
		return cachedBody{}, err
	}
	body.Artifacts, err = queryArtifacts(db, homeIDs)
	if err != nil {
		return cachedBody{}, err
	}
	predicate, args := homePredicate(homeIDs)
	query := `SELECT COUNT(*) FROM wakeups WHERE handled = 0`
	if predicate != "" {
		query += ` AND ` + predicate
	}
	if err := db.QueryRow(query, args...).Scan(&body.WakeupsUnhandled); err != nil {
		return cachedBody{}, fmt.Errorf("count unhandled wakeups: %w", err)
	}
	return body, nil
}

func queryHomes(db *sql.DB, homeIDs []string) ([]Home, error) {
	predicate, args := homePredicate(homeIDs)
	query := `SELECT home_id, label, kind FROM homes`
	if predicate != "" {
		query += ` WHERE ` + predicate
	}
	query += ` ORDER BY home_id LIMIT ?`
	args = append(args, maxRows)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("homes: %w", err)
	}
	defer rows.Close()
	var out []Home
	for rows.Next() {
		var home Home
		if err := rows.Scan(&home.HomeID, &home.Label, &home.Kind); err != nil {
			return nil, err
		}
		out = append(out, home)
	}
	return out, rows.Err()
}

func queryDocs(db *sql.DB, table string, homeIDs []string) ([]Doc, error) {
	predicate, args := homePredicate(homeIDs)
	query := `SELECT home_id, relative_path, document_kind, version, observed_at
		FROM ` + table + ` WHERE is_current = 1`
	if predicate != "" {
		query += ` AND ` + predicate
	}
	query += ` ORDER BY home_id, relative_path LIMIT ?`
	args = append(args, maxRows)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", table, err)
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		var doc Doc
		if err := rows.Scan(&doc.HomeID, &doc.RelativePath, &doc.DocumentKind, &doc.Version, &doc.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

func queryArtifacts(db *sql.DB, homeIDs []string) ([]Artifact, error) {
	predicate, args := homePredicate(homeIDs)
	query := `SELECT home_id, task_id, relative_path, document_kind, version, observed_at
		FROM task_artifact_versions WHERE is_current = 1`
	if predicate != "" {
		query += ` AND ` + predicate
	}
	query += ` ORDER BY home_id, task_id, relative_path LIMIT ?`
	args = append(args, maxRows)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("task_artifact_versions: %w", err)
	}
	defer rows.Close()
	var out []Artifact
	for rows.Next() {
		var art Artifact
		if err := rows.Scan(&art.HomeID, &art.TaskID, &art.RelativePath, &art.DocumentKind, &art.Version, &art.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, art)
	}
	return out, rows.Err()
}

func homePredicate(homeIDs []string) (string, []any) {
	if len(homeIDs) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(homeIDs))
	args := make([]any, len(homeIDs))
	for i, id := range homeIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	return `home_id IN (` + strings.Join(placeholders, ",") + `)`, args
}

// RenderTOON paints the bounded AXI text form.
func RenderTOON(result Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", result.Status)
	fmt.Fprintf(&b, "watermark: %s\n", toonString(result.Watermark))
	fmt.Fprintf(&b, "generated_at: %s\n", result.GeneratedAt)
	writeTable(&b, "homes", []string{"home_id", "label", "kind"}, homesRows(result.Homes))
	writeTable(&b, "working_set", []string{"home_id", "relative_path", "document_kind", "version", "observed_at"}, docRows(result.WorkingSet))
	writeTable(&b, "archives", []string{"home_id", "relative_path", "document_kind", "version", "observed_at"}, docRows(result.Archives))
	writeTable(&b, "task_state", []string{"home_id", "relative_path", "document_kind", "version", "observed_at"}, docRows(result.TaskState))
	writeTable(&b, "operational_docs", []string{"home_id", "relative_path", "document_kind", "version", "observed_at"}, docRows(result.OperationalDocs))
	writeTable(&b, "artifacts", []string{"home_id", "task_id", "relative_path", "document_kind", "version", "observed_at"}, artifactRows(result.Artifacts))
	fmt.Fprintf(&b, "wakeups_unhandled: %d\n", result.WakeupsUnhandled)
	fmt.Fprintf(&b, "help[%d]:", len(result.Help))
	for _, line := range result.Help {
		b.WriteString("\n  - ")
		b.WriteString(toonString(line))
	}
	b.WriteByte('\n')
	return b.String()
}

// RenderJSON paints the complete envelope.
func RenderJSON(result Result) ([]byte, error) {
	result.Help = HelpLines()
	return json.MarshalIndent(result, "", "  ")
}

func homesRows(homes []Home) [][]string {
	rows := make([][]string, len(homes))
	for i, home := range homes {
		rows[i] = []string{home.HomeID, home.Label, home.Kind}
	}
	return rows
}

func docRows(docs []Doc) [][]string {
	rows := make([][]string, len(docs))
	for i, doc := range docs {
		rows[i] = []string{doc.HomeID, doc.RelativePath, doc.DocumentKind, strconv.Itoa(doc.Version), doc.ObservedAt}
	}
	return rows
}

func artifactRows(arts []Artifact) [][]string {
	rows := make([][]string, len(arts))
	for i, art := range arts {
		rows[i] = []string{art.HomeID, art.TaskID, art.RelativePath, art.DocumentKind, strconv.Itoa(art.Version), art.ObservedAt}
	}
	return rows
}

func writeTable(b *strings.Builder, name string, columns []string, rows [][]string) {
	if len(rows) == 0 {
		fmt.Fprintf(b, "%s: none\n", name)
		return
	}
	fmt.Fprintf(b, "%s[%d]{", name, len(rows))
	b.WriteString(strings.Join(columns, ","))
	b.WriteString("}:")
	for _, row := range rows {
		b.WriteString("\n  ")
		for i, cell := range row {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(toonString(cell))
		}
	}
	b.WriteByte('\n')
}

func toonString(value string) string {
	if value == "" || value == "true" || value == "false" || value == "null" ||
		strings.Trim(value, " \t") != value || strings.ContainsAny(value, ",:\"\\[]{}\n\r\t") ||
		strings.HasPrefix(value, "-") || strings.HasPrefix(value, "#") {
		return quoteTOON(value)
	}
	return value
}

func quoteTOON(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			out.WriteString(`\\`)
		case '"':
			out.WriteString(`\"`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			out.WriteRune(r)
		}
	}
	out.WriteByte('"')
	return out.String()
}
