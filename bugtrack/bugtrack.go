// Package bugtrack is the server's automatic bug tracker. It collects bug signals from a
// log-stream interceptor (Writer, teed into the stdlib log output) and from direct Report calls,
// deduplicates them by a normalized signature and persists them to bug_reports for the ESP admin
// panel (/bugs).
//
// Producers push onto a buffered channel and never block; events are dropped when it is full.
// One consumer goroutine coalesces hits per signature and flushes every couple of seconds.
// Writer works before Init: events queue in the buffer until the consumer starts.
package bugtrack

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"log"
	"strings"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/safego"
)

// event is one bug occurrence headed for the database.
type event struct {
	signature string
	category  string
	service   string
	method    string
	sample    string
	count     int64
	when      int64
}

var (
	events  = make(chan event, 4096)
	started atomic.Bool
)

const maxSampleLen = 2000

func init() { safego.Report = ReportPanic }

// Init starts the background consumer that drains reported events into the
// bug_reports table.  Call once after the database is connected and
// EnsureBugReportsTable has run.  Safe to call more than once.
func Init(sqldb *sql.DB) {
	if !started.CompareAndSwap(false, true) {
		return
	}
	safego.Forever("bugtrack.run", func() { run(sqldb) })
}

// run coalesces incoming events per signature and flushes to the database on
// a short ticker, so a hot error loop costs one upsert per flush instead of
// one per occurrence.
func run(sqldb *sql.DB) {
	pending := make(map[string]*event)
	flush := time.NewTicker(2 * time.Second)
	defer flush.Stop()
	for {
		select {
		case ev := <-events:
			if p, ok := pending[ev.signature]; ok {
				p.count += ev.count
				p.when = ev.when
				p.sample = ev.sample
			} else {
				e := ev
				pending[ev.signature] = &e
			}
		case <-flush.C:
			for sig, ev := range pending {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				err := evedb.UpsertBugReport(ctx, sqldb, sig, ev.category,
					ev.service, ev.method, ev.sample, ev.count, ev.when)
				cancel()
				if err != nil {
					// The "bugtrack:" prefix makes the interceptor skip this
					// line, so a database outage cannot feed back into itself.
					log.Printf("bugtrack: upsert %s: %v", sig, err)
				}
				delete(pending, sig)
			}
		}
	}
}

// Report files a bug occurrence directly, with structured service/method
// context that log-line scanning cannot recover.  Never blocks.
func Report(category, service, method, detail string) {
	norm := normalize(detail)
	enqueue(event{
		signature: sigOf(category + "|" + service + "|" + method + "|" + norm),
		category:  category,
		service:   service,
		method:    method,
		sample:    clip(detail),
		count:     1,
		when:      time.Now().Unix(),
	})
}

// ReportPanic files a recovered panic with its stack trace.  The signature
// comes from the recovery site plus the normalized panic value and the top of
// the stack, so the same crash site dedupes across goroutine IDs, pointers
// and argument values.
func ReportPanic(where string, r any, stack []byte) {
	val := fmt.Sprint(r)
	top := string(stack)
	if len(top) > 400 {
		top = top[:400]
	}
	enqueue(event{
		signature: sigOf("panic|" + where + "|" + normalize(val) + "|" + normalize(top)),
		category:  "panic",
		service:   where,
		sample:    clip("panic: " + val + "\n" + string(stack)),
		count:     1,
		when:      time.Now().Unix(),
	})
}

// enqueue pushes an event without ever blocking the caller; when the buffer
// is full the event is dropped (logging must never stall on the tracker).
func enqueue(ev event) {
	select {
	case events <- ev:
	default:
	}
}

// interceptor is the io.Writer teed into the log output in main().  The
// stdlib log package issues one Write per log line, so each chunk is treated
// as one message.
type interceptor struct{}

// Writer returns the log-stream interceptor for use in io.MultiWriter.
// Usable before Init: events buffer in the channel until the consumer runs.
func Writer() interceptor { return interceptor{} }

func (interceptor) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\n")
	if cat := classify(line); cat != "" {
		enqueue(event{
			signature: sigOf(cat + "|log|" + normalize(line)),
			category:  cat,
			sample:    clip(line),
			count:     1,
			when:      time.Now().Unix(),
		})
	}
	return len(p), nil
}

// classify decides whether a log line is a bug signal and returns its
// category, or "" for lines that should not be tracked.
func classify(line string) string {
	l := strings.ToLower(line)
	switch {
	case strings.Contains(l, "bugtrack:"):
		return "" // our own logging, never fed back
	case strings.Contains(l, "panic"):
		return "panic"
	case strings.Contains(l, "unhandled") ||
		strings.Contains(l, "stub service") ||
		strings.Contains(l, "not implemented"):
		return "unhandled"
	case strings.Contains(l, "exception"):
		return "exception"
	case strings.Contains(l, "error") || strings.Contains(l, "failed"):
		return "error"
	}
	return ""
}

// normalize collapses every digit run to '#' so that timestamps, addresses,
// entity/char IDs and coordinates all map to the same signature.
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDigit := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			if !prevDigit {
				b.WriteByte('#')
			}
			prevDigit = true
			continue
		}
		prevDigit = false
		b.WriteRune(r)
	}
	return b.String()
}

// sigOf hashes a normalized key to the 16-hex-char signature used as the
// dedup key in bug_reports.
func sigOf(key string) string {
	h := fnv.New64a()
	h.Write([]byte(key))
	return fmt.Sprintf("%016x", h.Sum64())
}

func clip(s string) string {
	if len(s) > maxSampleLen {
		return s[:maxSampleLen]
	}
	return s
}
