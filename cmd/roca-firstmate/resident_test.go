package main

import (
	"context"
	"database/sql"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/nerve"
	"github.com/thellmwhisperer/roca-firstmate/internal/scribe"
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

func TestWatchTakeoverRefreshesCachedFingerprintState(t *testing.T) {
	path := appliedDBPath(t)
	db, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	homePath := fabricatedHome(t, "northwind-harbor")
	captain := filepath.Join(homePath, "data", "captain.md")
	original, err := os.ReadFile(captain)
	if err != nil {
		t.Fatal(err)
	}
	pair := homePair{Path: homePath, ID: "northwind-harbor"}
	holderIngester, err := newIngester(context.Background(), db, pair, "firstmate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holderIngester.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	holderToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	seatID := nerve.WatchSeatID(pair.ID)
	held, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		SeatID: seatID, HomeID: pair.ID, HolderToken: holderToken,
		Destination: "machine", Now: time.Now().UTC(), Lease: time.Minute,
	})
	if err != nil || !held {
		t.Fatalf("holder lease held=%v err=%v", held, err)
	}
	standbyToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	standby := &leasedHome{
		pair: pair, token: standbyToken, seatID: seatID, sourceAgent: "firstmate",
	}
	msgs := make(chan watchMsg, 16)
	var watches []homeWatch
	var summaries []scribe.Summary
	if err := acquireWatchHomes(
		context.Background(), db, []*leasedHome{standby}, 20*time.Millisecond,
		time.Minute, 20*time.Millisecond, nil, msgs, &watches, &summaries,
	); err != nil {
		t.Fatal(err)
	}
	if len(watches) != 0 || !standby.prepared {
		t.Fatalf("standby preparation watches=%d prepared=%v", len(watches), standby.prepared)
	}
	if err := os.WriteFile(captain, []byte("# intervening holder content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := holderIngester.IngestPath(context.Background(), captain); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(captain, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := nerve.ReleaseSeat(context.Background(), db, seatID, holderToken, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	defer standDownWatchHome(context.Background(), db, standby, 0, nil, time.Time{})
	watches = nil
	summaries = nil
	if err := acquireWatchHomes(
		context.Background(), db, []*leasedHome{standby}, 20*time.Millisecond,
		time.Minute, 20*time.Millisecond, nil, msgs, &watches, &summaries,
	); err != nil {
		t.Fatal(err)
	}
	if len(watches) != 1 || len(summaries) != 1 || summaries[0].Inserted != 1 {
		t.Fatalf("takeover watches=%d summaries=%+v", len(watches), summaries)
	}
	if got := currentVersions(t, path, pair.ID, "captain.md"); got != 3 {
		t.Fatalf("takeover versions=%d, want 3", got)
	}
}

func TestWatchStandbyDoesNotMutateHomeMetadata(t *testing.T) {
	path := appliedDBPath(t)
	db, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	homePath := fabricatedHome(t, "northwind-harbor")
	ownerConfig := scribe.Config{
		Home: homePath, HomeID: "northwind-harbor", Label: "Owner Label", Kind: "primary", SourceAgent: "firstmate",
	}
	if _, err := scribe.New(context.Background(), db, ownerConfig); err != nil {
		t.Fatal(err)
	}
	holderToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	seatID := nerve.WatchSeatID(ownerConfig.HomeID)
	held, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		SeatID: seatID, HomeID: ownerConfig.HomeID, HolderToken: holderToken,
		Destination: "machine", Now: time.Now().UTC(), Lease: time.Minute,
	})
	if err != nil || !held {
		t.Fatalf("holder lease held=%v err=%v", held, err)
	}
	defer nerve.ReleaseSeat(context.Background(), db, seatID, holderToken, time.Now().UTC())
	standbyToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	standby := &leasedHome{
		pair: homePair{
			Path: homePath, ID: ownerConfig.HomeID, Label: "Standby Label", Kind: "secondmate",
		},
		token: standbyToken, seatID: seatID, sourceAgent: "firstmate",
	}
	msgs := make(chan watchMsg, 1)
	var watches []homeWatch
	var summaries []scribe.Summary
	if err := acquireWatchHomes(
		context.Background(), db, []*leasedHome{standby}, 20*time.Millisecond,
		time.Minute, 20*time.Millisecond, nil, msgs, &watches, &summaries,
	); err != nil {
		t.Fatal(err)
	}
	if len(watches) != 0 {
		t.Fatalf("standby activated %d watches", len(watches))
	}
	var label, kind string
	if err := db.QueryRow(`SELECT label, kind FROM homes WHERE home_id = ?`, ownerConfig.HomeID).Scan(&label, &kind); err != nil {
		t.Fatal(err)
	}
	if label != ownerConfig.Label || kind != ownerConfig.Kind {
		t.Fatalf("standby changed home metadata to label=%q kind=%q", label, kind)
	}
}

