/*
*
@overview Deterministic Nerve routing for firstmate.db. ~490 lines, 11 public symbols.

		READING GUIDE
		-------------
		1. Start at Follow                   <- WAL-driven subscription loop
		2. Read Drain                        <- no-loss at-least-once delivery boundary
		3. Read Tick                         <- ephemeral silence/orphan reconciliation

		MAIN FLOW
		---------
		RegisterSeat -> Follow -> Drain -> at-least-once generation-confirmed wakeup

		PUBLIC API
		----------
	  SeatConfig         Registration inputs without a persisted workspace path
	  Seat               Registered subscription identity
	  Handoff            Latest mirrored handoff row
	  Wakeup             One delivered queue row
	  TickResult         Ephemeral tick summary
		WorkspaceSeatID()  Stable path-derived identity
		RegisterSeat()     Create or refresh a seat lease
		LastHandoff()      Read the latest mirrored handoff
		Follow()           Subscribe to WAL changes and drain one destination (optional home filter)
		Drain()            Deliver and generation-confirm pending rows
		Tick()             Run silence clock and orphan recovery once

		INTERNALS
		---------
		heartbeat, silenceClock, drainOrphans, drainDestination, claimNext, nextGeneration

@exports SeatConfig, Seat, Handoff, Wakeup, TickResult, WorkspaceSeatID, RegisterSeat, LastHandoff, Follow, Drain, Tick
@deps database/sql queue state; encoding/json one-line envelopes; filesystem watcher in wal_*.go
*/
package nerve

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultLeaseDuration = 2 * time.Minute
	heartbeatInterval    = 30 * time.Second
)

// -- 1/4 HELPER · Public values and seat identity --

// SeatConfig describes one workspace subscription without persisting its path.
type SeatConfig struct {
	SeatID      string
	HomeID      string
	Workspace   string
	Label       string
	Destination string
	Now         time.Time
}

// Seat is the bounded registration result.
type Seat struct {
	Status      string `json:"status"`
	SeatID      string `json:"seat_id"`
	HomeID      string `json:"home_id"`
	Label       string `json:"label"`
	Destination string `json:"destination"`
	AttachedAt  string `json:"attached_at"`
	LeaseUntil  string `json:"lease_until"`
}

// Handoff is the latest current handoff mirrored by Scribe.
type Handoff struct {
	HomeID       string `json:"home_id"`
	RelativePath string `json:"relative_path"`
	Version      int    `json:"version"`
	ObservedAt   string `json:"observed_at"`
	Content      string `json:"content"`
}

// Wakeup is one generation-confirmed line delivered by follow or tick.
type Wakeup struct {
	ID                int64  `json:"id"`
	Destination       string `json:"destination"`
	Generation        int64  `json:"generation"`
	HandledGeneration int64  `json:"handled_generation"`
	Kind              string `json:"kind"`
	HomeID            string `json:"home_id,omitempty"`
	TaskID            string `json:"task_id,omitempty"`
	Payload           string `json:"payload"`
	CreatedAt         string `json:"created_at"`
}

// TickResult is the complete persisted-state result of one ephemeral tick.
type TickResult struct {
	Status         string   `json:"status"`
	SilenceWakeups int      `json:"silence_wakeups"`
	OrphanWakeups  int      `json:"orphan_wakeups"`
	Destinations   []string `json:"destinations"`
	Help           []string `json:"help"`
}

// WorkspaceSeatID derives a stable opaque seat identity. The resolved path is
// hashed and never stored in firstmate.db.
func WorkspaceSeatID(homeID, workspace string) (string, string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", "", fmt.Errorf("resolve workspace: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(homeID) + "\x00" + abs))
	fingerprint := hex.EncodeToString(sum[:])
	return "seat-" + fingerprint[:16], fingerprint, nil
}

