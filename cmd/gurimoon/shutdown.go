package main

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"gurimoon/safego"
	"gurimoon/tick"
)

// shutdownWait bounds how long a clean stop waits for tick work in flight; shutdownFlushTimeout
// bounds the final asteroid flush. Both are set at boot from server.shutdownWaitSec and
// server.shutdownFlushSec.
var (
	shutdownWait         = 20 * time.Second
	shutdownFlushTimeout = 60 * time.Second
)

// handshakeTimeout is how long a new connection has to finish version exchange and login;
// unauthIdleKick drops a connection left unauthenticated that long (server.handshakeTimeoutSec,
// server.unauthIdleKickSec).
var (
	handshakeTimeout = 30 * time.Second
	unauthIdleKick   = 2 * time.Minute
)

// flushOnShutdown writes the state held only in memory between periodic flushes (asteroid
// depletion, structure damage) so a clean stop loses nothing. Order: stop the clock so no sweep
// starts, let running ones finish (one may be the asteroid flush), then drain.
func flushOnShutdown(sqldb *sql.DB, tickMgr *tick.Manager) {
	start := time.Now()
	tickMgr.Stop()
	if !tickMgr.Wait(shutdownWait) {
		log.Printf("[shutdown] tick work still running after %v — flushing anyway", shutdownWait)
	}

	waitShutdownWork()
	finalAsteroidFlush(sqldb)
	flushStructureDamage(sqldb)
	if sqldb != nil {
		stampStarbaseFuelSweep(sqldb)
	}
	log.Printf("[shutdown] final flush done in %v", time.Since(start).Round(time.Millisecond))
}

// finalAsteroidFlush drains every loaded system's dirty rock set. The periodic flush runs behind
// asteroidFlushRunning, which tickMgr.Wait cannot see; two racing drains could land an older
// quantity over a newer one, so this takes the same guard.
func finalAsteroidFlush(sqldb *sql.DB) {
	deadline := time.Now().Add(shutdownWait)
	for !asteroidFlushRunning.CompareAndSwap(false, true) {
		if time.Now().After(deadline) {
			log.Printf("[shutdown] asteroid flush still running after %v — skipping final asteroid flush", shutdownWait)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer asteroidFlushRunning.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), shutdownFlushTimeout)
	defer cancel()
	systems, rocks := flushAllAsteroids(ctx, sqldb)
	log.Printf("[shutdown] asteroid depletion: wrote %d rocks across %d systems", rocks, systems)
}

// shutdownWork counts fire-and-forget writes that a clean stop should let
// finish: a pilot who died a second before Ctrl+C should still get a
// killboard row and a kill report.
var shutdownWork sync.WaitGroup

// goShutdownSafe runs fn on its own goroutine, counted in shutdownWork. The
// Add happens here, before the goroutine exists, so a Wait that starts right
// after the call cannot miss it.
func goShutdownSafe(fn func()) {
	shutdownWork.Add(1)
	go func() {
		defer safego.Recover("goShutdownSafe")
		defer shutdownWork.Done()
		fn()
	}()
}

// fileKillmail records a kill on the killboard and sends its CONCORD kill
// reports. Two goroutines, not one: the mail must go out even if the
// killboard write fails, and the row must land even if the mail does not.
// km is copied by the call, so the caller may go on changing its own.
func fileKillmail(sqldb *sql.DB, km killReport, isPod bool) {
	goShutdownSafe(func() { recordKillmail(sqldb, km, isPod) })
	goShutdownSafe(func() { sendKillReportMails(sqldb, km) })
}

// waitShutdownWork gives in-flight writes until shutdownWait to finish.
func waitShutdownWork() {
	done := make(chan struct{})
	go func() {
		defer safego.Recover("waitShutdownWork")
		shutdownWork.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(shutdownWait):
		log.Printf("[shutdown] killmail writes still running after %v — exiting without them", shutdownWait)
	}
}
