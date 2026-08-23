package main

import (
	"context"
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchResidentCatchUpDiesWithStdinAndInheritsLease(t *testing.T) {
	clearHomeEnv(t)
	t.Setenv("ROCA_FIRSTMATE_WATCH_LEASE", "2s")
	t.Setenv("ROCA_FIRSTMATE_WATCH_RETRY", "50ms")
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	captain := filepath.Join(home, "data", "captain.md")

	first := startWatch(t, path, home, "northwind-harbor")
	waitWatching(t, first)
	assertWatchLog(t, path, `"kind":"raise"`, `"kind":"lease-acquired"`, `"kind":"sweep"`)
	if err := os.WriteFile(captain, []byte("# live session write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, path, "northwind-harbor", "captain.md", 2)
	if err := first.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, first.done, 3*time.Second)

	if err := os.WriteFile(captain, []byte("# missed while nobody listened\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := currentVersions(t, path, "northwind-harbor", "captain.md"); got != 2 {
		t.Fatalf("idle write landed version %d", got)
	}

	second := startWatch(t, path, home, "northwind-harbor")
	waitWatching(t, second)
	waitVersion(t, path, "northwind-harbor", "captain.md", 3)
	if err := second.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, second.done, 3*time.Second)
}

func TestWatchResidentSingleFlightAndTakeover(t *testing.T) {
	clearHomeEnv(t)
	t.Setenv("ROCA_FIRSTMATE_WATCH_LEASE", "2s")
	t.Setenv("ROCA_FIRSTMATE_WATCH_RETRY", "50ms")
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	captain := filepath.Join(home, "data", "captain.md")

	holder := startWatch(t, path, home, "northwind-harbor")
	waitWatching(t, holder)
	standin := startWatch(t, path, home, "northwind-harbor")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if holders := watchHolders(t, path, "northwind-harbor"); holders == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if holders := watchHolders(t, path, "northwind-harbor"); holders != 1 {
		standin.stdin.Close()
		holder.stdin.Close()
		t.Fatalf("holders = %d, want 1", holders)
	}

	if err := os.WriteFile(captain, []byte("# one holder only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, path, "northwind-harbor", "captain.md", 2)
	if err := holder.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, holder.done, 3*time.Second)
	waitWatching(t, standin)
	if err := os.WriteFile(captain, []byte("# inherited session write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, path, "northwind-harbor", "captain.md", 3)
	if err := standin.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, standin.done, 3*time.Second)
}

func TestWatchResidentRetriesFailedIngestWithCatchUp(t *testing.T) {
	clearHomeEnv(t)
	t.Setenv("ROCA_FIRSTMATE_WATCH_LEASE", "2s")
	t.Setenv("ROCA_FIRSTMATE_WATCH_RETRY", "100ms")
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	captain := filepath.Join(home, "data", "captain.md")
	proc := startWatch(t, path, home, "northwind-harbor")
	waitWatching(t, proc)

	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fabricated_ingest_failure
		BEFORE INSERT ON working_set_versions
		WHEN NEW.relative_path = 'captain.md' AND NEW.version = 2
		BEGIN SELECT RAISE(ABORT, 'fabricated transient failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(captain, []byte("# transient failure then catch-up\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitWatchLog(t, path, `"kind":"crash-retry"`, `"kind":"lease-lost"`)
	select {
	case code := <-proc.done:
		t.Fatalf("watch exited %d instead of retrying", code)
	default:
	}
	if _, err := db.Exec(`DROP TRIGGER fabricated_ingest_failure`); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, path, "northwind-harbor", "captain.md", 2)
	if err := proc.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, proc.done, 3*time.Second)
}

func TestAttachAndFollowRejectWatchLeaseSeatID(t *testing.T) {
	clearHomeEnv(t)
	for _, verb := range []string{"attach", "follow"} {
		t.Run(verb, func(t *testing.T) {
			path := appliedDBPath(t)
			home := fabricatedHome(t, "northwind-harbor")
			stdout := &safeBuffer{}
			stderr := &safeBuffer{}
			code := runContext(context.Background(), []string{
				verb, "--db", path, "--home", home, "--home-id", "northwind-harbor",
				"--seat-id", "watch-northwind-harbor",
			}, nil, stdout, stderr)
			if code != exitError || !strings.Contains(stdout.String(), "reserved") {
				t.Fatalf("%s code=%d stdout=%q stderr=%q", verb, code, stdout.String(), stderr.String())
			}
		})
	}
}

func TestRenewFailureStandsDownWatchHome(t *testing.T) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	source := &closingWatchSource{events: make(chan string), errors: make(chan error)}
	home := &leasedHome{
		token: "holder", seatID: "watch-northwind-harbor", source: source, holding: true,
	}
	now := time.Now().UTC()
	err = renewWatchHomes(context.Background(), db, []*leasedHome{home}, now, time.Minute, 50*time.Millisecond, nil)
	if err == nil {
		t.Fatal("renew against closed database succeeded")
	}
	if home.holding || home.source != nil || !source.closed {
		t.Fatalf("renew failure left watcher active: holding=%v source=%v closed=%v", home.holding, home.source, source.closed)
	}
	if home.retryAt.Before(now.Add(50 * time.Millisecond)) {
		t.Fatalf("renew failure did not back off until %s", home.retryAt)
	}
}

type closingWatchSource struct {
	events chan string
	errors chan error
	closed bool
}

func (s *closingWatchSource) Backend() string       { return "test" }
func (s *closingWatchSource) Events() <-chan string { return s.events }
func (s *closingWatchSource) Errors() <-chan error  { return s.errors }
func (s *closingWatchSource) Close() error          { s.closed = true; return nil }

type watchProc struct {
	stdin io.WriteCloser
	out   *safeBuffer
	err   *safeBuffer
	done  chan int
}

func startWatch(t *testing.T, dbPath, home, homeID string) *watchProc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	stdinR, stdinW := io.Pipe()
	t.Cleanup(func() { _ = stdinW.Close(); _ = stdinR.Close() })
	proc := &watchProc{stdin: stdinW, out: &safeBuffer{}, err: &safeBuffer{}, done: make(chan int, 1)}
	go func() {
		proc.done <- runContext(ctx, []string{
			"watch", "--db", dbPath, "--json", "--poll-interval", "20ms",
			"--home", home, "--home-id", homeID,
		}, stdinR, proc.out, proc.err)
	}()
	return proc
}

func waitWatching(t *testing.T, proc *watchProc) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(proc.out.String(), `"status":"watching"`) {
			return
		}
		select {
		case code := <-proc.done:
			t.Fatalf("watch exited %d before start stdout %s stderr %s", code, proc.out.String(), proc.err.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watch did not start stdout %s stderr %s", proc.out.String(), proc.err.String())
}

func waitExit(t *testing.T, done <-chan int, timeout time.Duration) {
	t.Helper()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("watch exit %d, want 0", code)
		}
	case <-time.After(timeout):
		t.Fatal("watch outlived stdin close")
	}
}

func waitVersion(t *testing.T, dbPath, homeID, relative string, want int) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		got = currentVersions(t, dbPath, homeID, relative)
		if got == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("versions of %s = %d, want %d", relative, got, want)
}

func currentVersions(t *testing.T, dbPath, homeID, relative string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM working_set_versions WHERE home_id = ? AND relative_path = ?`,
		homeID, relative).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertWatchLog(t *testing.T, dbPath string, want ...string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "logs", "watch-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("watch wrote no JSONL log")
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, snippet := range want {
		if !strings.Contains(text, snippet) {
			t.Fatalf("watch log missing %s in %s", snippet, text)
		}
	}
}

func waitWatchLog(t *testing.T, dbPath string, want ...string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "logs", "watch-*.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) > 0 {
			body, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatal(err)
			}
			found := true
			for _, snippet := range want {
				found = found && strings.Contains(string(body), snippet)
			}
			if found {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watch log did not contain %v", want)
}

func watchHolders(t *testing.T, dbPath, homeID string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM seats
		WHERE seat_id = ? AND lease_until > ?`,
		"watch-"+homeID, time.Now().UTC().Format(time.RFC3339Nano)).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