func TestWatchHomeMetadataUpdateIsLeaseFenced(t *testing.T) {
	path := appliedDBPath(t)
	db, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	homePath := fabricatedHome(t, "northwind-harbor")
	if err := scribe.EnsureHome(context.Background(), db, scribe.Config{
		Home: homePath, HomeID: "northwind-harbor", Label: "Prepared", Kind: "primary",
	}); err != nil {
		t.Fatal(err)
	}
	firstToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seatID := nerve.WatchSeatID("northwind-harbor")
	held, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		SeatID: seatID, HomeID: "northwind-harbor", HolderToken: firstToken,
		Destination: "machine", Now: now, Lease: 2 * time.Second,
	})
	if err != nil || !held {
		t.Fatalf("first lease held=%v err=%v", held, err)
	}
	stale := &leasedHome{
		pair:  homePair{Path: homePath, ID: "northwind-harbor", Label: "Stale Label", Kind: "primary"},
		token: firstToken, seatID: seatID, sourceAgent: "firstmate",
	}
	if _, err := newWatchIngester(context.Background(), db, stale); err != nil {
		t.Fatal(err)
	}
	secondToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	taken, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		SeatID: seatID, HomeID: "northwind-harbor", HolderToken: secondToken,
		Destination: "machine", Now: now.Add(3 * time.Second), Lease: 2 * time.Second,
	})
	if err != nil || !taken {
		t.Fatalf("second lease held=%v err=%v", taken, err)
	}
	current := &leasedHome{
		pair:  homePair{Path: homePath, ID: "northwind-harbor", Label: "Current Label", Kind: "secondmate"},
		token: secondToken, seatID: seatID, sourceAgent: "firstmate",
	}
	if _, err := newWatchIngester(context.Background(), db, current); err != nil {
		t.Fatal(err)
	}
	if _, err := newWatchIngester(context.Background(), db, stale); err == nil {
		t.Fatal("stale holder updated home metadata")
	}
	var label, kind string
	if err := db.QueryRow(`SELECT label, kind FROM homes WHERE home_id = ?`, "northwind-harbor").Scan(&label, &kind); err != nil {
		t.Fatal(err)
	}
	if label != "Current Label" || kind != "secondmate" {
		t.Fatalf("stale metadata committed label=%q kind=%q", label, kind)
	}
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

func TestWatchResidentRetriesRuntimeInitialization(t *testing.T) {
	clearHomeEnv(t)
	t.Setenv("ROCA_FIRSTMATE_WATCH_RETRY", "50ms")
	path := appliedDBPath(t)
	home := filepath.Join(t.TempDir(), "late-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	proc := startWatch(t, path, home, "late-home")
	waitWatchLog(t, path, `"kind":"raise"`, `"kind":"crash-retry"`)
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "logs", "watch-*.jsonl"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("watch telemetry files=%v err=%v", matches, err)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), home) || strings.Contains(string(body), path) {
		t.Fatalf("watch telemetry leaked a configured path: %s", body)
	}
	if !strings.Contains(string(body), `"error":"not-found"`) {
		t.Fatalf("watch telemetry did not record a bounded error code: %s", body)
	}
	select {
	case code := <-proc.done:
		t.Fatalf("watch exited %d instead of retrying initialization", code)
	default:
	}
	if err := os.MkdirAll(filepath.Join(home, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "data", "captain.md"), []byte("# late runtime home\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitWatching(t, proc)
	waitVersion(t, path, "late-home", "captain.md", 1)
	if err := proc.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, proc.done, 3*time.Second)
}

