package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thellmwhisperer/roca-firstmate/internal/nerve"
	"github.com/thellmwhisperer/roca-firstmate/internal/scribe"
	filewatch "github.com/thellmwhisperer/roca-firstmate/internal/watch"
	"github.com/thellmwhisperer/roca-firstmate/internal/watchlog"
)

const (
	defaultWatchLease = 2 * time.Minute
	defaultWatchRetry = 30 * time.Second
)

type leasedHome struct {
	pair     homePair
	token    string
	seatID   string
	ingester *scribe.Ingester
	source   filewatch.Source
	holding  bool
}

func runWatch(ctx context.Context, db *sql.DB, pairs []homePair, values scribeFlags, stdin io.Reader, stdout io.Writer) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeOnEOF(ctx, cancel, stdin)

	log := watchlog.Open(watchlog.Dir(values.dbPath), time.Now)
	homes := make([]*leasedHome, 0, len(pairs))
	for _, pair := range pairs {
		token, err := nerve.NewHolderToken()
		if err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		ingester, err := newIngester(ctx, db, pair, values.sourceAgent)
		if err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		home := &leasedHome{
			pair: pair, token: token, seatID: nerve.WatchSeatID(pair.ID), ingester: ingester,
		}
		_ = log.Append(watchlog.Event{Kind: watchlog.KindRaise, HomeID: pair.ID, SeatID: home.seatID})
		homes = append(homes, home)
	}
	defer func() {
		now := time.Now().UTC()
		for _, home := range homes {
			if home.source != nil {
				_ = home.source.Close()
			}
			if home.holding {
				_ = nerve.ReleaseSeat(context.Background(), db, home.seatID, home.token, now)
				_ = log.Append(watchlog.Event{Kind: watchlog.KindLeaseLost, HomeID: home.pair.ID, SeatID: home.seatID})
			}
		}
	}()

	lease := watchLeaseDuration()
	retry := watchRetryDuration()
	msgs := make(chan watchMsg, 16)
	var watches []homeWatch
	var summaries []scribe.Summary
	if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, log, msgs, &watches, &summaries); err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	if len(watches) > 0 {
		if err := renderWatchStarts(stdout, values.asJSON, watchBackend(watches), summaries); err != nil {
			printScribeError(stdout, err)
			return exitError
		}
	}

	ticker := time.NewTicker(retry)
	defer ticker.Stop()
	heartbeat := time.NewTicker(retry)
	defer heartbeat.Stop()
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case now := <-heartbeat.C:
			if err := renewWatchHomes(ctx, db, homes, now.UTC(), lease, log); err != nil {
				attempt++
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: err.Error()})
			}
		case <-ticker.C:
			before := len(watches)
			if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, log, msgs, &watches, &summaries); err != nil {
				attempt++
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: err.Error()})
				continue
			}
			if before == 0 && len(watches) > 0 {
				if err := renderWatchStarts(stdout, values.asJSON, watchBackend(watches), summaries); err != nil {
					printScribeError(stdout, err)
					return exitError
				}
			}
		case msg := <-msgs:
			if msg.err != nil || msg.eof {
				attempt++
				errText := "watch event stream closed"
				if msg.err != nil {
					errText = msg.err.Error()
				}
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: errText})
				dropWatchHome(homes[msg.idx], log)
				continue
			}
			if err := ingestWatchPath(ctx, homes[msg.idx], msg.path, values.asJSON, stdout, log); err != nil {
				attempt++
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: err.Error()})
			}
		}
	}
}

