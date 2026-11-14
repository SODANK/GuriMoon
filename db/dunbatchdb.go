package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Dungeon editor batch writes. The 3D editor sends every change (a gizmo drag, a paste, a
// pattern, an undo) as one batch in one transaction. Undo works on snapshots: a delete answers
// the full rows it removed (NULLs and loot kept) and a restore re-inserts them at the same
// objectID, so leash references and dunTriggers.targetObjectID come back alive. InnoDB never
// reuses an auto-increment value, so the ID stays free.

// DunLootSpec is one authored drop carried inside a snapshot or a create.
type DunLootSpec struct {
	ItemTypeID int64   `json:"itemTypeID"`
	MinQty     int64   `json:"minQty"`
	MaxQty     int64   `json:"maxQty"`
	DropChance float64 `json:"dropChance"`
}

// DunObjectSnapshot is a dunObjects row exactly as stored: nullable columns stay
// nil, so writing it back reproduces the row rather than a flattened copy.
type DunObjectSnapshot struct {
	ObjectID        int64         `json:"objectID"`
	RoomID          int64         `json:"roomID"`
	TypeID          int64         `json:"typeID"`
	X               float64       `json:"x"`
	Y               float64       `json:"y"`
	Z               float64       `json:"z"`
	Yaw             *float64      `json:"yaw"`
	Pitch           *float64      `json:"pitch"`
	Roll            *float64      `json:"roll"`
	Radius          *float64      `json:"radius"`
	AggressionRange *float64      `json:"aggressionRange"`
	Stance          *string       `json:"stance"`
	OrbitDistance   *float64      `json:"orbitDistance"`
	FollowRange     *float64      `json:"followRange"`
	LeashRange      *float64      `json:"leashRange"`
	NPCFactionID    *int64        `json:"npcFactionID"`
	MusicUrl        *string       `json:"musicUrl"`
	LeashObjectID   *int64        `json:"leashObjectID"`
	Description     *string       `json:"description"`
	SpawnWave       *int64        `json:"spawnWave"`
	BallName        *string       `json:"ballName"`
	Invulnerable    bool          `json:"invulnerable"`
	HPScale         *float64      `json:"hpScale"`
	Solid           *bool         `json:"solid,omitempty"` // nil = type default (and pre-column prefabs/undo)
	LootMinDrops    *int64        `json:"lootMinDrops"`
	LootMaxDrops    *int64        `json:"lootMaxDrops"`
	Loot            []DunLootSpec `json:"loot"`
}

// dunObjectColumns is every authored dunObjects column after objectID, in the
// order DunObjectSnapshot scans and writes them.
const dunObjectColumns = `roomID, typeID, x, y, z, yaw, pitch, roll, radius, aggressionRange,
	stance, orbitDistance, followRange, leashRange, npcFactionID, musicUrl,
	leashObjectID, description, spawnWave, ballName, invulnerable, hpScale,
	solid, lootMinDrops, lootMaxDrops`

func (s *DunObjectSnapshot) columnArgs() []any {
	return []any{s.RoomID, s.TypeID, s.X, s.Y, s.Z, s.Yaw, s.Pitch, s.Roll, s.Radius,
		s.AggressionRange, storedStancePtr(s.Stance), s.OrbitDistance, s.FollowRange, s.LeashRange,
		s.NPCFactionID, s.MusicUrl, s.LeashObjectID, s.Description, s.SpawnWave,
		s.BallName, s.Invulnerable, s.HPScale, s.Solid, s.LootMinDrops, s.LootMaxDrops}
}

// storedStancePtr normalises a snapshot's stance for the column (StoredStance):
// the aggressive default goes back as NULL.
func storedStancePtr(p *string) any {
	if p == nil {
		return nil
	}
	if st := StoredStance(*p); st != "" {
		return st
	}
	return nil
}

// SnapshotObjects returns the stored rows (with loot) for the given IDs, in the
// order the IDs were given. Missing IDs are skipped.
func SnapshotObjects(ctx context.Context, q DBTX, ids []int64) ([]DunObjectSnapshot, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	ph, args := int64Placeholders(ids)
	rows, err := q.QueryContext(ctx,
		`SELECT objectID, `+dunObjectColumns+` FROM dunObjects WHERE objectID IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("SnapshotObjects: %w", err)
	}
	byID := make(map[int64]*DunObjectSnapshot, len(ids))
	for rows.Next() {
		var s DunObjectSnapshot
		var yaw, pitch, roll, radius, aggr, orbit, follow, leashR, hpScale sql.NullFloat64
		var stance, music, desc, ballName sql.NullString
		var faction, leashObj, wave, lootMin, lootMax sql.NullInt64
		var invuln int64
		var solid sql.NullBool
		if err := rows.Scan(&s.ObjectID, &s.RoomID, &s.TypeID, &s.X, &s.Y, &s.Z,
			&yaw, &pitch, &roll, &radius, &aggr, &stance, &orbit, &follow, &leashR,
			&faction, &music, &leashObj, &desc, &wave, &ballName, &invuln, &hpScale,
			&solid, &lootMin, &lootMax); err != nil {
			rows.Close()
			return nil, fmt.Errorf("SnapshotObjects scan: %w", err)
		}
		s.Yaw, s.Pitch, s.Roll = nullFloatPtr(yaw), nullFloatPtr(pitch), nullFloatPtr(roll)
		s.Radius, s.AggressionRange = nullFloatPtr(radius), nullFloatPtr(aggr)
		s.OrbitDistance, s.FollowRange, s.LeashRange = nullFloatPtr(orbit), nullFloatPtr(follow), nullFloatPtr(leashR)
		s.Stance, s.MusicUrl = nullStringPtr(stance), nullStringPtr(music)
		s.Description, s.BallName = nullStringPtr(desc), nullStringPtr(ballName)
		s.NPCFactionID, s.LeashObjectID, s.SpawnWave = nullIntPtr(faction), nullIntPtr(leashObj), nullIntPtr(wave)
		s.Invulnerable = invuln != 0
		s.HPScale = nullFloatPtr(hpScale)
		if solid.Valid {
			s.Solid = &solid.Bool
		}
		s.LootMinDrops, s.LootMaxDrops = nullIntPtr(lootMin), nullIntPtr(lootMax)
		byID[s.ObjectID] = &s
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("SnapshotObjects: %w", err)
	}

	lootRows, err := q.QueryContext(ctx,
		`SELECT objectID, itemTypeID, minQty, maxQty, dropChance FROM dunObjectLoot
		 WHERE objectID IN (`+ph+`) ORDER BY lootID`, args...)
	if err != nil {
		return nil, fmt.Errorf("SnapshotObjects loot: %w", err)
	}
	for lootRows.Next() {
		var objectID int64
		var l DunLootSpec
		if err := lootRows.Scan(&objectID, &l.ItemTypeID, &l.MinQty, &l.MaxQty, &l.DropChance); err != nil {
			lootRows.Close()
			return nil, fmt.Errorf("SnapshotObjects loot scan: %w", err)
		}
		if s := byID[objectID]; s != nil {
			s.Loot = append(s.Loot, l)
		}
	}
	if err := lootRows.Close(); err != nil {
		return nil, fmt.Errorf("SnapshotObjects loot: %w", err)
	}

	out := make([]DunObjectSnapshot, 0, len(byID))
	for _, id := range ids {
		if s := byID[id]; s != nil {
			out = append(out, *s)
		}
	}
	return out, nil
}

func nullFloatPtr(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.Float64
}

func nullStringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func nullIntPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// InsertObjectSnapshot writes a snapshot as a new row with its loot. With
// keepID the row is inserted at s.ObjectID (a restore); otherwise it gets a
// fresh ID. Returns the row's objectID.
func InsertObjectSnapshot(ctx context.Context, tx DBTX, s DunObjectSnapshot, keepID bool) (int64, error) {
	// Placeholders are built from the argument list so a new column cannot leave VALUES one short.
	args := s.columnArgs()
	cols, ph := dunObjectColumns, strings.TrimSuffix(strings.Repeat("?,", len(args)), ",")
	if keepID {
		cols, ph, args = "objectID, "+cols, "?,"+ph, append([]any{s.ObjectID}, args...)
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO dunObjects (`+cols+`) VALUES (`+ph+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("InsertObjectSnapshot %d: %w", s.ObjectID, err)
	}
	id := s.ObjectID
	if !keepID {
		if id, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	}
	for _, l := range s.Loot {
		if err := SaveObjectLoot(ctx, tx, id, l.ItemTypeID, l.MinQty, l.MaxQty, l.DropChance); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// replaceObjectRow overwrites every authored column of an existing row with a
// snapshot. Loot is left alone: no editor edit touches it, so the undo of an
// edit has no reason to.
func replaceObjectRow(ctx context.Context, tx DBTX, s DunObjectSnapshot) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE dunObjects SET roomID=?, typeID=?, x=?, y=?, z=?, yaw=?, pitch=?, roll=?,
		       radius=?, aggressionRange=?, stance=?, orbitDistance=?, followRange=?,
		       leashRange=?, npcFactionID=?, musicUrl=?, leashObjectID=?, description=?,
		       spawnWave=?, ballName=?, invulnerable=?, hpScale=?,
		       solid=?, lootMinDrops=?, lootMaxDrops=?
		 WHERE objectID=?`, append(s.columnArgs(), s.ObjectID)...)
	if err != nil {
		return fmt.Errorf("replace object %d: %w", s.ObjectID, err)
	}
	return nil
}

// ObjectCreate is one new ball in a batch.
type ObjectCreate struct {
	ClientKey string // echoed back so the editor can match IDs to its pending rows
	RoomID    int64
	TypeID    int64
	X, Y, Z   float64
	Fields    EditObjectFields
	// CopyLootFrom copies an existing ball's drops (paste, duplicate, pattern).
	CopyLootFrom int64
	// Loot is written as given (prefab stamp).
	Loot []DunLootSpec
	// LeashClientKey leashes this ball to another create in the same batch.
	LeashClientKey string
}

// ObjectInsert is a new ball copied from a snapshot (paste, duplicate, pattern, prefab stamp):
// every column and drop comes from the snapshot and the ID is fresh. The editor has already moved
// it to its new room and position and chosen its leash target.
type ObjectInsert struct {
	ClientKey      string
	LeashClientKey string // leash to another insert/create in the same batch
	Snapshot       DunObjectSnapshot
}

// ObjectUpdate is a partial edit of one existing ball.
type ObjectUpdate struct {
	ObjectID int64
	Fields   EditObjectFields
}

// ObjectBatch is everything one editor action writes. Applied in the order
// restore, insert, create, replace, update, delete.
type ObjectBatch struct {
	Restore []DunObjectSnapshot // re-insert at their own objectID (undo of a delete)
	Insert  []ObjectInsert
	Create  []ObjectCreate
	Replace []DunObjectSnapshot // overwrite whole rows (undo of an edit)
	Update  []ObjectUpdate
	Delete  []int64
}

// CreatedObject pairs a create's client key with the ID it was given.
type CreatedObject struct {
	ClientKey string `json:"clientKey"`
	ObjectID  int64  `json:"objectID"`
}

// ObjectBatchResult carries what the editor needs to show the change and to
// build its exact inverse.
type ObjectBatchResult struct {
	Created          []CreatedObject
	CreatedSnapshots []DunObjectSnapshot // redo of a create restores these
	UpdatedBefore    []DunObjectSnapshot // undo of an update/replace replaces with these
	Deleted          []DunObjectSnapshot // undo of a delete restores these
	Objects          []ESPObjectRow      // every row that exists after the batch and was touched
}

// ErrObjectBatchConflict marks a batch refused because of the dungeon's own
// wiring rather than a database failure.
var ErrObjectBatchConflict = errors.New("dungeon editor conflict")

// ApplyObjectBatch applies a batch in one transaction.
func ApplyObjectBatch(ctx context.Context, db *sql.DB, b ObjectBatch) (*ObjectBatchResult, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ApplyObjectBatch begin: %w", err)
	}
	defer tx.Rollback()

	res := &ObjectBatchResult{}
	var touched []int64

	for _, s := range b.Restore {
		id, err := InsertObjectSnapshot(ctx, tx, s, true)
		if err != nil {
			return nil, err
		}
		touched = append(touched, id)
	}

	keyToID := make(map[string]int64, len(b.Insert)+len(b.Create))
	var createdIDs []int64
	var leashKeys []string // parallel to createdIDs
	for _, in := range b.Insert {
		id, err := InsertObjectSnapshot(ctx, tx, in.Snapshot, false)
		if err != nil {
			return nil, err
		}
		if in.ClientKey != "" {
			keyToID[in.ClientKey] = id
		}
		res.Created = append(res.Created, CreatedObject{ClientKey: in.ClientKey, ObjectID: id})
		createdIDs = append(createdIDs, id)
		leashKeys = append(leashKeys, in.LeashClientKey)
	}
	for _, c := range b.Create {
		if c.RoomID == 0 || c.TypeID == 0 {
			return nil, fmt.Errorf("%w: create needs roomID and typeID", ErrObjectBatchConflict)
		}
		id, err := AddObject(ctx, tx, c.RoomID, c.TypeID, c.X, c.Y, c.Z)
		if err != nil {
			return nil, err
		}
		if err := EditObject(ctx, tx, id, c.Fields); err != nil {
			return nil, fmt.Errorf("create %s fields: %w", c.ClientKey, err)
		}
		if c.CopyLootFrom != 0 {
			if err := CopyObjectLoot(ctx, tx, c.CopyLootFrom, id); err != nil {
				return nil, err
			}
		}
		for _, l := range c.Loot {
			if err := SaveObjectLoot(ctx, tx, id, l.ItemTypeID, l.MinQty, l.MaxQty, l.DropChance); err != nil {
				return nil, err
			}
		}
		if c.ClientKey != "" {
			keyToID[c.ClientKey] = id
		}
		res.Created = append(res.Created, CreatedObject{ClientKey: c.ClientKey, ObjectID: id})
		createdIDs = append(createdIDs, id)
		leashKeys = append(leashKeys, c.LeashClientKey)
	}
	// Leashes inside the batch need every new ball's ID, so they go second.
	for i, key := range leashKeys {
		if key == "" {
			continue
		}
		target, ok := keyToID[key]
		if !ok {
			continue
		}
		if err := EditObject(ctx, tx, createdIDs[i], EditObjectFields{LeashObjectID: &target}); err != nil {
			return nil, fmt.Errorf("object %d leash: %w", createdIDs[i], err)
		}
	}
	touched = append(touched, createdIDs...)

	var editIDs []int64
	for _, s := range b.Replace {
		editIDs = append(editIDs, s.ObjectID)
	}
	for _, u := range b.Update {
		editIDs = append(editIDs, u.ObjectID)
	}
	if res.UpdatedBefore, err = SnapshotObjects(ctx, tx, editIDs); err != nil {
		return nil, err
	}
	for _, s := range b.Replace {
		if err := checkRoomMove(ctx, tx, s.ObjectID, s.RoomID); err != nil {
			return nil, err
		}
		if err := replaceObjectRow(ctx, tx, s); err != nil {
			return nil, err
		}
	}
	for _, u := range b.Update {
		if u.Fields.RoomID != nil {
			if err := checkRoomMove(ctx, tx, u.ObjectID, *u.Fields.RoomID); err != nil {
				return nil, err
			}
		}
		if err := EditObject(ctx, tx, u.ObjectID, u.Fields); err != nil {
			return nil, fmt.Errorf("update object %d: %w", u.ObjectID, err)
		}
	}
	touched = append(touched, editIDs...)

	if res.Deleted, err = SnapshotObjects(ctx, tx, b.Delete); err != nil {
		return nil, err
	}
	for _, id := range b.Delete {
		if err := RemoveObject(ctx, tx, id); err != nil {
			return nil, fmt.Errorf("delete object %d: %w", id, err)
		}
	}

	if res.CreatedSnapshots, err = SnapshotObjects(ctx, tx, createdIDs); err != nil {
		return nil, err
	}
	deleted := make(map[int64]bool, len(b.Delete))
	for _, id := range b.Delete {
		deleted[id] = true
	}
	var live []int64
	for _, id := range touched {
		if !deleted[id] {
			live = append(live, id)
		}
	}
	if res.Objects, err = ESPGetObjectsByID(ctx, tx, live); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ApplyObjectBatch commit: %w", err)
	}
	return res, nil
}

// checkRoomMove refuses moving a ball to a room of another dungeon, or out of a room whose
// triggers name it (the trigger would point at a ball that no longer spawns there).
func checkRoomMove(ctx context.Context, tx DBTX, objectID, newRoomID int64) error {
	var curRoom, curDungeon int64
	err := tx.QueryRowContext(ctx, `
		SELECT o.roomID, r.dungeonVID FROM dunObjects o JOIN dunRooms r ON r.roomID = o.roomID
		 WHERE o.objectID = ?`, objectID).Scan(&curRoom, &curDungeon)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: object %d does not exist", ErrObjectBatchConflict, objectID)
	}
	if err != nil {
		return fmt.Errorf("room move check %d: %w", objectID, err)
	}
	if curRoom == newRoomID {
		return nil
	}
	var newDungeon int64
	if err := tx.QueryRowContext(ctx, `SELECT dungeonVID FROM dunRooms WHERE roomID = ?`, newRoomID).
		Scan(&newDungeon); err != nil || newDungeon != curDungeon {
		return fmt.Errorf("%w: room %d is not in this dungeon", ErrObjectBatchConflict, newRoomID)
	}
	var triggerID int64
	err = tx.QueryRowContext(ctx, `
		SELECT triggerID FROM dunTriggers WHERE roomID = ? AND targetObjectID = ? LIMIT 1`,
		curRoom, objectID).Scan(&triggerID)
	if err == nil {
		return fmt.Errorf("%w: object %d is the target of trigger %d in its room — retarget that trigger first",
			ErrObjectBatchConflict, objectID, triggerID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("room move trigger check %d: %w", objectID, err)
	}
	return nil
}

// DuplicateRoom copies a room into the same dungeon (every ball with its loot, every trigger)
// in one transaction. Leash references and trigger targets inside the room are rewired to the
// copies; a leash to a ball outside the room is kept. Returns the new roomID.
func DuplicateRoom(ctx context.Context, db *sql.DB, roomID int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("DuplicateRoom begin: %w", err)
	}
	defer tx.Rollback()

	var dungeonVID int64
	var name string
	var phenomena int64
	if err := tx.QueryRowContext(ctx,
		`SELECT dungeonVID, roomName, COALESCE(naturalPhenomena, 1) FROM dunRooms WHERE roomID = ?`, roomID).
		Scan(&dungeonVID, &name, &phenomena); err != nil {
		return 0, fmt.Errorf("DuplicateRoom %d: %w", roomID, err)
	}
	copyName := name + " (copy)"
	if len(copyName) > 255 {
		copyName = copyName[:255]
	}
	rs, err := tx.ExecContext(ctx,
		`INSERT INTO dunRooms (dungeonVID, roomName, naturalPhenomena) VALUES (?, ?, ?)`,
		dungeonVID, copyName, phenomena)
	if err != nil {
		return 0, fmt.Errorf("DuplicateRoom insert room: %w", err)
	}
	newRoomID, err := rs.LastInsertId()
	if err != nil {
		return 0, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT objectID FROM dunObjects WHERE roomID = ? ORDER BY objectID`, roomID)
	if err != nil {
		return 0, fmt.Errorf("DuplicateRoom objects: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	snaps, err := SnapshotObjects(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	idMap := make(map[int64]int64, len(snaps))
	for _, s := range snaps {
		old := s.ObjectID
		s.RoomID = newRoomID
		newID, err := InsertObjectSnapshot(ctx, tx, s, false)
		if err != nil {
			return 0, err
		}
		idMap[old] = newID
	}
	for _, s := range snaps {
		if s.LeashObjectID == nil {
			continue
		}
		if target, ok := idMap[*s.LeashObjectID]; ok {
			if err := EditObject(ctx, tx, idMap[s.ObjectID], EditObjectFields{LeashObjectID: &target}); err != nil {
				return 0, err
			}
		}
	}

	trows, err := tx.QueryContext(ctx, `
		SELECT event, targetObjectID, action, header, icon, body, senderName,
		       deferUntilLanded, onceOnly, delayMs, spawnWave, chatColor, itemTypeID, itemQty
		  FROM dunTriggers WHERE roomID = ? ORDER BY triggerID`, roomID)
	if err != nil {
		return 0, fmt.Errorf("DuplicateRoom triggers: %w", err)
	}
	type trig struct {
		event, action                         string
		target, wave, itemType                sql.NullInt64
		header, icon, body, sender, chatColor sql.NullString
		deferLanded, once, delay, itemQty     int64
	}
	var trigs []trig
	for trows.Next() {
		var t trig
		if err := trows.Scan(&t.event, &t.target, &t.action, &t.header, &t.icon, &t.body, &t.sender,
			&t.deferLanded, &t.once, &t.delay, &t.wave, &t.chatColor, &t.itemType, &t.itemQty); err != nil {
			trows.Close()
			return 0, err
		}
		trigs = append(trigs, t)
	}
	if err := trows.Close(); err != nil {
		return 0, err
	}
	for _, t := range trigs {
		if t.target.Valid {
			if newID, ok := idMap[t.target.Int64]; ok {
				t.target.Int64 = newID
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO dunTriggers (roomID, event, targetObjectID, action, header, icon, body,
			                         senderName, deferUntilLanded, onceOnly, delayMs, spawnWave, chatColor,
			                         itemTypeID, itemQty)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newRoomID, t.event, t.target, t.action, t.header, t.icon, t.body, t.sender,
			t.deferLanded, t.once, t.delay, t.wave, t.chatColor, t.itemType, t.itemQty); err != nil {
			return 0, fmt.Errorf("DuplicateRoom insert trigger: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("DuplicateRoom commit: %w", err)
	}
	return newRoomID, nil
}
