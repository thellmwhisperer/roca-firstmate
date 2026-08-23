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
	defaultWatchLease           = 2 * time.Minute
	defaultWatchRetry           = 30 * time.Second
	defaultWatchShutdownTimeout = 250 * time.Millisecond
)

type leasedHome struct {
	mu           sync.Mutex
	pair         homePair
	token        string
	seatID       string
	sourceAgent  string
	ingester     watchIngester
	newIngester  func(context.Context, *sql.DB, *leasedHome) (watchIngester, error)
	source       filewatch.Source
	prepared     bool
	holding      bool
	generation   uint64
	leaseUntil   time.Time
	standbyUntil time.Time
	retryAt      time.Time
	leaseCtx     context.Context
	leaseCancel  context.CancelFunc
}

type watchTelemetry struct {
	logger *watchlog.Logger
	stderr io.Writer
	warn   sync.Once
}

func (t *watchTelemetry) Append(event watchlog.Event) {
	if t == nil || t.logger == nil {
		return
	}
	if err := t.logger.Append(event); err != nil {
		t.warn.Do(func() {
			if t.stderr != nil {
				_, _ = fmt.Fprintln(t.stderr, "warning: watch telemetry unavailable")
			}
		})
	}
}

type watchIngester interface {
	DataRoot() string
	Backfill(context.Context) (scribe.Summary, error)
	IngestPath(context.Context, string) (scribe.Event, error)
}

type watchActivation struct {
	watch   homeWatch
	summary scribe.Summary
	err     error
}

type watchSeatStore interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func runWatch(
	ctx context.Context, dbPath string, pairs []homePair, values scribeFlags,
	stdin io.Reader, stdout, stderr io.Writer,
) int {
	return runWatchWithDatabase(ctx, dbPath, pairs, values, stdin, stdout, stderr, openWatchDatabase)
}

