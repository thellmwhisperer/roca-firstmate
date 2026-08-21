package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/nerve"
	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

func TestUnbalancedHomeFlagsExitUsage(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"scribe", "--db", path, "--home", home, "--home-id", "northwind-harbor", "--home-id", "skiff-secondmate",
	}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("exit %d, want %d stderr %s stdout %s", code, exitUsage, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "--home") || !strings.Contains(stderr.String(), "--home-id") {
		t.Fatalf("unbalanced error stderr %s", stderr.String())
	}
}

func TestScribeTwoHomesTagsWakeupsByHomeID(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	harbor := fabricatedHome(t, "northwind-harbor")
	skiff := fabricatedHome(t, "skiff-secondmate")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"scribe", "--db", path,
		"--home", harbor, "--home-id", "northwind-harbor", "--kind", "primary",
		"--home", skiff, "--home-id", "skiff-secondmate", "--kind", "secondmate",
		"--json",
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d stderr %s stdout %s", code, stderr.String(), stdout.String())
	}
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	var summaries []map[string]any
	for {
		var summary map[string]any
		if err := dec.Decode(&summary); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("scribe json: %v\n%s", err, stdout.String())
		}
		summaries = append(summaries, summary)
	}
	if len(summaries) != 2 {
		t.Fatalf("summaries = %d, want 2 stdout %s", len(summaries), stdout.String())
	}
	if summaries[0]["home_id"] != "northwind-harbor" || summaries[1]["home_id"] != "skiff-secondmate" {
		t.Fatalf("summary home ids %+v", summaries)
	}

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertHomeKind(t, db, "northwind-harbor", "primary")
	assertHomeKind(t, db, "skiff-secondmate", "secondmate")
	assertWakeupHomes(t, db, "northwind-harbor", "skiff-secondmate")
}

func TestSingleHomeScribeStaysUnwrapped(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"scribe", "--db", path, "--home", home, "--home-id", "northwind-harbor",
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("exit %d stderr %s", code, stderr.String())
	}
	text := stdout.String()
	if !strings.HasPrefix(text, "status: mirrored\nhome_id: northwind-harbor\n") {
		t.Fatalf("single-home scribe shape changed:\n%s", text)
	}
	if strings.Contains(text, "homes[") {
		t.Fatalf("single-home scribe wrapped homes: %s", text)
	}
}

func TestChartHomeIDFilterAndFollowInterleave(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	harbor := fabricatedHome(t, "northwind-harbor")
	skiff := fabricatedHome(t, "skiff-secondmate")
	var ignored bytes.Buffer
	if code := run([]string{
		"scribe", "--db", path,
		"--home", harbor, "--home-id", "northwind-harbor",
		"--home", skiff, "--home-id", "skiff-secondmate", "--kind", "secondmate",
	}, &ignored, &ignored); code != exitOK {
		t.Fatalf("scribe exit %d", code)
	}

	var chartOut, chartErr bytes.Buffer
	if code := run([]string{"chart", "--db", path, "--home-id", "skiff-secondmate"}, &chartOut, &chartErr); code != exitOK {
		t.Fatalf("chart filter exit %d stderr %s stdout %s", code, chartErr.String(), chartOut.String())
	}
	text := chartOut.String()
	if !strings.Contains(text, "skiff-secondmate") || strings.Contains(text, "northwind-harbor") {
		t.Fatalf("chart filter mixed homes:\n%s", text)
	}

	var all bytes.Buffer
	if code := run([]string{"chart", "--db", path}, &all, &ignored); code != exitOK {
		t.Fatalf("chart all exit %d", code)
	}
	if !strings.Contains(all.String(), "northwind-harbor") || !strings.Contains(all.String(), "skiff-secondmate") {
		t.Fatalf("chart default missed a home:\n%s", all.String())
	}

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var drained bytes.Buffer
	count, err := nerve.DrainMatching(context.Background(), db, []string{"companion"}, nil, &drained)
	if err != nil {
		t.Fatal(err)
	}
	if count < 2 {
		t.Fatalf("drained %d, want both homes", count)
	}
	sawHarbor, sawSkiff := false, false
	for _, line := range strings.Split(strings.TrimSpace(drained.String()), "\n") {
		if line == "" {
			continue
		}
		var wakeup nerve.Wakeup
		if err := json.Unmarshal([]byte(line), &wakeup); err != nil {
			t.Fatalf("follow line %q: %v", line, err)
		}
		switch wakeup.HomeID {
		case "northwind-harbor":
			sawHarbor = true
		case "skiff-secondmate":
			sawSkiff = true
		}
	}
	if !sawHarbor || !sawSkiff {
		t.Fatalf("follow did not interleave both homes:\n%s", drained.String())
	}

	var filtered bytes.Buffer
	if _, err := nerve.DrainMatching(context.Background(), db, []string{"companion"}, []string{"skiff-secondmate"}, &filtered); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(filtered.String(), "northwind-harbor") {
		t.Fatalf("home-id filter leaked harbor:\n%s", filtered.String())
	}
}