func acquireWatchHomes(
	ctx context.Context, db *sql.DB, homes []*leasedHome, poll, lease time.Duration,
	log *watchlog.Logger, msgs chan watchMsg, watches *[]homeWatch, summaries *[]scribe.Summary,
) error {
	now := time.Now().UTC()
	for i, home := range homes {
		if home.holding {
			continue
		}
		held, seat, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
			SeatID: home.seatID, HomeID: home.pair.ID, HolderToken: home.token,
			Label: "watch", Destination: "machine", Now: now, Lease: lease,
		})
		if err != nil {
			return err
		}
		if !held {
			continue
		}
		_ = log.Append(watchlog.Event{Kind: watchlog.KindLeaseAcquired, HomeID: home.pair.ID, SeatID: seat.SeatID})
		source, err := filewatch.New(home.ingester.DataRoot(), poll)
		if err != nil {
			_ = nerve.ReleaseSeat(ctx, db, home.seatID, home.token, now)
			return fmt.Errorf("watch: %w", err)
		}
		summary, err := home.ingester.Backfill(ctx)
		if err != nil {
			_ = source.Close()
			_ = nerve.ReleaseSeat(ctx, db, home.seatID, home.token, now)
			return err
		}
		_ = log.Append(watchlog.Event{
			Kind: watchlog.KindSweep, HomeID: home.pair.ID, SeatID: home.seatID,
			Scanned: summary.Scanned, Inserted: summary.Inserted,
			Unchanged: summary.Unchanged, Wakeups: summary.Wakeups,
		})
		home.source = source
		home.holding = true
		*watches = append(*watches, homeWatch{ingester: home.ingester, source: source})
		*summaries = append(*summaries, summary)
		go pumpWatch(ctx, i, source, msgs)
	}
	return nil
}

func renewWatchHomes(ctx context.Context, db *sql.DB, homes []*leasedHome, now time.Time, lease time.Duration, log *watchlog.Logger) error {
	for _, home := range homes {
		if !home.holding {
			continue
		}
		held, err := nerve.RenewSeat(ctx, db, home.seatID, home.token, now, lease)
		if err != nil {
			return err
		}
		if !held {
			dropWatchHome(home, log)
		}
	}
	return nil
}

func dropWatchHome(home *leasedHome, log *watchlog.Logger) {
	if home.source != nil {
		_ = home.source.Close()
		home.source = nil
	}
	if home.holding {
		home.holding = false
		_ = log.Append(watchlog.Event{Kind: watchlog.KindLeaseLost, HomeID: home.pair.ID, SeatID: home.seatID})
	}
}

func ingestWatchPath(ctx context.Context, home *leasedHome, path string, asJSON bool, stdout io.Writer, log *watchlog.Logger) error {
	if path == "" {
		return nil
	}
	if filepath.Clean(path) == filepath.Clean(home.ingester.DataRoot()) {
		recovery, err := home.ingester.Backfill(ctx)
		if err != nil {
			return err
		}
		_ = log.Append(watchlog.Event{
			Kind: watchlog.KindSweep, HomeID: home.pair.ID, SeatID: home.seatID,
			Scanned: recovery.Scanned, Inserted: recovery.Inserted,
			Unchanged: recovery.Unchanged, Wakeups: recovery.Wakeups,
		})
		return renderScribe(stdout, asJSON, recovery)
	}
	event, err := home.ingester.IngestPath(ctx, path)
	if err != nil {
		return err
	}
	if !event.Inserted {
		return nil
	}
	_ = log.Append(watchlog.Event{
		Kind: watchlog.KindSweep, HomeID: home.pair.ID, SeatID: home.seatID,
		Inserted: 1, Wakeups: event.Wakeups,
	})
	return renderScribe(stdout, asJSON, event)
}

func pumpWatch(ctx context.Context, idx int, source filewatch.Source, out chan watchMsg) {
	events := source.Events()
	errs := source.Errors()
	for events != nil || errs != nil {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			select {
			case out <- watchMsg{idx: idx, err: err}:
			case <-ctx.Done():
			}
			return
		case path, ok := <-events:
			if !ok {
				events = nil
				select {
				case out <- watchMsg{idx: idx, eof: true}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- watchMsg{idx: idx, path: path}:
			case <-ctx.Done():
			}
		}
	}
}

func closeOnEOF(ctx context.Context, cancel context.CancelFunc, stdin io.Reader) {
	if stdin == nil {
		return
	}
	_, _ = io.Copy(io.Discard, stdin)
	if ctx.Err() == nil {
		cancel()
	}
}

func watchLeaseDuration() time.Duration {
	return envDuration("ROCA_FIRSTMATE_WATCH_LEASE", defaultWatchLease)
}

func watchRetryDuration() time.Duration {
	return envDuration("ROCA_FIRSTMATE_WATCH_RETRY", defaultWatchRetry)
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
