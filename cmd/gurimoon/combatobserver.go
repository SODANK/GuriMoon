package main

// Bystander combat log: every pilot on a grid reads the fight in their combat
// log; the two parties of a hit keep their own lines and are left out. The
// seam is destiny.ObserveCombat. Client side is godma.OnDamageMessages: every
// line goes to the logger and the first line of a batch flashes mid-screen, so
// lines are coalesced per (attacker, victim) into one OnMultiEvent per witness
// per second, kills first. Names travel as strings under 'attacker'/'victim':
// the client resolves only 'source'/'target', and that needs the ball, which a
// killing blow's line would arrive after. A destroyed hit flushes at once.
// server_settings combat.observerMessages = 0 turns it off (re-read every 30 s).

import (
	"context"
	"database/sql"
	"log"
	"sync/atomic"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/marshal"
	"gurimoon/server"
	"gurimoon/tick"
)

const (
	combatObsQueue       = 4096 // events buffered between damage paths and the flusher
	combatObsMaxPerFlush = 30   // lines per witness per flush; the rest are dropped
)

type combatObsEvent struct {
	ss               *destiny.SolarSystem
	victim, attacker *destiny.Entity
	damage           float64
	destroyed        bool
}

type combatObsPair struct{ attacker, victim uint32 }

type combatObsLine struct {
	victim, attacker *destiny.Entity
	damage           float64
	hits             int64
	destroyed        bool
	seq              int
}

var (
	combatObsCh      = make(chan combatObsEvent, combatObsQueue)
	combatObsEnabled atomic.Bool
)

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		combatObsEnabled.Store(combatObsSetting(sqldb))
		destiny.CombatObserver = enqueueCombatObs
		go runCombatObserver(sqldb)
	})
}

// enqueueCombatObs runs on the damage path: no locks, never blocks.
func enqueueCombatObs(ss *destiny.SolarSystem, victim, attacker *destiny.Entity, damage float64, destroyed bool) {
	if !combatObsEnabled.Load() {
		return
	}
	select {
	case combatObsCh <- combatObsEvent{ss: ss, victim: victim, attacker: attacker, damage: damage, destroyed: destroyed}:
	default:
	}
}

func combatObsSetting(sqldb *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	v, ok, err := evedb.GetServerSetting(ctx, sqldb, "combat.observerMessages")
	if err != nil || !ok {
		return true
	}
	return v != "0"
}

func runCombatObserver(sqldb *sql.DB) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("combat observer: panic: %v", r)
		}
	}()
	pending := map[*destiny.SolarSystem]map[combatObsPair]*combatObsLine{}
	names := map[int64]string{}
	seq := 0
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case ev := <-combatObsCh:
			lines := pending[ev.ss]
			if lines == nil {
				lines = map[combatObsPair]*combatObsLine{}
				pending[ev.ss] = lines
			}
			key := combatObsPair{ev.attacker.ID, ev.victim.ID}
			ln := lines[key]
			if ln == nil {
				seq++
				ln = &combatObsLine{victim: ev.victim, attacker: ev.attacker, seq: seq}
				lines[key] = ln
			}
			if ev.damage > 0 {
				ln.damage += ev.damage
				ln.hits++
			}
			if ev.destroyed {
				ln.destroyed = true
				flushCombatObs(sqldb, pending, names)
			}
		case <-ticker.C:
			if ticks++; ticks%30 == 0 {
				combatObsEnabled.Store(combatObsSetting(sqldb))
			}
			flushCombatObs(sqldb, pending, names)
		}
	}
}

func flushCombatObs(sqldb *sql.DB, pending map[*destiny.SolarSystem]map[combatObsPair]*combatObsLine, names map[int64]string) {
	for ss, lines := range pending {
		delete(pending, ss)
		if len(lines) == 0 {
			continue
		}
		ordered := make([]*combatObsLine, 0, len(lines))
		for _, ln := range lines {
			ordered = append(ordered, ln)
		}
		// Kills lead (the batch's first line is the one that flashes), then
		// hits in the order they first landed.
		sortCombatObsLines(ordered)

		for _, w := range ss.GetAllPlayersInSystem() {
			if w.Conn == nil || w.IsInWarp() || w.InLandingGrace() {
				continue
			}
			var events []marshal.PyRep
			for _, ln := range ordered {
				if len(events) >= combatObsMaxPerFlush {
					break
				}
				if !combatObsWitness(w, ln) {
					continue
				}
				attacker := combatObsName(sqldb, names, ln.attacker)
				victim := combatObsName(sqldb, names, ln.victim)
				if ln.destroyed {
					events = append(events, combatObsEventRep("CombatObservedKill", []marshal.DictEntry{
						{Key: marshal.PyString{Value: "victim"}, Value: marshal.PyString{Value: victim}},
						{Key: marshal.PyString{Value: "attacker"}, Value: marshal.PyString{Value: attacker}},
					}))
				}
				if ln.hits == 0 {
					continue
				}
				key := "CombatObservedHit"
				entries := []marshal.DictEntry{
					{Key: marshal.PyString{Value: "attacker"}, Value: marshal.PyString{Value: attacker}},
					{Key: marshal.PyString{Value: "victim"}, Value: marshal.PyString{Value: victim}},
					{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: ln.damage}},
				}
				if ln.hits > 1 {
					key = "CombatObservedHits"
					entries = append(entries, marshal.DictEntry{Key: marshal.PyString{Value: "hits"}, Value: marshal.PyInt{Value: uint64(ln.hits)}})
				}
				events = append(events, combatObsEventRep(key, entries))
			}
			if len(events) == 0 {
				continue
			}
			args := marshal.PyTuple{Items: []marshal.PyRep{marshal.PyList{Items: events}}}
			_ = server.SendNotification(w.Conn, int64(w.AccountID), "OnMultiEvent", "charid", args)
		}
	}
}

func sortCombatObsLines(ls []*combatObsLine) {
	less := func(a, b *combatObsLine) bool {
		if a.destroyed != b.destroyed {
			return a.destroyed
		}
		return a.seq < b.seq
	}
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && less(ls[j], ls[j-1]); j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
	}
}

// combatObsWitness: w is a bystander who can see at least one side of the
// exchange. The two parties, and the pilot whose drone fired or was hit, get
// their own lines from the damage path and are left out.
func combatObsWitness(w *destiny.Entity, ln *combatObsLine) bool {
	if w.ID == ln.victim.ID || w.ID == ln.attacker.ID {
		return false
	}
	if cid := w.Slim.CharID; cid != 0 && (ln.attacker.Slim.OwnerID == cid || ln.victim.Slim.OwnerID == cid) {
		return false
	}
	return w.KnowsBall(ln.victim.ID) || w.KnowsBall(ln.attacker.ID)
}

// combatObsName is "Pilot (Hull)" for a pilot or rolebot, the type name for
// anything else. Only the flusher goroutine touches names.
func combatObsName(sqldb *sql.DB, names map[int64]string, e *destiny.Entity) string {
	tn, ok := names[e.Slim.TypeID]
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		tn, _ = evedb.GetTypeName(ctx, sqldb, e.Slim.TypeID)
		cancel()
		names[e.Slim.TypeID] = tn
	}
	switch {
	case e.CharName != "" && tn != "":
		return e.CharName + " (" + tn + ")"
	case e.CharName != "":
		return e.CharName
	case tn != "":
		return tn
	}
	return "Some object"
}

func combatObsEventRep(key string, entries []marshal.DictEntry) marshal.PyRep {
	return marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "OnDamageMessage"},
		marshal.PyString{Value: key},
		marshal.PyDict{Items: entries},
	}}
}
