package npc

import (
	"context"
	"database/sql"
	"fmt"
)

// Write-behind for the live NPC tables (npcLiveEntities, npcDungeonEntities).
//
// HP used to be written on every hit and position every 15 s per moving NPC, each
// as its own goroutine and autocommit UPDATE. A fight produced hundreds of commits a
// second, and on a stock MariaDB every commit is a redo fsync plus page flushes.
// Nothing reads these columns back during play (both tables are truncated at boot
// and the info window reads only type and name), so the rows only need to be
// roughly current. Callers now mark an NPC dirty and FlushState writes every dirty
// row in one transaction.

const (
	dirtyHP uint8 = 1 << iota
	dirtyPos
)

// markDirty records that an NPC's HP and/or position changed since the last flush.
func (m *NPCManager) markDirty(entityID uint32, bits uint8) {
	if m.sqldb == nil {
		return
	}
	m.dirtyMu.Lock()
	if m.dirty == nil {
		m.dirty = make(map[uint32]uint8)
	}
	m.dirty[entityID] |= bits
	m.dirtyMu.Unlock()
}

// forgetDirty drops an NPC that is gone, so the flush does not write a row that
// is being deleted.
func (m *NPCManager) forgetDirty(entityID uint32) {
	m.dirtyMu.Lock()
	delete(m.dirty, entityID)
	m.dirtyMu.Unlock()
}

// DirtyCount is the number of NPCs waiting for the next flush.
func (m *NPCManager) DirtyCount() int {
	m.dirtyMu.Lock()
	defer m.dirtyMu.Unlock()
	return len(m.dirty)
}

// npcStateRow is one NPC's state as the flush writes it.
type npcStateRow struct {
	id                uint32
	bits              uint8
	dungeon           bool
	shield, armor, st float64
	x, y, z           float64
}

// takeDirty swaps out the dirty set and reads the current state of each NPC still
// alive. Dead and removed NPCs are skipped.
func (m *NPCManager) takeDirty() []npcStateRow {
	m.dirtyMu.Lock()
	dirty := m.dirty
	m.dirty = nil
	m.dirtyMu.Unlock()
	if len(dirty) == 0 {
		return nil
	}
	out := make([]npcStateRow, 0, len(dirty))
	for id, bits := range dirty {
		m.mu.RLock()
		rec := m.npcs[id]
		m.mu.RUnlock()
		if rec == nil {
			continue
		}
		rec.mu.Lock()
		dead, dunObj, ent := rec.Dead, rec.DunObjectID, rec.Entity
		rec.mu.Unlock()
		if dead || ent == nil {
			continue
		}
		r := npcStateRow{id: id, bits: bits, dungeon: dunObj != 0}
		if bits&dirtyHP != 0 {
			d := ent.DamageStateFrac()
			r.shield, r.armor, r.st = d.Shield, d.Armor, d.Structure
		}
		if bits&dirtyPos != 0 {
			p := ent.Pos()
			r.x, r.y, r.z = p.X, p.Y, p.Z
		}
		out = append(out, r)
	}
	return out
}

// FlushState writes every dirty NPC's HP and position in one transaction and
// returns how many NPCs it wrote. On error the rows are lost, not retried: the
// next hit or move marks the NPC again.
func (m *NPCManager) FlushState(ctx context.Context) (int, error) {
	if m.sqldb == nil {
		return 0, nil
	}
	rows := m.takeDirty()
	if len(rows) == 0 {
		return 0, nil
	}
	tx, err := m.sqldb.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("npc state flush begin: %w", err)
	}
	defer tx.Rollback()

	stmts := map[string]*sql.Stmt{}
	prep := func(q string) (*sql.Stmt, error) {
		if s, ok := stmts[q]; ok {
			return s, nil
		}
		s, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return nil, err
		}
		stmts[q] = s
		return s, nil
	}
	const (
		liveHP    = `UPDATE npcLiveEntities SET shieldFrac=?, armorFrac=?, structureFrac=? WHERE entityID=?`
		livePos   = `UPDATE npcLiveEntities SET posX=?, posY=?, posZ=? WHERE entityID=?`
		liveBoth  = `UPDATE npcLiveEntities SET shieldFrac=?, armorFrac=?, structureFrac=?, posX=?, posY=?, posZ=? WHERE entityID=?`
		dungeonHP = `UPDATE npcDungeonEntities SET shieldFrac=?, armorFrac=?, structureFrac=? WHERE entityID=?`
	)
	for _, r := range rows {
		var q string
		var args []any
		switch {
		case r.dungeon:
			// npcDungeonEntities keeps the spawn position; only HP is live.
			if r.bits&dirtyHP == 0 {
				continue
			}
			q, args = dungeonHP, []any{r.shield, r.armor, r.st, r.id}
		case r.bits == dirtyHP|dirtyPos:
			q, args = liveBoth, []any{r.shield, r.armor, r.st, r.x, r.y, r.z, r.id}
		case r.bits&dirtyHP != 0:
			q, args = liveHP, []any{r.shield, r.armor, r.st, r.id}
		default:
			q, args = livePos, []any{r.x, r.y, r.z, r.id}
		}
		s, err := prep(q)
		if err != nil {
			return 0, fmt.Errorf("npc state flush prepare: %w", err)
		}
		if _, err := s.ExecContext(ctx, args...); err != nil {
			return 0, fmt.Errorf("npc state flush entity %d: %w", r.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("npc state flush commit: %w", err)
	}
	return len(rows), nil
}
