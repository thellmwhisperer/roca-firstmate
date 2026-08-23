package main

import (
	"context"
	"database/sql"
	"errors"
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
	pair       homePair
	token      string
	seatID     string
	ingester   *scribe.Ingester
	source     filewatch.Source
	holding    bool
	generation uint64
	retryAt    time.Time
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
		for _, home := range homes {
			standDownWatchHome(context.Background(), db, home, log, time.Time{})
		}
	}()

	lease := watchLeaseDuration()
	retry := watchRetryDuration()
	msgs := make(chan watchMsg, 16)
	var watches []homeWatch
	var summaries []scribe.Summary
	attempt := 0
	if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, retry, log, msgs, &watches, &summaries); err != nil {
		attempt++
		_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: err.Error()})
	}
	announced := len(watches) > 0
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
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case <-heartbeat.C:
			if err := renewWatchHomes(ctx, db, homes, time.Now().UTC(), lease, retry, log); err != nil {
				attempt++
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: err.Error()})
			}
		case <-ticker.C:
			watches = nil
			summaries = nil
			if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, retry, log, msgs, &watches, &summaries); err != nil {
				attempt++
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: err.Error()})
			}
			if !announced && len(watches) > 0 {
				if err := renderWatchStarts(stdout, values.asJSON, watchBackend(watches), summaries); err != nil {
					printScribeError(stdout, err)
					return exitError
				}
				announced = true
			}
		case msg := <-msgs:
			home := homes[msg.idx]
			if !home.holding || msg.generation != home.generation {
				continue
			}
			if msg.err != nil || msg.eof {
				attempt++
				errText := "watch event stream closed"
				if msg.err != nil {
					errText = msg.err.Error()
				}
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: errText})
				standDownWatchHome(ctx, db, home, log, time.Now().UTC().Add(retry))
				continue
			}
			if err := ingestWatchPath(ctx, home, msg.path, values.asJSON, stdout, log); err != nil {
				attempt++
				_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: attempt, Err: err.Error()})
				standDownWatchHome(ctx, db, home, log, time.Now().UTC().Add(retry))
			}
		}
	}
}

func acquireWatchHomes(
	ctx context.Context, db *sql.DB, homes []*leasedHome, poll, lease, retry time.Duration,
	log *watchlog.Logger, msgs chan watchMsg, watches *[]homeWatch, summaries *[]scribe.Summary,
) error {
	var failures []error
	for i, home := range homes {
		now := time.Now().UTC()
		if home.holding || now.Before(home.retryAt) {
			continue
		}
		held, seat, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
			SeatID: home.seatID, HomeID: home.pair.ID, HolderToken: home.token,
			Label: "watch", Destination: "machine", Now: now, Lease: lease,
		})
		if err != nil {
			home.retryAt = now.Add(retry)
			failures = append(failures, err)
			continue
		}
		if !held {
			continue
		}
		home.holding = true
		_ = log.Append(watchlog.Event{Kind: watchlog.KindLeaseAcquired, HomeID: home.pair.ID, SeatID: seat.SeatID})
		source, err := filewatch.New(home.ingester.DataRoot(), poll)
		if err != nil {
			standDownWatchHome(ctx, db, home, log, now.Add(retry))
			failures = append(failures, fmt.Errorf("watch: %w", err))
			continue
		}
		home.source = source
		summary, err := home.ingester.Backfill(ctx)
		if err != nil {
			standDownWatchHome(ctx, db, home, log, time.Now().UTC().Add(retry))
			failures = append(failures, err)
			continue
		}
		fenced, err := nerve.RenewSeat(ctx, db, home.seatID, home.token, time.Now().UTC(), lease)
		if err != nil {
			standDownWatchHome(ctx, db, home, log, time.Now().UTC().Add(retry))
			failures = append(failures, err)
			continue
		}
		if !fenced {
			standDownWatchHome(ctx, db, home, log, time.Now().UTC().Add(retry))
			continue
		}
		_ = log.Append(watchlog.Event{
			Kind: watchlog.KindSweep, HomeID: home.pair.ID, SeatID: home.seatID,
			Scanned: summary.Scanned, Inserted: summary.Inserted,
			Unchanged: summary.Unchanged, Wakeups: summary.Wakeups,
		})
		home.retryAt = time.Time{}
		home.generation++
		*watches = append(*watches, homeWatch{ingester: home.ingester, source: source})
		*summaries = append(*summaries, summary)
		go pumpWatch(ctx, i, home.generation, source, msgs)
	}
	return errors.Join(failures...)
}

func renewWatchHomes(ctx context.Context, db *sql.DB, homes []*leasedHome, now time.Time, lease, retry time.Duration, log *watchlog.Logger) error {
	var failures []error
	for _, home := range homes {
		if !home.holding {
			continue
		}
		held, err := nerve.RenewSeat(ctx, db, home.seatID, home.token, now, lease)
		if err != nil {
			standDownWatchHome(ctx, db, home, log, now.Add(retry))
			failures = append(failures, err)
			continue
		}
		if !held {
			standDownWatchHome(ctx, db, home, log, now.Add(retry))
		}
	}
	return errors.Join(failures...)
}

func standDownWatchHome(ctx context.Context, db *sql.DB, home *leasedHome, log *watchlog.Logger, retryAt time.Time) {
	if home.source != nil {
		_ = home.source.Close()
		home.source = nil
	}
	if home.holding {
		_ = nerve.ReleaseSeat(ctx, db, home.seatID, home.token, time.Now().UTC())
		home.holding = false
		_ = log.Append(watchlog.Event{Kind: watchlog.KindLeaseLost, HomeID: home.pair.ID, SeatID: home.seatID})
	}
	home.retryAt = retryAt
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

func pumpWatch(ctx context.Context, idx int, generation uint64, source filewatch.Source, out chan watchMsg) {
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
			case out <- watchMsg{idx: idx, generation: generation, err: err}:
			case <-ctx.Done():
			}
			return
		case path, ok := <-events:
			if !ok {
				events = nil
				select {
				case out <- watchMsg{idx: idx, generation: generation, eof: true}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case out <- watchMsg{idx: idx, generation: generation, path: path}:
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
