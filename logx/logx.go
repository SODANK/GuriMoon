// Package logx adds a level to the standard log output. The level gates success-path trace only
// (Tracef, Debugf, Infof); errors, warnings and anything describing a failure stay on log.Printf
// or an Error-level L() record and are never gated, because bugtrack classifies every line it
// sees. L returns a slog logger that writes through the same output.
// See docs/support-notes.md#log-levels-and-the-bug-tracker.
package logx

import (
	"log"
	"strings"
	"sync/atomic"
)

// Levels, in increasing verbosity.
const (
	LevelError = iota // failures only
	LevelInfo         // lifecycle: startup, login, system load (the default)
	LevelDebug        // per-call detail
	LevelTrace        // per-packet detail
)

// level is an atomic because gated call sites read it on every call and a runtime toggle (slash
// command, ESP control) must not race.
var level int32 = LevelInfo

// SetLevel sets the global log level. Unknown names leave it unchanged and
// report false, so a typo in config.json is visible rather than silently
// turning the log off.
func SetLevel(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "error":
		atomic.StoreInt32(&level, LevelError)
	case "", "info":
		atomic.StoreInt32(&level, LevelInfo)
	case "debug":
		atomic.StoreInt32(&level, LevelDebug)
	case "trace":
		atomic.StoreInt32(&level, LevelTrace)
	default:
		return false
	}
	syncSlogLevel(atomic.LoadInt32(&level))
	return true
}

// Level returns the current level.
func Level() int { return int(atomic.LoadInt32(&level)) }

// LevelName returns the current level as it is written in config.json.
func LevelName() string {
	switch Level() {
	case LevelError:
		return "error"
	case LevelDebug:
		return "debug"
	case LevelTrace:
		return "trace"
	default:
		return "info"
	}
}

// Enabled reports whether lines at lvl are being emitted. Use it to skip building an expensive
// argument; Tracef and Debugf already check.
func Enabled(lvl int) bool { return Level() >= lvl }

// Tracef logs per-packet detail. Off at the default level.
func Tracef(format string, v ...any) {
	if Level() >= LevelTrace {
		log.Printf(format, v...)
	}
}

// Debugf logs per-call detail. Off at the default level.
func Debugf(format string, v ...any) {
	if Level() >= LevelDebug {
		log.Printf(format, v...)
	}
}

// Infof logs lifecycle detail.
func Infof(format string, v ...any) {
	if Level() >= LevelInfo {
		log.Printf(format, v...)
	}
}

// Errorf logs a failure. Never gated; it is also bugtrack's input.
func Errorf(format string, v ...any) { log.Printf(format, v...) }