func TestWatchTwoHomesTagsTouchedFiles(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	harbor := fabricatedHome(t, "northwind-harbor")
	skiff := fabricatedHome(t, "skiff-secondmate")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &safeBuffer{}
	errBuf := &safeBuffer{}
	done := make(chan int, 1)
	go func() {
		done <- runContext(ctx, []string{
			"watch", "--db", path, "--json", "--poll-interval", "20ms",
			"--home", harbor, "--home-id", "northwind-harbor", "--kind", "primary",
			"--home", skiff, "--home-id", "skiff-secondmate", "--kind", "secondmate",
		}, out, errBuf)
	}()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), `"status":"watching"`) {
			break
		}
		if strings.Contains(out.String(), `"error"`) || strings.Contains(errBuf.String(), "error:") {
			cancel()
			<-done
			t.Fatalf("watch failed before start stdout %s stderr %s", out.String(), errBuf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out.String(), `"status":"watching"`) {
		cancel()
		<-done
		t.Fatalf("watch did not start stdout %s stderr %s", out.String(), errBuf.String())
	}

	if err := os.WriteFile(filepath.Join(harbor, "data", "captain.md"), []byte("# Harbor touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skiff, "data", "captain.md"), []byte("# Skiff touch\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sawHarbor, sawSkiff := false, false
	for time.Now().Before(deadline) {
		text := out.String()
		sawHarbor = strings.Contains(text, `"home_id":"northwind-harbor"`) && strings.Contains(text, `"relative_path":"captain.md"`)
		sawSkiff = strings.Contains(text, `"home_id":"skiff-secondmate"`) && strings.Contains(text, `"relative_path":"captain.md"`)
		if sawHarbor && sawSkiff {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	cancel()
	code := <-done
	if code != exitOK && code != 0 {
		t.Fatalf("watch exit %d stderr %s stdout %s", code, errBuf.String(), out.String())
	}
	if !sawHarbor || !sawSkiff {
		t.Fatalf("watch missed a home touch stdout %s", out.String())
	}

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	assertWakeupHomes(t, db, "northwind-harbor", "skiff-secondmate")
	assertLatestCaptainHome(t, db, "northwind-harbor")
	assertLatestCaptainHome(t, db, "skiff-secondmate")
}

func TestTickEvaluatesSilencePerHome(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	harbor := fabricatedHome(t, "northwind-harbor")
	skiff := fabricatedHome(t, "skiff-secondmate")
	var ignored bytes.Buffer
	if code := run([]string{
		"scribe", "--db", path,
		"--home", harbor, "--home-id", "northwind-harbor",
		"--home", skiff, "--home-id", "skiff-secondmate", "--kind", "secondmate",
	}, &ignored, &ignored); code != exitOK {
		t.Fatalf("scribe exit %d", code)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stamp := time.Date(2026, 3, 14, 9, 50, 0, 0, time.UTC).Format(time.RFC3339Nano)
	lease := time.Date(2026, 3, 14, 9, 52, 0, 0, time.UTC).Format(time.RFC3339Nano)
	mustExec(t, db, `INSERT INTO seats (
		seat_id, home_id, workspace_fingerprint, label, destination,
		attached_at, last_seen_at, lease_until, silence_generation
	) VALUES
	('seat-harbor', 'northwind-harbor', 'aa', 'harbor', 'companion', ?, ?, ?, 0),
	('seat-skiff', 'skiff-secondmate', 'bb', 'skiff', 'companion', ?, ?, ?, 0)`,
		stamp, stamp, lease, stamp, stamp, lease)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"tick", "--db", path, "--silence-after", "5m", "--json",
		"--home", harbor, "--home-id", "northwind-harbor",
		"--home", skiff, "--home-id", "skiff-secondmate", "--kind", "secondmate",
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("tick exit %d stderr %s stdout %s", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), `"silence_wakeups":2`) && !strings.Contains(stdout.String(), `"silence_wakeups": 2`) {
		t.Fatalf("tick did not silence both homes:\n%s", stdout.String())
	}

	db, err = sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT home_id FROM wakeups WHERE kind = 'seat-silent' ORDER BY home_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var homes []string
	for rows.Next() {
		var homeID string
		if err := rows.Scan(&homeID); err != nil {
			t.Fatal(err)
		}
		homes = append(homes, homeID)
	}
	if strings.Join(homes, ",") != "northwind-harbor,skiff-secondmate" {
		t.Fatalf("silence home_ids %v", homes)
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func clearHomeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("FIRSTMATE_HOME", "")
	t.Setenv("FIRSTMATE_HOME_ID", "")
	t.Setenv("ROCA_FIRSTMATE_DB", "")
}

func appliedDBPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "firstmate.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Apply(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func fabricatedHome(t *testing.T, name string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate fabricated fixture")
	}
	source := filepath.Join(filepath.Dir(file), "..", "..", "testdata", "homes", name)
	destination := filepath.Join(t.TempDir(), name)
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, body, 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	return destination
}

func assertHomeKind(t *testing.T, db *sql.DB, homeID, kind string) {
	t.Helper()
	var got string
	if err := db.QueryRow(`SELECT kind FROM homes WHERE home_id = ?`, homeID).Scan(&got); err != nil {
		t.Fatalf("home %s: %v", homeID, err)
	}
	if got != kind {
		t.Fatalf("home %s kind %q, want %q", homeID, got, kind)
	}
}

func assertWakeupHomes(t *testing.T, db *sql.DB, want ...string) {
	t.Helper()
	for _, homeID := range want {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM wakeups WHERE home_id = ?`, homeID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatalf("no wakeups tagged %s", homeID)
		}
	}
}

func assertLatestCaptainHome(t *testing.T, db *sql.DB, homeID string) {
	t.Helper()
	var version int
	if err := db.QueryRow(`SELECT version FROM working_set_versions
		WHERE home_id = ? AND relative_path = 'captain.md' AND is_current = 1`, homeID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version < 2 {
		t.Fatalf("%s captain version %d, want a touch rewrite", homeID, version)
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, query)
	}
}
