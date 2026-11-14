package logx

import (
	"context"
	"log"
	"log/slog"
	"strings"
)

// LevelTraceSlog is the slog level of LevelTrace lines, below slog.LevelDebug.
const LevelTraceSlog = slog.LevelDebug - 4

// slogLevel follows SetLevel, so the structured and the Printf lines share one level.
var slogLevel = new(slog.LevelVar)

// logOutput writes each slog record through the log package, so a structured line gets the same
// timestamp prefix, the same output and the same bugtrack scan as a log.Printf line.
type logOutput struct{}

func (logOutput) Write(p []byte) (int, error) {
	if err := log.Output(4, strings.TrimSuffix(string(p), "\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

var logger = slog.New(slog.NewTextHandler(logOutput{}, &slog.HandlerOptions{
	Level: slogLevel,
	ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
		if len(groups) > 0 {
			return a
		}
		switch a.Key {
		case slog.TimeKey:
			return slog.Attr{} // the log package prefix carries the time
		case slog.LevelKey:
			if lv, ok := a.Value.Any().(slog.Level); ok && lv <= LevelTraceSlog {
				return slog.String(slog.LevelKey, "TRACE")
			}
		}
		return a
	},
}))

// L returns the structured logger. Attach the IDs a reader would filter on with With, for
// example logx.L().With("char", charID). See docs/go-idioms.md#logging.
func L() *slog.Logger { return logger }

// Conn returns the logger for one client connection, tagged with its address and the account
// and character it is logged in as (0 before login).
func Conn(addr string, accountID, charID int64) *slog.Logger {
	return logger.With("addr", addr, "account", accountID, "char", charID)
}

// Trace logs a per-packet record at LevelTraceSlog. Off at the default level.
func Trace(l *slog.Logger, msg string, args ...any) {
	l.Log(context.Background(), LevelTraceSlog, msg, args...)
}

// syncSlogLevel maps the logx level onto slogLevel. Error lines are never gated.
func syncSlogLevel(lvl int32) {
	switch lvl {
	case LevelError:
		slogLevel.Set(slog.LevelWarn)
	case LevelDebug:
		slogLevel.Set(slog.LevelDebug)
	case LevelTrace:
		slogLevel.Set(LevelTraceSlog)
	default:
		slogLevel.Set(slog.LevelInfo)
	}
}
