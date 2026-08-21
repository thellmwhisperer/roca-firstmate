//go:build !darwin || !cgo

package watch

import "time"

func newPlatform(root string, pollInterval time.Duration) (Source, error) {
	return newPolling(root, pollInterval)
}
