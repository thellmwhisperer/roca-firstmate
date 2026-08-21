//go:build darwin && cgo

package watch

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsevents"
)

var errClosed = errors.New("FSEvents stream closed")

type fseventSource struct {
	root   string
	stream *fsevents.EventStream
	events chan string
	errors chan error
	done   chan struct{}
	once   sync.Once
}

func newPlatform(root string, pollInterval time.Duration) (Source, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	nativeEvents := make(chan []fsevents.Event, 32)
	stream := &fsevents.EventStream{
		Events:  nativeEvents,
		Paths:   []string{abs},
		Flags:   fsevents.FileEvents | fsevents.NoDefer | fsevents.WatchRoot,
		Latency: 100 * time.Millisecond,
	}
	if err := stream.Start(); err != nil {
		return newPolling(abs, pollInterval)
	}
	source := &fseventSource{
		root: abs, stream: stream, events: make(chan string, 256),
		errors: make(chan error, 1), done: make(chan struct{}),
	}
	go source.loop(nativeEvents)
	return source, nil
}

func (f *fseventSource) Backend() string       { return "fsevents" }
func (f *fseventSource) Events() <-chan string { return f.events }
func (f *fseventSource) Errors() <-chan error  { return f.errors }
func (f *fseventSource) Close() error {
	f.once.Do(func() {
		close(f.done)
		f.stream.Stop()
	})
	return nil
}

func (f *fseventSource) loop(input <-chan []fsevents.Event) {
	defer close(f.events)
	defer close(f.errors)
	for {
		select {
		case <-f.done:
			return
		case batch, ok := <-input:
			if !ok {
				select {
				case f.errors <- errClosed:
				case <-f.done:
				}
				return
			}
			rescan := false
			for _, event := range batch {
				if event.Flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped) != 0 {
					rescan = true
					continue
				}
				if event.Flags&fsevents.ItemIsDir != 0 {
					rescan = true
					continue
				}
				if !strings.EqualFold(filepath.Ext(event.Path), ".md") {
					continue
				}
				select {
				case f.events <- event.Path:
				case <-f.done:
					return
				}
			}
			if rescan {
				select {
				case f.events <- f.root:
				case <-f.done:
					return
				}
			}
		}
	}
}
