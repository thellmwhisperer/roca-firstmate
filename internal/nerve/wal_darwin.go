//go:build darwin && cgo

/*
*
@overview Native millisecond WAL notification through macOS FSEvents. ~120 lines.

	READING GUIDE
	-------------
	1. Start at newWALSource  <- opens the native stream
	2. Read loop              <- filters database, WAL, and dropped events

	MAIN FLOW
	---------
	FSEvents batch -> relevant path/drop -> coalesced signal

	PUBLIC API
	----------
	none

	INTERNALS
	---------
	fseventWALSource, loop, signal

@exports
@deps github.com/fsnotify/fsevents
*/
package nerve

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsevents"
)

type fseventWALSource struct {
	dbPath  string
	stream  *fsevents.EventStream
	signals chan struct{}
	errors  chan error
	done    chan struct{}
	once    sync.Once
}

func newWALSource(dbPath string) (walSource, error) {
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = resolved
	}
	nativeEvents := make(chan []fsevents.Event, 32)
	stream := &fsevents.EventStream{
		Events:  nativeEvents,
		Paths:   []string{filepath.Dir(abs)},
		Flags:   fsevents.FileEvents | fsevents.NoDefer | fsevents.WatchRoot,
		Latency: 20 * time.Millisecond,
	}
	if err := stream.Start(); err != nil {
		return nil, err
	}
	source := &fseventWALSource{
		dbPath: abs, stream: stream, signals: make(chan struct{}, 1),
		errors: make(chan error, 1), done: make(chan struct{}),
	}
	go source.loop(nativeEvents)
	return source, nil
}

func (f *fseventWALSource) Signals() <-chan struct{} { return f.signals }
func (f *fseventWALSource) Errors() <-chan error     { return f.errors }
func (f *fseventWALSource) Close() error {
	f.once.Do(func() {
		close(f.done)
		f.stream.Stop()
	})
	return nil
}

func (f *fseventWALSource) loop(input <-chan []fsevents.Event) {
	defer close(f.signals)
	defer close(f.errors)
	for {
		select {
		case <-f.done:
			return
		case batch, ok := <-input:
			if !ok {
				select {
				case f.errors <- errors.New("FSEvents WAL stream closed"):
				case <-f.done:
				}
				return
			}
			for _, event := range batch {
				if event.Flags&(fsevents.MustScanSubDirs|fsevents.UserDropped|fsevents.KernelDropped) != 0 || f.relevant(event.Path) {
					f.signal()
					break
				}
			}
		}
	}
}

func (f *fseventWALSource) relevant(path string) bool {
	path = filepath.Clean(path)
	return path == f.dbPath || strings.HasPrefix(path, f.dbPath+"-")
}

func (f *fseventWALSource) signal() {
	select {
	case f.signals <- struct{}{}:
	default:
	}
}
