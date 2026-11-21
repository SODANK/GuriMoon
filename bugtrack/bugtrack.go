// Package bugtrack is the server's automatic bug tracker. It collects bug signals from a
// log-stream interceptor (Writer, teed into the stdlib log output) and from direct Report calls,
// deduplicates them by a normalized signature and persists them to bug_reports for the ESP admin
// panel (/bugs).
//
// Producers push onto a buffered channel and never block; events are dropped when it is full.
// One consumer goroutine coalesces hits per signature and flushes every 30 seconds in one transaction.
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

// The flush pace. Every log line with "error", "failed" and the like becomes an
// event, so a noisy server produces many signatures. Each flush used to upsert every
// pending signature as its own commit every 2 s, rewriting its 2 KB sample each time;
// on a stock MariaDB that is a steady stream of fsyncs and page writes. Now one
// transaction every 30 s carries at most flushMaxRows signatures (the rest wait for
// the next flush), and the sample is written once.
const (
	flushEvery   = 30 * time.Second
	flushMaxRows = 200
	// pruneEvery / pruneKeep bound the table: rows past the newest pruneKeep (by
	// lastSeen) that nobody wrote notes on are deleted hourly.
	pruneEvery = time.Hour
	pruneKeep  = 5000
)

// run coalesces incoming events per signature and flushes them to the database on
// a slow ticker, so a hot error loop costs one row per flush instead of one per
// occurrence.
func run(sqldb *sql.DB) {
	pending := make(map[string]*event)
	flush := time.NewTicker(flushEvery)
	defer flush.Stop()
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	for {
		select {
		case ev := <-events:
			if p, ok := pending[ev.signature]; ok {
				p.count += ev.count
				p.when = ev.when
			} else {
				e := ev
				pending[ev.signature] = &e
			}
		case <-flush.C:
			flushPending(sqldb, pending)
		case <-prune.C:
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			if n, err := evedb.PruneBugReports(ctx, sqldb, pruneKeep); err != nil {
				log.Printf("bugtrack: prune: %v", err)
			} else if n > 0 {
				log.Printf("bugtrack: pruned %d old report(s)", n)
			}
			cancel()
		}
	}
}

// flushPending writes up to flushMaxRows pending signatures in one transaction and
// removes them from pending. On error they stay pending for the next flush.
func flushPending(sqldb *sql.DB, pending map[string]*event) {
	if len(pending) == 0 {
		return
	}
	batch := make([]evedb.BugReportHit, 0, min(len(pending), flushMaxRows))
	for sig, ev := range pending {
		if len(batch) >= flushMaxRows {
			break
		}
		batch = append(batch, evedb.BugReportHit{
			Signature: sig, Category: ev.category, Service: ev.service, Method: ev.method,
			Sample: ev.sample, Count: ev.count, When: ev.when,
		})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := evedb.UpsertBugReports(ctx, sqldb, batch)
	cancel()
	if err != nil {
		// The "bugtrack:" prefix makes the interceptor skip this line, so a
		// database outage cannot feed back into itself.
		log.Printf("bugtrack: upsert %d report(s): %v", len(batch), err)
		if len(pending) > 4*flushMaxRows {
			// The database is down and the map would grow without end: keep the
			// newest occurrences' counts only for what the next flush can carry.
			for _, h := range batch {
				delete(pending, h.Signature)
			}
		}
		return
	}
	for _, h := range batch {
		delete(pending, h.Signature)
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

// normalize maps the parts of a message that vary between occurrences of the same
// bug onto placeholders, so they share one signature: every digit run becomes '#'
// (timestamps, addresses, IDs, coordinates), a 0x hex literal or a long hex run
// (pointers, hashes) becomes '#', and a quoted string ("..." or '...', which is
// where %q puts names and method strings) becomes "#".
func normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevHash := false
	emitHash := func() {
		if !prevHash {
			b.WriteByte('#')
		}
		prevHash = true
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			// A quoted span up to the matching quote on the same line.
			if j := strings.IndexByte(s[i+1:], c); j >= 0 && !strings.ContainsRune(s[i+1:i+1+j], '\n') {
				b.WriteByte(c)
				b.WriteByte('#')
				b.WriteByte(c)
				prevHash = false
				i += j + 2
				continue
			}
		case c == '0' && i+2 < len(s) && (s[i+1] == 'x' || s[i+1] == 'X') && isHex(s[i+2]):
			j := i + 2
			for j < len(s) && isHex(s[j]) {
				j++
			}
			emitHash()
			i = j
			continue
		case isHex(c):
			// A run of 8+ hex characters with at least one digit and at a word
			// boundary is an ID or hash, not a word.
			j := i
			digit := false
			for j < len(s) && isHex(s[j]) {
				if s[j] >= '0' && s[j] <= '9' {
					digit = true
				}
				j++
			}
			atStart := i == 0 || !isWordByte(s[i-1])
			atEnd := j == len(s) || !isWordByte(s[j])
			if j-i >= 8 && digit && atStart && atEnd {
				emitHash()
				i = j
				continue
			}
		}
		if c >= '0' && c <= '9' {
			emitHash()
			i++
			continue
		}
		prevHash = false
		b.WriteByte(c)
		i++
	}
	return b.String()
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isWordByte(c byte) bool {
	return isHex(c) || (c >= 'g' && c <= 'z') || (c >= 'G' && c <= 'Z') || c == '_'
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
