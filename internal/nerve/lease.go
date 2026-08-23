package nerve

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// WatchSeatID is the single-flight seat identity for one home's filesystem
// watcher. Concurrent MCP sessions compete for this row; they do not invent a
// second lock.
func WatchSeatID(homeID string) string {
	return "watch-" + strings.TrimSpace(homeID)
}

// NewHolderToken returns an opaque 32-byte identity. It is stored as the seat
// workspace fingerprint so a holder can renew without persisting a path.
func NewHolderToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("holder token: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

// TryAcquireSeat takes the watch lease for a home only when the seat is absent
// or expired, or when this holder already owns it. A live foreign lease is left
// untouched.
func TryAcquireSeat(ctx context.Context, db *sql.DB, config SeatConfig) (bool, Seat, error) {
	homeID := strings.TrimSpace(config.HomeID)
	if homeID == "" {
		return false, Seat{}, fmt.Errorf("watch seat requires a home id")
	}
	token := strings.TrimSpace(config.HolderToken)
	if token == "" {
		return false, Seat{}, fmt.Errorf("watch seat requires a holder token")
	}
	if strings.TrimSpace(config.Destination) == "" {
		config.Destination = "machine"
	}
	if !validDestination(config.Destination) {
		return false, Seat{}, fmt.Errorf("destination %q must be captain, companion, or machine", config.Destination)
	}
	now := config.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	lease := config.Lease
	if lease <= 0 {
		lease = defaultLeaseDuration
	}
	seatID := strings.TrimSpace(config.SeatID)
	if seatID == "" {
		seatID = WatchSeatID(homeID)
	}
	label := strings.TrimSpace(config.Label)
	if label == "" {
		label = "watch"
	}
	stamp := now.Format(time.RFC3339Nano)
	until := now.Add(lease).Format(time.RFC3339Nano)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, Seat{}, fmt.Errorf("begin watch lease: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `INSERT INTO seats (
		seat_id, home_id, workspace_fingerprint, label, destination,
		attached_at, last_seen_at, lease_until, silence_generation
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	ON CONFLICT(seat_id) DO UPDATE SET
		workspace_fingerprint = excluded.workspace_fingerprint,
		label = excluded.label,
		destination = excluded.destination,
		last_seen_at = excluded.last_seen_at,
		lease_until = excluded.lease_until,
		silence_generation = 0
	WHERE seats.lease_until <= excluded.last_seen_at
	   OR seats.workspace_fingerprint = excluded.workspace_fingerprint`,
		seatID, homeID, token, label, config.Destination, stamp, stamp, until)
	if err != nil {
		return false, Seat{}, fmt.Errorf("acquire watch lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, Seat{}, err
	}
	if changed == 0 {
		if err := tx.Commit(); err != nil {
			return false, Seat{}, err
		}
		return false, Seat{}, nil
	}
	var attachedAt, leaseUntil string
	if err := tx.QueryRowContext(ctx, `SELECT attached_at, lease_until FROM seats WHERE seat_id = ?`, seatID).
		Scan(&attachedAt, &leaseUntil); err != nil {
		return false, Seat{}, fmt.Errorf("read watch lease: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, Seat{}, err
	}
	return true, Seat{
		Status: "holding", SeatID: seatID, HomeID: homeID, Label: label,
		Destination: config.Destination, AttachedAt: attachedAt, LeaseUntil: leaseUntil,
	}, nil
}

// RenewSeat extends a watch lease only when this holder still owns it.
func RenewSeat(ctx context.Context, db *sql.DB, seatID, holderToken string, now time.Time, lease time.Duration) (bool, error) {
	seatID = strings.TrimSpace(seatID)
	holderToken = strings.TrimSpace(holderToken)
	if seatID == "" || holderToken == "" {
		return false, fmt.Errorf("renew watch lease requires a seat and holder token")
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if lease <= 0 {
		lease = defaultLeaseDuration
	}
	result, err := db.ExecContext(ctx, `UPDATE seats SET
		last_seen_at = ?, lease_until = ?, silence_generation = 0
		WHERE seat_id = ? AND workspace_fingerprint = ?`,
		now.Format(time.RFC3339Nano), now.Add(lease).Format(time.RFC3339Nano),
		seatID, holderToken)
	if err != nil {
		return false, fmt.Errorf("renew watch lease: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// ReleaseSeat expires a watch lease this holder owns so the next candidate can
// inherit without waiting out the remaining window.
func ReleaseSeat(ctx context.Context, db *sql.DB, seatID, holderToken string, now time.Time) error {
	seatID = strings.TrimSpace(seatID)
	holderToken = strings.TrimSpace(holderToken)
	if seatID == "" || holderToken == "" {
		return fmt.Errorf("release watch lease requires a seat and holder token")
	}
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	stamp := now.Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `UPDATE seats SET last_seen_at = ?, lease_until = ?
		WHERE seat_id = ? AND workspace_fingerprint = ?`,
		stamp, stamp, seatID, holderToken)
	if err != nil {
		return fmt.Errorf("release watch lease: %w", err)
	}
	return nil
}
