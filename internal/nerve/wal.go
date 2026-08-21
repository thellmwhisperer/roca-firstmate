/*
*
@overview WAL signal abstraction used by Nerve follow. ~30 lines, no public symbols.

		READING GUIDE
		-------------
	  1. Start at walSource                  <- platform contract
		2. Open wal_darwin.go or wal_poll.go  <- implementation

		MAIN FLOW
		---------
		newWALSource -> Signals -> Follow drain

		PUBLIC API
		----------
		none

		INTERNALS
		---------
		walSource, newWALSource

@exports
@deps platform-specific filesystem notification
*/
package nerve

type walSource interface {
	Signals() <-chan struct{}
	Errors() <-chan error
	Close() error
}
