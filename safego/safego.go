// Package safego starts goroutines that report a panic instead of ending the process. A panic in
// a goroutine with no recover kills the whole server, so every background goroutine starts here.
// See docs/go-idioms.md#goroutines.
package safego

import (
	"log"
	"runtime/debug"
	"time"
)

// Report is set by bugtrack's init to file a recovered panic. Nil only in a binary without
// bugtrack, where the panic is still logged.
var Report func(where string, r any, stack []byte)

// Go runs fn on a new goroutine. A panic in fn is logged and reported, and ends only fn.
func Go(where string, fn func()) {
	go func() {
		defer Recover(where)
		fn()
	}()
}

// Recover logs and reports a panic. It must be deferred directly: `defer safego.Recover("x")`.
func Recover(where string) {
	if r := recover(); r != nil {
		handle(where, r)
	}
}

// Forever runs fn on a new goroutine and starts it again a second after a panic. It is for a
// loop that must outlive one bad iteration. A lock fn held when it panicked stays held.
func Forever(where string, fn func()) {
	go func() {
		for !runOnce(where, fn) {
			time.Sleep(restartDelay)
		}
	}()
}

// restartDelay spaces the restarts of a Forever loop that panics on every pass.
var restartDelay = time.Second

// runOnce runs fn and reports whether it returned without a panic.
func runOnce(where string, fn func()) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			handle(where, r)
		}
	}()
	fn()
	return true
}

func handle(where string, r any) {
	stack := debug.Stack()
	log.Printf("PANIC in %s: %v\n%s", where, r, stack)
	if Report != nil {
		Report(where, r, stack)
	}
}