func TestWatchRejectsPermanentDatabaseConfiguration(t *testing.T) {
	clearHomeEnv(t)
	home := fabricatedHome(t, "northwind-harbor")
	for _, test := range []struct {
		name string
		path string
	}{
		{name: "missing", path: filepath.Join(t.TempDir(), "missing.db")},
		{name: "directory", path: t.TempDir()},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout := &safeBuffer{}
			stderr := &safeBuffer{}
			code := runContext(context.Background(), []string{
				"watch", "--db", test.path, "--home", home, "--home-id", "northwind-harbor",
			}, nil, stdout, stderr)
			if code != exitError || stdout.String() == "" {
				t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			matches, err := filepath.Glob(filepath.Join(filepath.Dir(test.path), "logs", "watch-*.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("permanent database configuration entered retry lifecycle: %v", matches)
			}
		})
	}
}

func TestWatchResidentRetriesTransientDatabaseLock(t *testing.T) {
	clearHomeEnv(t)
	t.Setenv("ROCA_FIRSTMATE_WATCH_RETRY", "50ms")
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	locker, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	proc := startWatch(t, path, home, "northwind-harbor")
	waitWatchLog(t, path, `"kind":"raise"`, `"kind":"crash-retry"`)
	select {
	case code := <-proc.done:
		t.Fatalf("watch exited %d instead of retrying database lock", code)
	default:
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false
	waitWatching(t, proc)
	if err := proc.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, proc.done, 3*time.Second)
}

func TestWatchRetriesDatabaseDisappearanceAfterPreflight(t *testing.T) {
	clearHomeEnv(t)
	t.Setenv("ROCA_FIRSTMATE_WATCH_RETRY", "50ms")
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	missing := filepath.Join(t.TempDir(), "disappeared.db")
	var calls atomic.Int64
	openDB := func(path string) (*sql.DB, error) {
		if calls.Add(1) == 1 {
			return openDatabase(missing)
		}
		return openDatabase(path)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdinR, stdinW := io.Pipe()
	defer stdinR.Close()
	defer stdinW.Close()
	proc := &watchProc{stdin: stdinW, out: &safeBuffer{}, err: &safeBuffer{}, done: make(chan int, 1)}
	go func() {
		proc.done <- runWatchWithDatabase(
			ctx, path, []homePair{{Path: home, ID: "northwind-harbor"}},
			scribeFlags{dbPath: path, sourceAgent: "firstmate", asJSON: true, pollInterval: 20 * time.Millisecond},
			stdinR, proc.out, proc.err, openDB,
		)
	}()
	waitWatchLog(t, path, `"kind":"raise"`, `"kind":"crash-retry"`, `"error":"not-found"`)
	waitWatching(t, proc)
	if calls.Load() < 2 {
		t.Fatalf("database open calls=%d, want retry", calls.Load())
	}
	if err := proc.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, proc.done, 3*time.Second)
}

func TestWatchResidentReportsTelemetryFailureOnce(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "logs"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	home := fabricatedHome(t, "northwind-harbor")
	proc := startWatch(t, path, home, "northwind-harbor")
	waitWatching(t, proc)
	if err := proc.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitExit(t, proc.done, 3*time.Second)
	diagnostic := "warning: watch telemetry unavailable"
	if got := strings.Count(proc.err.String(), diagnostic); got != 1 {
		t.Fatalf("telemetry diagnostics = %d, want 1: %q", got, proc.err.String())
	}
	if strings.Contains(proc.err.String(), path) || strings.Contains(proc.err.String(), home) {
		t.Fatalf("telemetry diagnostic leaked configured paths: %q", proc.err.String())
	}
}