func runWatchWithDatabase(
	ctx context.Context, dbPath string, pairs []homePair, values scribeFlags,
	stdin io.Reader, stdout, stderr io.Writer, openDB func(context.Context, string) (*sql.DB, error),
) int {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go closeOnEOF(ctx, cancel, stdin)

	homes := make([]*leasedHome, 0, len(pairs))
	for _, pair := range pairs {
		if err := scribe.ValidateConfig(scribe.Config{
			Home: pair.Path, HomeID: pair.ID, Label: pair.Label, Kind: pair.Kind, SourceAgent: values.sourceAgent,
		}); err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		token, err := nerve.NewHolderToken()
		if err != nil {
			printScribeError(stdout, err)
			return exitError
		}
		home := &leasedHome{
			pair: pair, token: token, seatID: nerve.WatchSeatID(pair.ID), sourceAgent: values.sourceAgent,
			newIngester: func(ctx context.Context, db *sql.DB, home *leasedHome) (watchIngester, error) {
				return newWatchIngester(ctx, db, home)
			},
		}
		homes = append(homes, home)
	}
	if err := validateDatabasePath(dbPath); err != nil {
		printScribeError(stdout, err)
		return exitError
	}
	db, openErr := openDB(ctx, dbPath)
	if ctx.Err() != nil {
		if db != nil {
			_ = db.Close()
		}
		return exitOK
	}
	if openErr != nil && !retryableDatabaseOpenError(openErr) {
		printScribeError(stdout, openErr)
		return exitError
	}
	log := &watchTelemetry{logger: watchlog.Open(watchlog.Dir(dbPath), time.Now), stderr: stderr}
	for _, home := range homes {
		log.Append(watchlog.Event{Kind: watchlog.KindRaise, HomeID: home.pair.ID, SeatID: home.seatID})
	}
	retry := watchRetryDuration()
	var attempts atomic.Int64
	for openErr != nil {
		logWatchRetry(log, &attempts, openErr)
		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return exitOK
		case <-timer.C:
		}
		db, openErr = openDB(ctx, dbPath)
		if ctx.Err() != nil {
			if db != nil {
				_ = db.Close()
			}
			return exitOK
		}
		if openErr != nil && !retryableDatabaseOpenError(openErr) {
			printScribeError(stdout, openErr)
			return exitError
		}
	}
	defer db.Close()
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaultWatchShutdownTimeout)
		defer cleanupCancel()
		var cleanupStore watchSeatStore
		cleanupConn, err := db.Conn(cleanupCtx)
		if err == nil {
			defer cleanupConn.Close()
			if _, err := cleanupConn.ExecContext(cleanupCtx, `PRAGMA busy_timeout = 100`); err == nil {
				cleanupStore = cleanupConn
			}
		}
		for _, home := range homes {
			standDownWatchHome(cleanupCtx, cleanupStore, home, 0, log, time.Time{})
		}
	}()

	lease := watchLeaseDuration()
	msgs := make(chan watchMsg, 16)
	activations := make(chan watchActivation, len(homes))
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
	if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, retry, log, msgs, activations); err != nil {
		logWatchRetry(log, &attempts, err)
	}
	announced := false

	attemptTimer := time.NewTimer(nextWatchAttempt(homes, retry))
	defer attemptTimer.Stop()
	for {
		select {
		case <-ctx.Done():
			return exitOK
		case <-attemptTimer.C:
			if err := acquireWatchHomes(ctx, db, homes, values.pollInterval, lease, retry, log, msgs, activations); err != nil {
				logWatchRetry(log, &attempts, err)
			}
			attemptTimer.Reset(nextWatchAttempt(homes, retry))
		case activation := <-activations:
			if activation.err != nil {
				logWatchRetry(log, &attempts, activation.err)
				continue
			}
			if !announced && activation.watch.source != nil {
				watches := []homeWatch{activation.watch}
				if err := renderWatchStarts(stdout, values.asJSON, watchBackend(watches), []scribe.Summary{activation.summary}); err != nil {
					printScribeError(stdout, err)
					return exitError
				}
				announced = true
			}
		case msg := <-msgs:
			home := homes[msg.idx]
			leaseCtx, held := watchHomeLease(home, msg.generation, time.Now().UTC())
			if !held {
				standDownWatchHome(ctx, db, home, msg.generation, log, time.Now().UTC().Add(retry))
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

func newWatchIngester(ctx context.Context, db *sql.DB, home *leasedHome) (*scribe.Ingester, error) {
	return scribe.New(ctx, db, scribe.Config{
		Home: home.pair.Path, HomeID: home.pair.ID, Label: home.pair.Label,
		Kind: home.pair.Kind, SourceAgent: home.sourceAgent,
		CommitFence: func(ctx context.Context, tx *sql.Tx) error {
			return nerve.FenceSeat(ctx, tx, home.seatID, home.token, time.Now().UTC())
		},
	})
}

func acquireWatchHomes(
	ctx context.Context, db *sql.DB, homes []*leasedHome, poll, lease, retry time.Duration,
	log *watchTelemetry, msgs chan watchMsg, activations chan<- watchActivation,
) error {
	var failures []error
	for i, home := range homes {
		now := time.Now().UTC()
		home.mu.Lock()
		if home.holding || now.Before(home.retryAt) {
			home.mu.Unlock()
			continue
		}
		prepared := home.prepared
		factory := home.newIngester
		home.mu.Unlock()
		if factory == nil {
			factory = func(ctx context.Context, db *sql.DB, home *leasedHome) (watchIngester, error) {
				return newWatchIngester(ctx, db, home)
			}
		}
		if !prepared {
			err := scribe.EnsureHome(ctx, db, scribe.Config{
				Home: home.pair.Path, HomeID: home.pair.ID, Label: home.pair.Label,
				Kind: home.pair.Kind, SourceAgent: home.sourceAgent,
			})
			if err != nil {
				home.mu.Lock()
				home.retryAt = time.Now().UTC().Add(retry)
				home.standbyUntil = time.Time{}
				home.mu.Unlock()
				failures = append(failures, err)
				continue
			}
			home.mu.Lock()
			home.prepared = true
			home.mu.Unlock()
		}
		acquireNow := time.Now().UTC()
		home.mu.Lock()
		held, seat, err := nerve.TryAcquireSeat(ctx, db, nerve.SeatConfig{
			SeatID: home.seatID, HomeID: home.pair.ID, HolderToken: home.token,
			Label: "watch", Destination: "machine", Now: acquireNow, Lease: lease,
		})
		if err != nil {
			home.retryAt = acquireNow.Add(retry)
			home.standbyUntil = time.Time{}
			home.mu.Unlock()
			failures = append(failures, err)
			continue
		}
		if !held {
			deadline, parseErr := time.Parse(time.RFC3339Nano, seat.LeaseUntil)
			if parseErr == nil && deadline.After(acquireNow) {
				home.standbyUntil = deadline.UTC()
			} else {
				home.standbyUntil = time.Time{}
			}
			home.mu.Unlock()
			continue
		}
		home.holding = true
		home.standbyUntil = time.Time{}
		home.leaseUntil = acquireNow.Add(lease)
		home.generation++
		generation := home.generation
		leaseCtx, leaseCancel := context.WithCancel(ctx)
		home.leaseCtx = leaseCtx
		home.leaseCancel = leaseCancel
		home.mu.Unlock()
		log.Append(watchlog.Event{Kind: watchlog.KindLeaseAcquired, HomeID: home.pair.ID, SeatID: seat.SeatID})
		go activateWatchHome(ctx, leaseCtx, db, home, i, generation, factory, poll, lease, retry, log, msgs, activations)
	}
	return errors.Join(failures...)
}

func activateWatchHome(
	ctx, leaseCtx context.Context, db *sql.DB, home *leasedHome, idx int, generation uint64,
	factory func(context.Context, *sql.DB, *leasedHome) (watchIngester, error),
	poll, lease, retry time.Duration, log *watchTelemetry, msgs chan watchMsg,
	activations chan<- watchActivation,
) {
	activate := func(value watchActivation) {
		select {
		case activations <- value:
		case <-ctx.Done():
		}
	}
	ingester, err := factory(leaseCtx, db, home)
	if err != nil {
		standDownWatchHome(ctx, db, home, generation, log, time.Now().UTC().Add(retry))
		activate(watchActivation{err: err})
		return
	}
	home.mu.Lock()
	active := home.holding && home.generation == generation
	if active {
		home.ingester = ingester
	}
	home.mu.Unlock()
	if !active {
		return
	}
	source, err := filewatch.New(ingester.DataRoot(), poll)
	if err != nil {
		err = fmt.Errorf("watch: %w", err)
		standDownWatchHome(ctx, db, home, generation, log, time.Now().UTC().Add(retry))
		activate(watchActivation{err: err})
		return
	}
	home.mu.Lock()
	active = home.holding && home.generation == generation
	if active {
		home.source = source
	}
	home.mu.Unlock()
	if !active {
		_ = source.Close()
		return
	}
	summary, err := ingester.Backfill(leaseCtx)
	if err != nil {
		standDownWatchHome(ctx, db, home, generation, log, time.Now().UTC().Add(retry))
		activate(watchActivation{err: err})
		return
	}
	home.mu.Lock()
	if !home.holding || home.generation != generation {
		home.mu.Unlock()
		return
	}
	fenced, deadline, err := renewWatchSeat(leaseCtx, db, home.seatID, home.token, lease)
	if err != nil || !fenced {
		closedSource, lost := standDownWatchHomeLocked(ctx, db, home, generation, time.Now().UTC().Add(retry))
		home.mu.Unlock()
		finishWatchStandDown(home, closedSource, lost, log)
	} else {
		home.retryAt = time.Time{}
		home.leaseUntil = deadline
		go pumpWatch(leaseCtx, idx, generation, source, msgs)
		home.mu.Unlock()
	}
	if err != nil {
		activate(watchActivation{err: err})
		return
	}
	if !fenced {
		return
	}
	log.Append(watchlog.Event{
		Kind: watchlog.KindSweep, HomeID: home.pair.ID, SeatID: home.seatID,
		Scanned: summary.Scanned, Inserted: summary.Inserted,
		Unchanged: summary.Unchanged, Wakeups: summary.Wakeups,
	})
	activate(watchActivation{watch: homeWatch{ingester: ingester, source: source}, summary: summary})
}

func renewWatchSeat(
	ctx context.Context, db *sql.DB, seatID, token string, lease time.Duration,
) (bool, time.Time, error) {
	for range 2 {
		renewedAt := time.Now().UTC()
		held, err := nerve.RenewSeat(ctx, db, seatID, token, renewedAt, lease)
		if err != nil || !held {
			return held, time.Time{}, err
		}
		deadline := renewedAt.Add(lease)
		if time.Now().UTC().Before(deadline) {
			return true, deadline, nil
		}
	}
	return false, time.Time{}, nil
}

func renewWatchHomes(ctx context.Context, db *sql.DB, homes []*leasedHome, lease, retry time.Duration, log *watchTelemetry) error {
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
		held, deadline, err := renewWatchSeat(ctx, db, home.seatID, home.token, lease)
		if err != nil || !held {
			closedSource, lost = standDownWatchHomeLocked(ctx, db, home, generation, time.Now().UTC().Add(retry))
		} else {
			home.leaseUntil = deadline
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
	log *watchTelemetry, attempts *atomic.Int64,
) {
	ticker := time.NewTicker(watchHeartbeatDuration(lease, retry))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := renewWatchHomes(ctx, db, homes, lease, retry, log); err != nil {
				if ctx.Err() != nil {
					return
				}
				logWatchRetry(log, attempts, err)
			}
		}
	}
}

func standDownWatchHome(
	ctx context.Context, db watchSeatStore, home *leasedHome, generation uint64,
	log *watchTelemetry, retryAt time.Time,
) {
	home.mu.Lock()
	closedSource, lost := standDownWatchHomeLocked(ctx, db, home, generation, retryAt)
	home.mu.Unlock()
	finishWatchStandDown(home, closedSource, lost, log)
}

func standDownWatchHomeLocked(
	ctx context.Context, db watchSeatStore, home *leasedHome, generation uint64, retryAt time.Time,
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
	home.leaseUntil = time.Time{}
	home.standbyUntil = time.Time{}
	if lost {
		home.holding = false
		if db != nil {
			_ = nerve.ReleaseSeat(ctx, db, home.seatID, home.token, time.Now().UTC())
		}
	}
	home.retryAt = retryAt
	return closedSource, lost
}

func finishWatchStandDown(home *leasedHome, source filewatch.Source, lost bool, log *watchTelemetry) {
	if source != nil {
		_ = source.Close()
	}
	if lost {
		log.Append(watchlog.Event{Kind: watchlog.KindLeaseLost, HomeID: home.pair.ID, SeatID: home.seatID})
	}
}

func watchHomeLease(home *leasedHome, generation uint64, now time.Time) (context.Context, bool) {
	home.mu.Lock()
	defer home.mu.Unlock()
	if !home.holding || home.generation != generation || home.leaseCtx == nil || !now.Before(home.leaseUntil) {
		return nil, false
	}
	return home.leaseCtx, true
}

func logWatchRetry(log *watchTelemetry, attempts *atomic.Int64, err error) {
	attempt := attempts.Add(1)
	log.Append(watchlog.Event{Kind: watchlog.KindCrashRetry, Attempt: int(attempt), Err: watchErrorCode(err)})
}

func watchErrorCode(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "not-found"
	case errors.Is(err, os.ErrPermission):
		return "permission-denied"
	case errors.Is(err, context.Canceled):
		return "context-canceled"
	default:
		return "runtime-failure"
	}
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

func nextWatchAttempt(homes []*leasedHome, retry time.Duration) time.Duration {
	now := time.Now().UTC()
	next := retry
	for _, home := range homes {
		home.mu.Lock()
		holding := home.holding
		retryAt := home.retryAt
		standbyUntil := home.standbyUntil
		home.mu.Unlock()
		if holding {
			continue
		}
		if retryAt.IsZero() {
			retryAt = now.Add(retry)
		}
		attemptAt := retryAt
		if !standbyUntil.IsZero() && standbyUntil.Before(attemptAt) {
			attemptAt = standbyUntil
		}
		delay := attemptAt.Sub(now)
		if delay <= 0 {
			return time.Millisecond
		}
		if delay < next {
			next = delay
		}
	}
	return next
}

func ingestWatchPath(ctx context.Context, home *leasedHome, path string, asJSON bool, stdout io.Writer, log *watchTelemetry) error {
	if path == "" {
		return nil
	}
	if filepath.Clean(path) == filepath.Clean(home.ingester.DataRoot()) {
		recovery, err := home.ingester.Backfill(ctx)
		if err != nil {
			return err
		}
		log.Append(watchlog.Event{
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
	log.Append(watchlog.Event{
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
