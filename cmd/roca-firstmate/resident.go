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
	"sync"
	"sync/atomic"
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
	mu          sync.Mutex
	pair        homePair
	token       string
	seatID      string
	ingester    watchIngester
	source      filewatch.Source
	holding     bool
	generation  uint64
	retryAt     time.Time
	leaseCtx    context.Context
	leaseCancel context.CancelFunc
}

type watchIngester interface {
	DataRoot() string
	Backfill(context.Context) (scribe.Summary, error)
	IngestPath(context.Context, string) (scribe.Event, error)
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
			standDownWatchHome(context.Background(), db, home, 0, log, time.Time{})
		}
	}()

	lease := watchLeaseDuration()
	retry := watchRetryDuration()
	msgs := make(chan watchMsg, 16)
	var watches []homeWatch
	var summaries []scribe.Summary
	var attempts atomic.Int64
	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		heartbeatWatchHomes(heartbeatCtx, db, homes, lease, retry, log, &attempts)
	}()
	defer func() {
		stopHeartbeat()
		<-heartbeatDone
	}()
	if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, retry, log, msgs, &watches, &summaries); err != nil {
		logWatchRetry(log, &attempts, err)
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
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case <-ticker.C:
			watches = nil
			summaries = nil
			if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, retry, log, msgs, &watches, &summaries); err != nil {
				logWatchRetry(log, &attempts, err)
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
			leaseCtx, held := watchHomeLease(home, msg.generation)
			if !held {
				continue
			}
			if msg.err != nil || msg.eof {
				errText := "watch event stream closed"
				if msg.err != nil {
					errText = msg.err.Error()
				}
				logWatchRetry(log, &attempts, errors.New(errText))
				standDownWatchHome(ctx, db, home, msg.generation, log, time.Now().UTC().Add(retry))
				continue
			}
			if err := ingestWatchPath(leaseCtx, home, msg.path, values.asJSON, stdout, log); err != nil {
				logWatchRetry(log, &attempts, err)
				standDownWatchHome(ctx, db, home, msg.generation, log, time.Now().UTC().Add(retry))
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
		home.mu.Lock()
		if home.holding || now.Before(home.retryAt) {
			home.mu.Unlock()
			continue
		}
		held, seat, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
			SeatID: home.seatID, HomeID: home.pair.ID, HolderToken: home.token,
			Label: "watch", Destination: "machine", Now: now, Lease: lease,
		})
		if err != nil {
			home.retryAt = now.Add(retry)
			home.mu.Unlock()
			failures = append(failures, err)
			continue
		}
		if !held {
			home.mu.Unlock()
			continue
		}
		home.holding = true
		home.generation++
		generation := home.generation
		leaseCtx, leaseCancel := context.WithCancel(ctx)
		home.leaseCtx = leaseCtx
		home.leaseCancel = leaseCancel
		home.mu.Unlock()
		_ = log.Append(watchlog.Event{Kind: watchlog.KindLeaseAcquired, HomeID: home.pair.ID, SeatID: seat.SeatID})
		source, err := filewatch.New(home.ingester.DataRoot(), poll)
		if err != nil {
			standDownWatchHome(ctx, db, home, generation, log, now.Add(retry))
			failures = append(failures, fmt.Errorf("watch: %w", err))
			continue
		}
		home.mu.Lock()
		active := home.holding && home.generation == generation
		if active {
			home.source = source
		}
		home.mu.Unlock()
		if !active {
			_ = source.Close()
			continue
		}
		summary, err := home.ingester.Backfill(leaseCtx)
		if err != nil {
			standDownWatchHome(ctx, db, home, generation, log, time.Now().UTC().Add(retry))
			failures = append(failures, err)
			continue
		}
		home.mu.Lock()
		if !home.holding || home.generation != generation {
			home.mu.Unlock()
			continue
		}
		fenced, err := nerve.RenewSeat(leaseCtx, db, home.seatID, home.token, time.Now().UTC(), lease)
		if err != nil || !fenced {
			closedSource, lost := standDownWatchHomeLocked(ctx, db, home, generation, time.Now().UTC().Add(retry))
			home.mu.Unlock()
			finishWatchStandDown(home, closedSource, lost, log)
		} else {
			home.retryAt = time.Time{}
			go pumpWatch(leaseCtx, i, generation, source, msgs)
			home.mu.Unlock()
		}
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !fenced {
			continue
		}
		_ = log.Append(watchlog.Event{
			Kind: watchlog.KindSweep, HomeID: home.pair.ID, SeatID: home.seatID,
			Scanned: summary.Scanned, Inserted: summary.Inserted,
			Unchanged: summary.Unchanged, Wakeups: summary.Wakeups,
		})
		*watches = append(*watches, homeWatch{ingester: home.ingester, source: source})
		*summaries = append(*summaries, summary)
	}
	return errors.Join(failures...)
}

