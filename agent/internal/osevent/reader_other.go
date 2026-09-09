//go:build !linux && !windows && !darwin

package osevent

import "errors"

// defaultReader has no supported implementation on this platform: the
// agent starts without an event-log tail (a startup warning, not a failure).
func defaultReader() (Reader, error) {
	return nil, errors.New("no OS event log reader for this platform")
}