func TestWatchIngesterRejectsStaleHolderCommit(t *testing.T) {
	path := appliedDBPath(t)
	db, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	homePath := fabricatedHome(t, "northwind-harbor")
	token, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	home := &leasedHome{
		pair:  homePair{Path: homePath, ID: "northwind-harbor"},
		token: token, seatID: nerve.WatchSeatID("northwind-harbor"), sourceAgent: "firstmate",
	}
	if err := scribe.EnsureHome(context.Background(), db, scribe.Config{
		Home: homePath, HomeID: home.pair.ID, SourceAgent: home.sourceAgent,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	held, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: home.pair.ID, HolderToken: token, Destination: "machine", Now: now, Lease: 2 * time.Second,
	})
	if err != nil || !held {
		t.Fatalf("initial lease held=%v err=%v", held, err)
	}
	ingester, err := newWatchIngester(context.Background(), db, home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingester.Backfill(context.Background()); err != nil {
		t.Fatal(err)
	}
	foreign, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	stolen, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: home.pair.ID, HolderToken: foreign, Destination: "machine",
		Now: now.Add(3 * time.Second), Lease: 2 * time.Second,
	})
	if err != nil || !stolen {
		t.Fatalf("expired lease takeover held=%v err=%v", stolen, err)
	}
	captain := filepath.Join(homePath, "data", "captain.md")
	if err := os.WriteFile(captain, []byte("# stale holder write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ingester.IngestPath(context.Background(), captain); err == nil {
		t.Fatal("stale holder committed an ingest")
	}
	if got := currentVersions(t, path, home.pair.ID, "captain.md"); got != 1 {
		t.Fatalf("stale holder left %d committed versions, want 1", got)
	}
}

func TestWatchRejectsInvalidIdentityBeforeRaise(t *testing.T) {
	clearHomeEnv(t)
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	stdout := &safeBuffer{}
	stderr := &safeBuffer{}
	code := run([]string{
		"watch", "--db", path, "--home", home, "--home-id", "invalid/home",
	}, stdout, stderr)
	if code != exitError || !strings.Contains(stdout.String(), "invalid home-id") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), "logs", "watch-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("invalid configuration entered retry lifecycle: %v", matches)
	}
}