func renewWatchHomes(ctx context.Context, db *sql.DB, homes []*leasedHome, now time.Time, lease, retry time.Duration, log *watchlog.Logger) error {
	var failures []error
	for _, home := range homes {
		var closedSource filewatch.Source
		var lost bool
		home.mu.Lock()
		if !home.holding {
			home.mu.Unlock()
			continue
		}
		generation := home.generation
		held, err := nerve.RenewSeat(ctx, db, home.seatID, home.token, now, lease)
		if err != nil || !held {
			closedSource, lost = standDownWatchHomeLocked(ctx, db, home, generation, now.Add(retry))
		}
		home.mu.Unlock()
		finishWatchStandDown(home, closedSource, lost, log)
		if err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func heartbeatWatchHomes(
	ctx context.Context, db *sql.DB, homes []*leasedHome, lease, retry time.Duration,
	log *watchlog.Logger, attempts *atomic.Int64,
) {
	ticker := time.NewTicker(watchHeartbeatDuration(lease, retry))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := renewWatchHomes(ctx, db, homes, time.Now().UTC(), lease, retry, log); err != nil {
				if ctx.Err() != nil {
					return
				}
				logWatchRetry(log, attempts, err)
			}
		}
	}
}

func standDownWatchHome(
	ctx context.Context, db *sql.DB, home *leasedHome, generation uint64,
	log *watchlog.Logger, retryAt time.Time,
) {
	home.mu.Lock()
	closedSource, lost := standDownWatchHomeLocked(ctx, db, home, generation, retryAt)
	home.mu.Unlock()
	finishWatchStandDown(home, closedSource, lost, log)
}

func standDownWatchHomeLocked(
	ctx context.Context, db *sql.DB, home *leasedHome, generation uint64, retryAt time.Time,
) (filewatch.Source, bool) {
	if generation != 0 && home.generation != generation {
		return nil, false
	}
	closedSource := home.source
	home.source = nil
	if home.leaseCancel != nil {
		home.leaseCancel()
		home.leaseCancel = nil
		home.leaseCtx = nil
	}
	lost := home.holding
	if lost {
		home.holding = false
		_ = nerve.ReleaseSeat(context.WithoutCancel(ctx), db, home.seatID, home.token, time.Now().UTC())
	}
	home.retryAt = retryAt
	return closedSource, lost
}

func finishWatchStandDown(home *leasedHome, source filewatch.Source, lost bool, log *watchlog.Logger) {
	if source != nil {
		_ = source.Close()
	}
	if lost {
		_ = log.Append(watchlog.Event{Kind: watchlog.KindLeaseLost, HomeID: home.pair.ID, SeatID: home.seatID})
	}
}

func watchHomeLease(home *leasedHome, generation uint64) (context.Context, bool) {
	home.mu.Lock()
	defer home.mu.Unlock()
	if !home.holding || home.generation != generation || home.leaseCtx == nil {
		return nil, false
	}
	return home.leaseCtx, true
}

func logWatchRetry(log *watchlog.Logger, attempts *atomic.Int64, err error) {
	attempt := attempts.Add(1)
	_ = log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: int(attempt), Err: err.Error()})
}

func watchHeartbeatDuration(lease, retry time.Duration) time.Duration {
	interval := lease / 3
	if interval <= 0 {
		interval = lease
	}
	if retry < interval {
		interval = retry
	}
	return interval
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
