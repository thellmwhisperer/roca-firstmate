// Package chart is the on-demand AXI get-or-create for the firstmate chart.
// It is inference-free. It never runs at session-start.
package chart

import (
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
		"Run `roca exec 'SELECT relative_path, version, observed_at FROM plugin_roca_firstmate.working_set_versions WHERE is_current = 1 ORDER BY relative_path'` to list the current working set",
		"Run `roca exec 'SELECT id, destination, kind, created_at FROM plugin_roca_firstmate.wakeups WHERE handled = 0 ORDER BY id'` to list unhandled wakeups",
	}
}

// GetOrCreate returns the chart for db, creating or regenerating it when the
// watermark no longer matches the stored cache.
func GetOrCreate(db *sql.DB, now time.Time) (Result, error) {
	wm, err := watermark(db)
	if err != nil {
		return Result{}, err
	}
	var storedWM, storedBody, storedAt string
	err = db.QueryRow(`SELECT watermark, body, generated_at FROM chart_cache WHERE singleton = 1`).
		Scan(&storedWM, &storedBody, &storedAt)
	switch {
	case err == sql.ErrNoRows:
		return store(db, "created", wm, now)
	case err != nil:
		return Result{}, fmt.Errorf("read chart_cache: %w", err)
	case storedWM == wm:
		var body cachedBody
		if unmarshalErr := json.Unmarshal([]byte(storedBody), &body); unmarshalErr != nil {
			return Result{}, fmt.Errorf("parse cached chart: %w", unmarshalErr)
		}
		return resultOf("cached", storedWM, storedAt, body), nil
	default:
		return store(db, "regenerated", wm, now)
	}
}

func store(db *sql.DB, status, wm string, now time.Time) (Result, error) {
	body, err := snapshot(db)
	if err != nil {
		return Result{}, err
	}
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

func watermark(db *sql.DB) (string, error) {
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
	return b.String(), nil
}

func snapshot(db *sql.DB) (cachedBody, error) {
	var body cachedBody
	homes, err := queryHomes(db)
	if err != nil {
		return cachedBody{}, err
	}
	body.Homes = homes
	body.WorkingSet, err = queryDocs(db, "working_set_versions")
	if err != nil {
		return cachedBody{}, err
	}
	body.Archives, err = queryDocs(db, "archive_versions")
	if err != nil {
		return cachedBody{}, err
	}
	body.TaskState, err = queryDocs(db, "task_state_versions")
	if err != nil {
		return cachedBody{}, err
	}
	body.OperationalDocs, err = queryDocs(db, "operational_doc_versions")
	if err != nil {
		return cachedBody{}, err
	}
	body.Artifacts, err = queryArtifacts(db)
	if err != nil {
		return cachedBody{}, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM wakeups WHERE handled = 0`).Scan(&body.WakeupsUnhandled); err != nil {
		return cachedBody{}, fmt.Errorf("count unhandled wakeups: %w", err)
	}
	return body, nil
}

func queryHomes(db *sql.DB) ([]Home, error) {
	rows, err := db.Query(`SELECT home_id, label, kind FROM homes ORDER BY home_id LIMIT ?`, maxRows)
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

func queryDocs(db *sql.DB, table string) ([]Doc, error) {
	rows, err := db.Query(`SELECT relative_path, document_kind, version, observed_at
		FROM `+table+` WHERE is_current = 1 ORDER BY relative_path LIMIT ?`, maxRows)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", table, err)
	}
	defer rows.Close()
	var out []Doc
	for rows.Next() {
		var doc Doc
		if err := rows.Scan(&doc.RelativePath, &doc.DocumentKind, &doc.Version, &doc.ObservedAt); err != nil {
			return nil, err
		}
		out = append(out, doc)
	}
	return out, rows.Err()
}

func queryArtifacts(db *sql.DB) ([]Artifact, error) {
	rows, err := db.Query(`SELECT home_id, task_id, relative_path, document_kind, version, observed_at
		FROM task_artifact_versions WHERE is_current = 1 ORDER BY relative_path LIMIT ?`, maxRows)
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

// RenderTOON paints the bounded AXI text form.
func RenderTOON(result Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s\n", result.Status)
	fmt.Fprintf(&b, "watermark: %s\n", toonString(result.Watermark))
	fmt.Fprintf(&b, "generated_at: %s\n", result.GeneratedAt)
	writeTable(&b, "homes", []string{"home_id", "label", "kind"}, homesRows(result.Homes))
	writeTable(&b, "working_set", []string{"relative_path", "document_kind", "version", "observed_at"}, docRows(result.WorkingSet))
	writeTable(&b, "archives", []string{"relative_path", "document_kind", "version", "observed_at"}, docRows(result.Archives))
	writeTable(&b, "task_state", []string{"relative_path", "document_kind", "version", "observed_at"}, docRows(result.TaskState))
	writeTable(&b, "operational_docs", []string{"relative_path", "document_kind", "version", "observed_at"}, docRows(result.OperationalDocs))
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
		rows[i] = []string{doc.RelativePath, doc.DocumentKind, strconv.Itoa(doc.Version), doc.ObservedAt}
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
