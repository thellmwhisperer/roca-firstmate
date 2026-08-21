// Package watch provides Scribe's recursive filesystem event stream. macOS
// uses native FSEvents when cgo is available; every other build polls.
package watch

import (
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thellmwhisperer/la-roca/pkg/incrementality"
)

const defaultPollInterval = 30 * time.Second

// Source emits changed Markdown paths. A root path event asks the consumer to
// run a complete fingerprint scan after an event drop or directory change.
type Source interface {
	Backend() string
	Events() <-chan string
	Errors() <-chan error
	Close() error
}

// New starts the best available recursive watcher for root. Native startup
// failure on macOS degrades to polling instead of disabling continuous ingest.
func New(root string, pollInterval time.Duration) (Source, error) {
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}
	return newPlatform(root, pollInterval)
}

type pollSource struct {
	root   string
	events chan string
	errors chan error
	done   chan struct{}
	once   sync.Once
}

func newPolling(root string, interval time.Duration) (Source, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	state, err := markdownState(root)
	if err != nil {
		return nil, err
	}
	source := &pollSource{
		root: root, events: make(chan string, 256), errors: make(chan error, 8), done: make(chan struct{}),
	}
	go source.loop(interval, state)
	return source, nil
}

func (p *pollSource) Backend() string       { return "polling" }
func (p *pollSource) Events() <-chan string { return p.events }
func (p *pollSource) Errors() <-chan error  { return p.errors }
func (p *pollSource) Close() error          { p.once.Do(func() { close(p.done) }); return nil }
func (p *pollSource) emit(path string) bool {
	select {
	case p.events <- path:
		return true
	case <-p.done:
		return false
	}
}
func (p *pollSource) emitError(err error) bool {
	select {
	case p.errors <- err:
		return true
	case <-p.done:
		return false
	}
}

func (p *pollSource) loop(interval time.Duration, previous map[string]string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(p.events)
	defer close(p.errors)
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			current, err := markdownState(p.root)
			if err != nil {
				if !p.emitError(err) {
					return
				}
				continue
			}
			changed := make([]string, 0)
			for path, fingerprint := range current {
				if previous[path] == fingerprint {
					continue
				}
				changed = append(changed, path)
			}
			sort.Strings(changed)
			for _, path := range changed {
				if !p.emit(path) {
					return
				}
			}
			previous = current
		}
	}
}

func markdownState(root string) (map[string]string, error) {
	state := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".md") {
			return nil
		}
		if entry.Type()&fs.ModeType != 0 {
			return nil
		}
		fingerprint, err := incrementality.Fingerprint(path)
		if err != nil {
			return err
		}
		state[path] = fingerprint
		return nil
	})
	return state, err
}
