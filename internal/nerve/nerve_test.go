package nerve_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/nerve"
	"github.com/thellmwhisperer/roca-firstmate/schema"
	_ "modernc.org/sqlite"
)

var frozen = time.Date(2026, 3, 14, 10, 0, 0, 0, time.UTC)

func TestDrainPrintsOneLineAndConfirmsTheSameGeneration(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	mustExec(t, db, `INSERT INTO wakeups (
		destination, generation, kind, home_id, payload, created_at
	) VALUES ('companion', 7, 'ready', 'northwind-harbor', 'fabricated', '2026-03-14T09:00:00Z')`)

	var output bytes.Buffer
	drained, err := nerve.Drain(context.Background(), db, []string{"companion"}, &output)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if drained != 1 || strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("drained %d output %q", drained, output.String())
	}
	var wakeup nerve.Wakeup
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &wakeup); err != nil {
		t.Fatalf("one-line JSON: %v", err)
	}
	if wakeup.Generation != 7 || wakeup.HandledGeneration != 7 {
		t.Fatalf("wakeup generations %+v", wakeup)
	}
	var handled int
	var handledGeneration int64
	if err := db.QueryRow(`SELECT handled, handled_generation FROM wakeups WHERE id = ?`, wakeup.ID).
		Scan(&handled, &handledGeneration); err != nil {
		t.Fatal(err)
	}
	if handled != 1 || handledGeneration != 7 {
		t.Fatalf("database confirmation handled=%d generation=%d", handled, handledGeneration)
	}
}

func TestDrainRollsBackWhenDeliveryFails(t *testing.T) {
	db := appliedDB(t)
	mustExec(t, db, `INSERT INTO wakeups (
		destination, generation, kind, payload, created_at
	) VALUES ('machine', 3, 'ready', 'fabricated', '2026-03-14T09:00:00Z')`)
	if _, err := nerve.Drain(context.Background(), db, []string{"machine"}, failingWriter{}); err == nil {
		t.Fatal("drain accepted a failed delivery")
	}
	var handled int
	if err := db.QueryRow(`SELECT handled FROM wakeups WHERE generation = 3`).Scan(&handled); err != nil {
		t.Fatal(err)
	}
	if handled != 0 {
		t.Fatalf("failed delivery marked handled=%d", handled)
	}
}

func TestRegisterSeatStoresOnlyOpaqueWorkspaceIdentity(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	workspace := filepath.Join(t.TempDir(), "lantern-workspace")
	seat, err := nerve.RegisterSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: "northwind-harbor", Workspace: workspace,
		Destination: "companion", Now: frozen,
	})
	if err != nil {
		t.Fatalf("register seat: %v", err)
	}
	if !strings.HasPrefix(seat.SeatID, "seat-") {
		t.Fatalf("seat id %q", seat.SeatID)
	}
	if seat.Label != seat.SeatID || strings.Contains(seat.Label, "lantern-workspace") {
		t.Fatalf("default label %q is not opaque", seat.Label)
	}
	var fingerprint string
	if err := db.QueryRow(`SELECT workspace_fingerprint FROM seats WHERE seat_id = ?`, seat.SeatID).Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(fingerprint, workspace) || len(fingerprint) != 64 {
		t.Fatalf("workspace fingerprint %q leaks or is not SHA-256", fingerprint)
	}
}

