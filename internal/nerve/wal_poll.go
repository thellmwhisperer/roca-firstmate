//go:build !darwin || !cgo

/*
*
@overview Portable seconds-latency fallback for builds without FSEvents. ~110 lines.

	READING GUIDE
	-------------
	1. Start at newWALSource  <- captures initial database state
	2. Read loop              <- emits when database/WAL state moves

	MAIN FLOW
	---------
	ticker -> stat db files -> changed snapshot -> signal

	PUBLIC API
	----------
	none

	INTERNALS
	---------
	pollingWALSource, walState, loop

@exports
@deps os.Stat; time.Ticker
*/
package nerve

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

type pollingWALSource struct {
	dbPath  string
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
	previous, err := walState(abs)
	if err != nil {
		return nil, err
	}
	source := &pollingWALSource{
		dbPath: abs, signals: make(chan struct{}, 1), errors: make(chan error, 1), done: make(chan struct{}),
	}
	go source.loop(previous)
	return source, nil
}

func (p *pollingWALSource) Signals() <-chan struct{} { return p.signals }
func (p *pollingWALSource) Errors() <-chan error     { return p.errors }
func (p *pollingWALSource) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *pollingWALSource) loop(previous string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer close(p.signals)
	defer close(p.errors)
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
			current, err := walState(p.dbPath)
			if err != nil {
				select {
				case p.errors <- err:
				case <-p.done:
				}
				continue
			}
			if current == previous {
				continue
			}
			previous = current
			select {
			case p.signals <- struct{}{}:
			default:
			}
		}
	}
}

func walState(dbPath string) (string, error) {
	state := ""
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		info, err := os.Stat(path)
		if err != nil {
			if os.IsNotExist(err) {
				state += "missing|"
				continue
			}
			return "", err
		}
		state += info.ModTime().UTC().Format(time.RFC3339Nano) + ":" + strconv.FormatInt(info.Size(), 10) + "|"
	}
	return state, nil
}
