// Package watchlog writes session-owned watcher telemetry as local JSONL.
// It never writes a database table.
package watchlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	KindRaise         = "raise"
	KindLeaseAcquired = "lease-acquired"
	KindLeaseLost     = "lease-lost"
	KindSweep         = "sweep"
	KindCrashRetry    = "crash-retry"
)

// Event is one JSONL telemetry line. Counts and identities only; no file
// contents and no absolute home paths.
type Event struct {
	Timestamp time.Time `json:"timestamp"`
	Kind      string    `json:"kind"`
	HomeID    string    `json:"home_id,omitempty"`
	SeatID    string    `json:"seat_id,omitempty"`
	Scanned   int       `json:"scanned,omitempty"`
	Inserted  int       `json:"inserted,omitempty"`
	Unchanged int       `json:"unchanged,omitempty"`
	Wakeups   int       `json:"wakeups,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	Err       string    `json:"error,omitempty"`
}

// Logger appends JSONL lines under a logs directory next to firstmate.db.
type Logger struct {
	dir string
	now func() time.Time
	mu  sync.Mutex
}

func Open(dir string, now func() time.Time) *Logger {
	if now == nil {
		now = time.Now
	}
	return &Logger{dir: dir, now: now}
}

func Dir(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "logs")
}

func (l *Logger) Append(event Event) error {
	if l == nil {
		return nil
	}
	if event.Kind == "" {
		return fmt.Errorf("watch log kind is required")
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = l.now().UTC()
	} else {
		event.Timestamp = event.Timestamp.UTC()
	}
	line, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode watch log: %w", err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return fmt.Errorf("create watch log directory: %w", err)
	}
	path := filepath.Join(l.dir, "watch-"+event.Timestamp.Format(time.DateOnly)+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open watch log: %w", err)
	}
	_, writeErr := file.Write(append(line, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("append watch log: %w", writeErr)
	}
	return closeErr
}