func TestRegisterSeatKeepsExplicitIdentityOutOfDefaultLabel(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	seat, err := nerve.RegisterSeat(context.Background(), db, nerve.SeatConfig{
		SeatID: "operator-chosen-seat", HomeID: "northwind-harbor",
		Workspace:   filepath.Join(t.TempDir(), "lantern-workspace"),
		Destination: "companion", Now: frozen,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seat.Label == seat.SeatID || !strings.HasPrefix(seat.Label, "seat-") {
		t.Fatalf("default label %q exposes explicit seat identity %q", seat.Label, seat.SeatID)
	}
}

func TestTickDoesNotDrainWakeupsOwnedByLiveSeat(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	if _, err := nerve.RegisterSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: "northwind-harbor", Workspace: filepath.Join(t.TempDir(), "lantern-workspace"),
		Destination: "companion", Now: frozen,
	}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO wakeups (
		destination, generation, kind, home_id, payload, created_at
	) VALUES ('companion', 1, 'ready', 'northwind-harbor', 'fabricated', '2026-03-14T09:00:00Z')`)

	var output bytes.Buffer
	result, err := nerve.Tick(context.Background(), db, frozen, 5*time.Minute, &output)
	if err != nil {
		t.Fatal(err)
	}
	if result.OrphanWakeups != 0 || output.Len() != 0 {
		t.Fatalf("live destination drained: %+v output=%q", result, output.String())
	}
	var handled int
	if err := db.QueryRow(`SELECT handled FROM wakeups WHERE generation = 1`).Scan(&handled); err != nil {
		t.Fatal(err)
	}
	if handled != 0 {
		t.Fatalf("live destination wakeup handled=%d", handled)
	}
}

func TestTickRunsPersistentSilenceClockAndDrainsOrphans(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	seat, err := nerve.RegisterSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: "northwind-harbor", Workspace: filepath.Join(t.TempDir(), "lantern-workspace"),
		Destination: "companion", Now: frozen.Add(-10 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, `INSERT INTO wakeups (
		destination, generation, kind, home_id, payload, created_at
	) VALUES ('companion', 1, 'ready', 'northwind-harbor', 'fabricated', '2026-03-14T09:00:00Z')`)

	var output bytes.Buffer
	result, err := nerve.Tick(context.Background(), db, frozen, 5*time.Minute, &output)
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if result.SilenceWakeups != 1 || result.OrphanWakeups != 2 {
		t.Fatalf("tick result %+v output %s", result, output.String())
	}
	if strings.Count(output.String(), "\n") != 2 {
		t.Fatalf("orphan delivery lines %q", output.String())
	}
	var generation int64
	if err := db.QueryRow(`SELECT silence_generation FROM seats WHERE seat_id = ?`, seat.SeatID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("silence generation %d, want 2", generation)
	}
	var mismatched int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wakeups
		WHERE handled != 1 OR handled_generation != generation`).Scan(&mismatched); err != nil {
		t.Fatal(err)
	}
	if mismatched != 0 {
		t.Fatalf("mismatched handled generations %d", mismatched)
	}

	second, err := nerve.Tick(context.Background(), db, frozen, 5*time.Minute, &output)
	if err != nil {
		t.Fatal(err)
	}
	if second.SilenceWakeups != 0 || second.OrphanWakeups != 0 {
		t.Fatalf("same-generation tick was not idempotent: %+v", second)
	}
}

func TestTickClaimsSilenceGenerationOnceAcrossConcurrentRuns(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	if _, err := nerve.RegisterSeat(context.Background(), db, nerve.SeatConfig{
		HomeID: "northwind-harbor", Workspace: filepath.Join(t.TempDir(), "lantern-workspace"),
		Destination: "companion", Now: frozen.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result nerve.TickResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			var output bytes.Buffer
			result, err := nerve.Tick(context.Background(), db, frozen, 5*time.Minute, &output)
			outcomes <- outcome{result: result, err: err}
		}()
	}
	close(start)

	created := 0
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("concurrent tick: %v", outcome.err)
		}
		created += outcome.result.SilenceWakeups
	}
	if created != 1 {
		t.Fatalf("concurrent ticks created %d silence wakeups", created)
	}
	var rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM wakeups WHERE kind = 'seat-silent'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("seat-silent rows = %d, want 1", rows)
	}
}

func TestLastHandoffUsesLatestCurrentVersion(t *testing.T) {
	db := appliedDB(t)
	seedHome(t, db)
	mustExec(t, db, `INSERT INTO operational_doc_versions (
		home_id, relative_path, document_kind, version, is_current,
		content, content_sha256, observed_at
	) VALUES
	('northwind-harbor', '2026-03-13-handover-lantern.md', 'handover', 1, 1,
	 'older fabricated handoff', 'aaa', '2026-03-13T09:00:00Z'),
	('northwind-harbor', '2026-03-14-handover-lantern.md', 'handover', 1, 1,
	 'latest fabricated handoff', 'bbb', '2026-03-14T09:00:00Z')`)
	handoff, err := nerve.LastHandoff(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if handoff == nil || handoff.Content != "latest fabricated handoff" {
		t.Fatalf("handoff %+v", handoff)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("fabricated output failure") }

func appliedDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "firstmate.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := schema.Apply(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedHome(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `INSERT INTO homes (home_id, label, kind, recorded_at)
		VALUES ('northwind-harbor', 'Northwind Harbor', 'primary', '2026-03-14T09:00:00Z')`)
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, query)
	}
}