func TestWatchSubprocessSessionLifecycleAndAbruptTakeover(t *testing.T) {
	clearHomeEnv(t)
	binary := buildWatchCLI(t)
	path := appliedDBPath(t)
	home := fabricatedHome(t, "northwind-harbor")
	captain := filepath.Join(home, "data", "captain.md")

	first := startWatchChild(t, binary, path, home, "northwind-harbor")
	waitChildWatching(t, first)
	if err := os.WriteFile(captain, []byte("# subprocess live write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, path, "northwind-harbor", "captain.md", 2)
	if err := first.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitChildExit(t, first, true)
	if first.cmd.ProcessState == nil || !first.cmd.ProcessState.Exited() {
		t.Fatal("session-owned child remained alive after stdin closed")
	}

	if err := os.WriteFile(captain, []byte("# subprocess missed write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := currentVersions(t, path, "northwind-harbor", "captain.md"); got != 2 {
		t.Fatalf("write after child exit landed version %d", got)
	}

	holder := startWatchChild(t, binary, path, home, "northwind-harbor")
	waitChildWatching(t, holder)
	waitVersion(t, path, "northwind-harbor", "captain.md", 3)
	if err := os.WriteFile(captain, []byte("# subprocess resumed live write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, path, "northwind-harbor", "captain.md", 4)

	standby := startWatchChild(t, binary, path, home, "northwind-harbor")
	time.Sleep(150 * time.Millisecond)
	if strings.Contains(standby.out.String(), `"status":"watching"`) {
		t.Fatal("standby activated while the holder was alive")
	}
	if err := holder.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitChildExit(t, holder, false)
	waitChildWatching(t, standby)
	if err := os.WriteFile(captain, []byte("# subprocess takeover write\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitVersion(t, path, "northwind-harbor", "captain.md", 5)
	if err := standby.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	waitChildExit(t, standby, true)
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
	err = renewWatchHomes(context.Background(), db, []*leasedHome{home}, time.Minute, 50*time.Millisecond, nil)
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

func TestDelayedRenewalKeepsOnlyFutureLease(t *testing.T) {
	path := appliedDBPath(t)
	db, err := openDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	homePath := fabricatedHome(t, "northwind-harbor")
	if err := scribe.EnsureHome(context.Background(), db, scribe.Config{
		Home: homePath, HomeID: "northwind-harbor", Label: "Northwind Harbor", Kind: "primary",
	}); err != nil {
		t.Fatal(err)
	}
	token, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	seatID := nerve.WatchSeatID("northwind-harbor")
	held, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		SeatID: seatID, HomeID: "northwind-harbor", HolderToken: token,
		Destination: "machine", Now: time.Now().UTC(), Lease: time.Minute,
	})
	if err != nil || !held {
		t.Fatalf("initial lease held=%v err=%v", held, err)
	}
	home := &leasedHome{token: token, seatID: seatID, holding: true, generation: 1}
	defer standDownWatchHome(context.Background(), db, home, 0, nil, time.Time{})
	locker, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	conn, err := locker.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- renewWatchHomes(context.Background(), db, []*leasedHome{home}, 500*time.Millisecond, 20*time.Millisecond, nil)
	}()
	<-started
	time.Sleep(700 * time.Millisecond)
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	home.mu.Lock()
	stillHolding := home.holding
	deadline := home.leaseUntil
	home.mu.Unlock()
	if !stillHolding || !time.Now().UTC().Before(deadline) {
		t.Fatalf("delayed renewal holding=%v deadline=%s", stillHolding, deadline)
	}
	competitor, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	stolen, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		SeatID: seatID, HomeID: "northwind-harbor", HolderToken: competitor,
		Destination: "machine", Now: time.Now().UTC(), Lease: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stolen {
		t.Fatal("delayed renewal left an immediately acquirable lease")
	}
}

func TestHeartbeatRenewsOtherHomeDuringBlockedSweepAndFencesLoss(t *testing.T) {
	path := appliedDBPath(t)
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`INSERT INTO homes (home_id, label, kind, recorded_at) VALUES
		('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T10:00:00Z'),
		('skiff-secondmate', 'Skiff Secondmate', 'secondmate', '2026-03-14T10:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	firstToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	secondToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	firstUnblock := make(chan struct{})
	close(firstUnblock)
	secondUnblock := make(chan struct{})
	var unblockSecond sync.Once
	t.Cleanup(func() { unblockSecond.Do(func() { close(secondUnblock) }) })
	firstIngester := &blockingWatchIngester{
		root: t.TempDir(), homeID: "northwind-harbor", started: make(chan struct{}, 1), unblock: firstUnblock,
	}
	secondIngester := &blockingWatchIngester{
		root: t.TempDir(), homeID: "skiff-secondmate", started: make(chan struct{}, 1), unblock: secondUnblock,
	}
	homes := []*leasedHome{
		{
			pair: homePair{ID: "northwind-harbor"}, token: firstToken, seatID: nerve.WatchSeatID("northwind-harbor"),
			prepared:    true,
			newIngester: func(context.Context, *sql.DB, *leasedHome) (watchIngester, error) { return firstIngester, nil },
		},
		{
			pair: homePair{ID: "skiff-secondmate"}, token: secondToken, seatID: nerve.WatchSeatID("skiff-secondmate"),
			prepared:    true,
			newIngester: func(context.Context, *sql.DB, *leasedHome) (watchIngester, error) { return secondIngester, nil },
		},
	}
	lease := 120 * time.Millisecond
	retry := 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	var attempts atomic.Int64
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		heartbeatWatchHomes(ctx, db, homes, lease, retry, nil, &attempts)
	}()
	t.Cleanup(func() {
		cancel()
		<-heartbeatDone
	})
	t.Cleanup(func() {
		for _, home := range homes {
			standDownWatchHome(context.Background(), db, home, 0, nil, time.Time{})
		}
	})

	msgs := make(chan watchMsg, 16)
	var watches []homeWatch
	var summaries []scribe.Summary
	acquired := make(chan error, 1)
	acquireDone := make(chan struct{})
	go func() {
		defer close(acquireDone)
		acquired <- acquireWatchHomes(
			ctx, db, homes, 10*time.Millisecond, lease, retry, nil, msgs, &watches, &summaries,
		)
	}()
	t.Cleanup(func() {
		cancel()
		unblockSecond.Do(func() { close(secondUnblock) })
		<-acquireDone
	})
	select {
	case <-secondIngester.started:
	case <-time.After(3 * time.Second):
		t.Fatal("second home sweep did not block")
	}
	time.Sleep(3 * lease)
	competitorToken, err := nerve.NewHolderToken()
	if err != nil {
		t.Fatal(err)
	}
	stolen, _, err := nerve.TryAcquireSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: "northwind-harbor", HolderToken: competitorToken,
		Destination: "machine", Now: time.Now().UTC(), Lease: lease,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stolen {
		t.Fatal("blocked second-home sweep starved the first home's heartbeat")
	}
	if _, err := db.Exec(`UPDATE seats SET workspace_fingerprint = ?, lease_until = ? WHERE seat_id = ?`,
		"foreign-holder", time.Now().UTC().Add(lease).Format(time.RFC3339Nano), nerve.WatchSeatID("skiff-secondmate")); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err == nil {
			t.Fatal("lease loss during sweep was not reported")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("lease loss did not cancel the blocked sweep")
	}
	if len(watches) != 1 {
		t.Fatalf("activated watches = %d, want only the fenced first home", len(watches))
	}
	homes[1].mu.Lock()
	secondHolding := homes[1].holding
	secondSource := homes[1].source
	homes[1].mu.Unlock()
	if secondHolding || secondSource != nil {
		t.Fatalf("lost second home remained active: holding=%v source=%v", secondHolding, secondSource)
	}
}

type blockingWatchIngester struct {
	root    string
	homeID  string
	started chan struct{}
	unblock <-chan struct{}
}

func (i *blockingWatchIngester) DataRoot() string { return i.root }

func (i *blockingWatchIngester) Backfill(ctx context.Context) (scribe.Summary, error) {
	select {
	case i.started <- struct{}{}:
	default:
	}
	select {
	case <-i.unblock:
		return scribe.Summary{HomeID: i.homeID}, nil
	case <-ctx.Done():
		return scribe.Summary{}, ctx.Err()
	}
}

func (i *blockingWatchIngester) IngestPath(context.Context, string) (scribe.Event, error) {
	return scribe.Event{HomeID: i.homeID}, nil
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

type childWatchProc struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *safeBuffer
	err   *safeBuffer
	done  chan error
}

func buildWatchCLI(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), placedExecutableName())
	cmd := exec.Command("go", "build", "-o", binary, ".")
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Dir = cwd
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build watch CLI: %v\n%s", err, output)
	}
	return binary
}

func startWatchChild(t *testing.T, binary, dbPath, home, homeID string) *childWatchProc {
	t.Helper()
	proc := &childWatchProc{out: &safeBuffer{}, err: &safeBuffer{}, done: make(chan error, 1)}
	proc.cmd = exec.Command(binary,
		"watch", "--db", dbPath, "--json", "--poll-interval", "20ms",
		"--home", home, "--home-id", homeID,
	)
	proc.cmd.Env = append(os.Environ(),
		"ROCA_FIRSTMATE_WATCH_LEASE=500ms",
		"ROCA_FIRSTMATE_WATCH_RETRY=50ms",
	)
	proc.cmd.Stdout = proc.out
	proc.cmd.Stderr = proc.err
	stdin, err := proc.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	proc.stdin = stdin
	if err := proc.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { proc.done <- proc.cmd.Wait() }()
	t.Cleanup(func() {
		_ = proc.stdin.Close()
		_ = proc.cmd.Process.Kill()
	})
	return proc
}

func waitChildWatching(t *testing.T, proc *childWatchProc) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(proc.out.String(), `"status":"watching"`) {
			return
		}
		select {
		case err := <-proc.done:
			t.Fatalf("watch child exited before activation: %v stdout=%q stderr=%q", err, proc.out.String(), proc.err.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("watch child did not activate stdout=%q stderr=%q", proc.out.String(), proc.err.String())
}

func waitChildExit(t *testing.T, proc *childWatchProc, success bool) {
	t.Helper()
	select {
	case err := <-proc.done:
		if success && err != nil {
			t.Fatalf("watch child exit: %v stdout=%q stderr=%q", err, proc.out.String(), proc.err.String())
		}
		if !success && err == nil {
			t.Fatal("abruptly killed watch child exited successfully")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch child did not exit")
	}
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