// RegisterSeat creates or refreshes one workspace subscription lease.
func RegisterSeat(ctx context.Context, db *sql.DB, config SeatConfig) (Seat, error) {
	now := config.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	seatID, fingerprint, err := WorkspaceSeatID(config.HomeID, config.Workspace)
	if err != nil {
		return Seat{}, err
	}
	opaqueLabel := seatID
	if strings.TrimSpace(config.SeatID) != "" {
		seatID = strings.TrimSpace(config.SeatID)
	}
	if strings.TrimSpace(config.Label) == "" {
		config.Label = opaqueLabel
	}
	if !validDestination(config.Destination) {
		return Seat{}, fmt.Errorf("destination %q must be captain, companion, or machine", config.Destination)
	}
	var existed int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM seats WHERE seat_id = ?`, seatID).Scan(&existed); err != nil {
		return Seat{}, fmt.Errorf("read seat: %w", err)
	}
	stamp := now.Format(time.RFC3339Nano)
	lease := now.Add(defaultLeaseDuration).Format(time.RFC3339Nano)
	_, err = db.ExecContext(ctx, `INSERT INTO seats (
		seat_id, home_id, workspace_fingerprint, label, destination,
		attached_at, last_seen_at, lease_until, silence_generation
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	ON CONFLICT(seat_id) DO UPDATE SET
		home_id = excluded.home_id,
		workspace_fingerprint = excluded.workspace_fingerprint,
		label = excluded.label,
		destination = excluded.destination,
		last_seen_at = excluded.last_seen_at,
		lease_until = excluded.lease_until,
		silence_generation = 0`,
		seatID, config.HomeID, fingerprint, config.Label, config.Destination,
		stamp, stamp, lease)
	if err != nil {
		return Seat{}, fmt.Errorf("register seat: %w", err)
	}
	var attachedAt, leaseUntil string
	if err := db.QueryRowContext(ctx, `SELECT attached_at, lease_until FROM seats WHERE seat_id = ?`, seatID).
		Scan(&attachedAt, &leaseUntil); err != nil {
		return Seat{}, fmt.Errorf("read registered seat: %w", err)
	}
	status := "registered"
	if existed != 0 {
		status = "refreshed"
	}
	return Seat{
		Status: status, SeatID: seatID, HomeID: config.HomeID, Label: config.Label,
		Destination: config.Destination, AttachedAt: attachedAt, LeaseUntil: leaseUntil,
	}, nil
}

// LastHandoff returns the latest current handoff across root and task docs.
func LastHandoff(ctx context.Context, db *sql.DB) (*Handoff, error) {
	row := db.QueryRowContext(ctx, `SELECT home_id, relative_path, version, observed_at, content
	FROM (
		SELECT home_id, relative_path, version, observed_at, content, id
		FROM operational_doc_versions WHERE is_current = 1 AND document_kind = 'handover'
		UNION ALL
		SELECT home_id, relative_path, version, observed_at, content, id
		FROM task_artifact_versions WHERE is_current = 1 AND document_kind = 'handover'
	) ORDER BY observed_at DESC, id DESC LIMIT 1`)
	var handoff Handoff
	if err := row.Scan(&handoff.HomeID, &handoff.RelativePath, &handoff.Version, &handoff.ObservedAt, &handoff.Content); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read last handoff: %w", err)
	}
	return &handoff, nil
}

// -/ 1/4

// -- 2/4 CORE · Follow and at-least-once one-line delivery -- <- START HERE

// Follow drains the current destination, then listens for database/WAL changes.
// Its registered seat owns and heartbeats the destination subscription.
func Follow(ctx context.Context, db *sql.DB, dbPath, destination string, seatIDs, homeIDs []string, w io.Writer) error {
	if !validDestination(destination) {
		return fmt.Errorf("destination %q must be captain, companion, or machine", destination)
	}
	seats := make([]string, 0, len(seatIDs))
	for _, seatID := range seatIDs {
		seatID = strings.TrimSpace(seatID)
		if seatID == "" {
			continue
		}
		seats = append(seats, seatID)
	}
	if len(seats) == 0 {
		return errors.New("follow requires a registered seat")
	}
	source, err := newWALSource(dbPath)
	if err != nil {
		return fmt.Errorf("watch database WAL: %w", err)
	}
	defer source.Close()
	now := time.Now().UTC()
	for _, seatID := range seats {
		if err := heartbeat(ctx, db, seatID, now); err != nil {
			return err
		}
	}
	if _, err := DrainMatching(ctx, db, []string{destination}, homeIDs, w); err != nil {
		return err
	}
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-source.Errors():
			if !ok {
				return errors.New("database WAL event stream closed")
			}
			return err
		case _, ok := <-source.Signals():
			if !ok {
				return errors.New("database WAL event stream closed")
			}
			if _, err := DrainMatching(ctx, db, []string{destination}, homeIDs, w); err != nil {
				return err
			}
		case now := <-ticker.C:
			now = now.UTC()
			for _, seatID := range seats {
				if err := heartbeat(ctx, db, seatID, now); err != nil {
					return err
				}
			}
		}
	}
}

// Drain writes one JSON line for each delivery attempt, then commits its
// handled generation. Output failure rolls the claim back. Process death or a
// commit failure after a successful write can repeat the line, so adapters
// deduplicate at-least-once delivery by destination and generation.
func Drain(ctx context.Context, db *sql.DB, destinations []string, w io.Writer) (int, error) {
	return DrainMatching(ctx, db, destinations, nil, w)
}

// DrainMatching is Drain with an optional home_id filter. An empty homeIDs
// list delivers every pending row for the destination.
func DrainMatching(ctx context.Context, db *sql.DB, destinations, homeIDs []string, w io.Writer) (int, error) {
	total := 0
	for _, destination := range destinations {
		if !validDestination(destination) {
			return total, fmt.Errorf("destination %q must be captain, companion, or machine", destination)
		}
		drained, err := drainDestination(ctx, db, destination, "", homeIDs, w)
		if err != nil {
			return total, err
		}
		total += drained
	}
	return total, nil
}

func drainOrphans(ctx context.Context, db *sql.DB, now time.Time, w io.Writer) (int, []string, error) {
	total := 0
	var destinations []string
	cutoff := now.UTC().Format(time.RFC3339Nano)
	for _, destination := range []string{"captain", "companion", "machine"} {
		drained, err := drainDestination(ctx, db, destination, cutoff, nil, w)
		if err != nil {
			return total, destinations, err
		}
		if drained != 0 {
			destinations = append(destinations, destination)
			total += drained
		}
	}
	return total, destinations, nil
}

func drainDestination(ctx context.Context, db *sql.DB, destination, orphanCutoff string, homeIDs []string, w io.Writer) (int, error) {
	total := 0
	for {
		wakeup, tx, err := claimNext(ctx, db, destination, orphanCutoff, homeIDs)
		if err != nil {
			return total, err
		}
		if wakeup == nil {
			return total, nil
		}
		raw, err := json.Marshal(wakeup)
		if err != nil {
			_ = tx.Rollback()
			return total, fmt.Errorf("render wakeup: %w", err)
		}
		if _, err := fmt.Fprintln(w, string(raw)); err != nil {
			_ = tx.Rollback()
			return total, fmt.Errorf("deliver wakeup generation %d: %w", wakeup.Generation, err)
		}
		if err := tx.Commit(); err != nil {
			return total, fmt.Errorf("confirm wakeup generation %d: %w", wakeup.Generation, err)
		}
		total++
	}
}

func claimNext(ctx context.Context, db *sql.DB, destination, orphanCutoff string, homeIDs []string) (*Wakeup, *sql.Tx, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin wakeup claim: %w", err)
	}
	filterSQL, filterArgs := homeIDFilter(homeIDs)
	query := `UPDATE wakeups
	SET handled = 1, handled_generation = generation
	WHERE id = (
		SELECT w.id FROM wakeups w
		WHERE w.handled = 0 AND w.destination = ?
		AND (? = '' OR NOT EXISTS (
			SELECT 1 FROM seats s
			WHERE s.destination = w.destination AND s.lease_until > ?
		))
		` + filterSQL + `
		ORDER BY generation, id LIMIT 1
	) AND handled = 0
	RETURNING id, destination, generation, handled_generation, kind,
		COALESCE(home_id, ''), COALESCE(task_id, ''), payload, created_at`
	args := append([]any{destination, orphanCutoff, orphanCutoff}, filterArgs...)
	row := tx.QueryRowContext(ctx, query, args...)
	var wakeup Wakeup
	if err := row.Scan(
		&wakeup.ID, &wakeup.Destination, &wakeup.Generation, &wakeup.HandledGeneration,
		&wakeup.Kind, &wakeup.HomeID, &wakeup.TaskID, &wakeup.Payload, &wakeup.CreatedAt,
	); err != nil {
		_ = tx.Rollback()
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("claim %s wakeup: %w", destination, err)
	}
	return &wakeup, tx, nil
}

// -/ 2/4

// -- 3/4 HELPER · Persistent silence clock and orphan discovery --

// Tick runs once and dies: silence generations, orphan drain, deterministic summary.
func Tick(ctx context.Context, db *sql.DB, now time.Time, silenceAfter time.Duration, w io.Writer) (TickResult, error) {
	if silenceAfter <= 0 {
		return TickResult{}, errors.New("silence-after must be positive")
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	silence, err := silenceClock(ctx, db, now, silenceAfter)
	if err != nil {
		return TickResult{}, err
	}
	drained, destinations, err := drainOrphans(ctx, db, now, w)
	if err != nil {
		return TickResult{}, err
	}
	return TickResult{
		Status: "ticked", SilenceWakeups: silence, OrphanWakeups: drained,
		Destinations: destinations,
		Help: []string{
			"Schedule `roca-firstmate tick` as an ephemeral cron command; it keeps no in-memory state",
			"Run `roca-firstmate attach` in an agent seat to subscribe instead of polling",
		},
	}, nil
}

func silenceClock(ctx context.Context, db *sql.DB, now time.Time, silenceAfter time.Duration) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT seat_id, home_id, destination, last_seen_at, silence_generation
		FROM seats WHERE lease_until <= ? ORDER BY seat_id`, now.Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("read silent seats: %w", err)
	}
	type silentSeat struct {
		seatID, homeID, destination, lastSeen string
		generation                            int64
	}
	var seats []silentSeat
	for rows.Next() {
		var seat silentSeat
		if err := rows.Scan(&seat.seatID, &seat.homeID, &seat.destination, &seat.lastSeen, &seat.generation); err != nil {
			rows.Close()
			return 0, err
		}
		seats = append(seats, seat)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	created := 0
	for _, seat := range seats {
		lastSeen, err := time.Parse(time.RFC3339Nano, seat.lastSeen)
		if err != nil {
			return created, fmt.Errorf("parse seat %s last_seen_at: %w", seat.seatID, err)
		}
		generation := int64(now.Sub(lastSeen) / silenceAfter)
		if generation < 1 || generation <= seat.generation {
			continue
		}
		payload, err := json.Marshal(map[string]any{
			"seat_id": seat.seatID, "destination": seat.destination,
			"silence_generation": generation,
		})
		if err != nil {
			return created, err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return created, err
		}
		claim, err := tx.ExecContext(ctx, `UPDATE seats SET silence_generation = ?
			WHERE seat_id = ? AND last_seen_at = ? AND lease_until <= ?
			AND silence_generation < ?`, generation, seat.seatID, seat.lastSeen,
			now.Format(time.RFC3339Nano), generation)
		if err != nil {
			_ = tx.Rollback()
			return created, fmt.Errorf("claim seat silence: %w", err)
		}
		claimed, err := claim.RowsAffected()
		if err != nil {
			_ = tx.Rollback()
			return created, err
		}
		if claimed == 0 {
			_ = tx.Rollback()
			continue
		}
		wakeGeneration, err := nextGeneration(ctx, tx)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO wakeups (
				destination, generation, kind, home_id, payload, created_at
			) VALUES ('captain', ?, 'seat-silent', ?, ?, ?)`,
				wakeGeneration, seat.homeID, string(payload), now.Format(time.RFC3339Nano))
		}
		if err != nil {
			_ = tx.Rollback()
			return created, fmt.Errorf("record seat silence: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

func nextGeneration(ctx context.Context, tx *sql.Tx) (int64, error) {
	var generation int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM wakeups`).Scan(&generation); err != nil {
		return 0, err
	}
	return generation, nil
}

// -/ 3/4

// -- 4/4 HELPER · Lease heartbeat and validation --

func heartbeat(ctx context.Context, db *sql.DB, seatID string, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	lease := now.UTC().Add(defaultLeaseDuration).Format(time.RFC3339Nano)
	result, err := db.ExecContext(ctx, `UPDATE seats SET
		last_seen_at = ?, lease_until = ?, silence_generation = 0
		WHERE seat_id = ?`, stamp, lease, seatID)
	if err != nil {
		return fmt.Errorf("refresh seat lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("seat %q is not registered; run attach first", seatID)
	}
	return nil
}

func homeIDFilter(homeIDs []string) (string, []any) {
	filtered := make([]string, 0, len(homeIDs))
	for _, id := range homeIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		filtered = append(filtered, id)
	}
	if len(filtered) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(filtered))
	args := make([]any, len(filtered))
	for i, id := range filtered {
		placeholders[i] = "?"
		args[i] = id
	}
	return " AND w.home_id IN (" + strings.Join(placeholders, ",") + ")", args
}

func validDestination(destination string) bool {
	switch destination {
	case "captain", "companion", "machine":
		return true
	default:
		return false
	}
}

// -/ 4/4
