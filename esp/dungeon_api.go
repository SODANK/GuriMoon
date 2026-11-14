package esp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/npc"
)

// ── JSON API Types ──────────────────────────────────────────────────────────

type apiDungeonSummary struct {
	DungeonVID  int64  `json:"dungeonVID"`
	DungeonName string `json:"dungeonVName"`
	FactionID   int64  `json:"factionID"`
	FactionName string `json:"factionName"`
	Version     int64  `json:"version"`
	Status      int64  `json:"status"`
	RoomCount   int    `json:"roomCount"`
}

type apiDungeon struct {
	DungeonVID  int64     `json:"dungeonVID"`
	DungeonName string    `json:"dungeonVName"`
	FactionID   int64     `json:"factionID"`
	FactionName string    `json:"factionName"`
	Version     int64     `json:"version"`
	Status      string    `json:"status"`
	Rooms       []apiRoom `json:"rooms"`
}

type apiRoom struct {
	RoomID     int64       `json:"roomID"`
	DungeonVID int64       `json:"dungeonVID"`
	RoomName   string      `json:"roomName"`
	Objects    []apiObject `json:"objects"`
}

type apiObject struct {
	ObjectID        int64   `json:"objectID"`
	RoomID          int64   `json:"roomID"`
	TypeID          int64   `json:"typeID"`
	TypeName        string  `json:"typeName"`
	X               float64 `json:"x"`
	Y               float64 `json:"y"`
	Z               float64 `json:"z"`
	Yaw             float64 `json:"yaw"`
	Pitch           float64 `json:"pitch"`
	Roll            float64 `json:"roll"`
	Radius          float64 `json:"radius"`
	AggressionRange float64 `json:"aggressionRange"`
	Stance          string  `json:"stance"`
	CategoryID      int64   `json:"categoryID"`
	GroupID         int64   `json:"groupID"`
	NPCFactionID    int64   `json:"npcFactionID"`
	NPCFactionName  string  `json:"npcFactionName"`
	MusicUrl        string  `json:"musicUrl"`
	LeashObjectID   int64   `json:"leashObjectID"`
	OrbitDistance   float64 `json:"orbitDistance"`
	Description     string  `json:"description"`
	SpawnWave       int64   `json:"spawnWave"`
	BallName        string  `json:"ballName"`
	Invulnerable    bool    `json:"invulnerable"`
	HPScale         float64 `json:"hpScale"`
	Solid           bool    `json:"solid"`         // collidable, resolved
	SolidAuthored   bool    `json:"solidAuthored"` // false = following the type default
}

type apiTypeResult struct {
	TypeID       int64   `json:"typeID"`
	TypeName     string  `json:"typeName"`
	GroupID      int64   `json:"groupID"`
	GroupName    string  `json:"groupName"`
	CategoryID   int64   `json:"categoryID"`
	CategoryName string  `json:"categoryName"`
	GraphicID    int64   `json:"graphicID"`
	Radius       float64 `json:"radius"`
	Seedable     bool    `json:"seedable"`
	Bounty       float64 `json:"bounty,omitempty"`
}

// ── Helpers ─────────────────────────────────────────────────────────────────

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func espObjToAPI(o evedb.ESPObjectRow) apiObject {
	return apiObject{
		ObjectID:        o.ObjectID,
		RoomID:          o.RoomID,
		TypeID:          o.TypeID,
		TypeName:        o.TypeName,
		X:               o.X,
		Y:               o.Y,
		Z:               o.Z,
		Yaw:             o.Yaw,
		Pitch:           o.Pitch,
		Roll:            o.Roll,
		Radius:          o.Radius,
		AggressionRange: o.AggressionRange,
		Stance:          o.Stance,
		CategoryID:      o.CategoryID,
		GroupID:         o.GroupID,
		NPCFactionID:    o.NPCFactionID,
		NPCFactionName:  o.NPCFactionName,
		MusicUrl:        o.MusicUrl,
		LeashObjectID:   o.LeashObjectID,
		OrbitDistance:   o.OrbitDistance,
		Description:     o.Description,
		SpawnWave:       o.SpawnWave,
		BallName:        o.BallName,
		Invulnerable:    o.Invulnerable,
		HPScale:         o.HPScale,
		Solid:           o.Solid,
		SolidAuthored:   o.SolidAuthored,
	}
}

// ── GET /api/dungeon-factions ────────────────────────────────────────────────

func (s *Server) handleAPIDungeonFactions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	factions, err := evedb.ESPGetDungeonFactions(ctx, s.db)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	type apiFaction struct {
		FactionID   int64  `json:"factionID"`
		FactionName string `json:"factionName"`
	}
	out := make([]apiFaction, len(factions))
	for i, f := range factions {
		out[i] = apiFaction{FactionID: f.FactionID, FactionName: f.FactionName}
	}
	jsonOK(w, out)
}

// ── GET /api/dungeons ───────────────────────────────────────────────────────

func (s *Server) handleAPIDungeonList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var factionID int64
	if q := r.URL.Query().Get("factionID"); q != "" {
		factionID, _ = strconv.ParseInt(q, 10, 64)
	}

	dungeons, err := evedb.ESPGetAllDungeons(ctx, s.db, factionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]apiDungeonSummary, len(dungeons))
	for i, d := range dungeons {
		out[i] = apiDungeonSummary{
			DungeonVID:  d.DungeonVID,
			DungeonName: d.DungeonVName,
			FactionID:   d.FactionID,
			FactionName: d.FactionName,
			Version:     d.Version,
			Status:      d.Status,
			RoomCount:   d.RoomCount,
		}
	}
	jsonOK(w, out)
}

// ── GET /api/dungeon/{vid} ──────────────────────────────────────────────────

func (s *Server) handleAPIDungeonDetail(w http.ResponseWriter, r *http.Request) {
	// Extract VID from path: /api/dungeon/123
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/dungeon/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		jsonError(w, "missing dungeon VID", http.StatusBadRequest)
		return
	}

	vid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		jsonError(w, "invalid dungeon VID", http.StatusBadRequest)
		return
	}

	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	detail, err := evedb.ESPGetDungeonDetail(ctx, s.db, vid)
	if err != nil {
		jsonError(w, err.Error(), http.StatusNotFound)
		return
	}

	out := apiDungeon{
		DungeonVID:  detail.DungeonVID,
		DungeonName: detail.DungeonVName,
		FactionID:   detail.FactionID,
		FactionName: detail.FactionName,
		Version:     detail.Version,
		Status:      fmt.Sprintf("%d", detail.Status),
		Rooms:       make([]apiRoom, len(detail.Rooms)),
	}
	for i, room := range detail.Rooms {
		apiR := apiRoom{
			RoomID:     room.RoomID,
			DungeonVID: room.DungeonVID,
			RoomName:   room.RoomName,
			Objects:    make([]apiObject, len(room.Objects)),
		}
		for j, obj := range room.Objects {
			apiR.Objects[j] = espObjToAPI(obj)
		}
		out.Rooms[i] = apiR
	}
	jsonOK(w, out)
}

// ── POST /api/dungeons ──────────────────────────────────────────────────────

func (s *Server) handleAPIDungeonCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleAPIDungeonList(w, r)
		return
	}
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Name      string `json:"name"`
		FactionID int64  `json:"factionID"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}
	if req.FactionID == 0 {
		req.FactionID = 500010 // default Serpentis
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	vid, err := evedb.CreateDungeon(ctx, s.db, req.Name, req.FactionID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]int64{"dungeonVID": vid})
}

// ── POST /api/objects ───────────────────────────────────────────────────────

func (s *Server) handleAPIObjectCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		RoomID int64   `json:"roomID"`
		TypeID int64   `json:"typeID"`
		X      float64 `json:"x"`
		Y      float64 `json:"y"`
		Z      float64 `json:"z"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.RoomID == 0 || req.TypeID == 0 {
		jsonError(w, "roomID and typeID are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	objectID, err := evedb.AddObject(ctx, s.db, req.RoomID, req.TypeID, req.X, req.Y, req.Z)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]any{
		"objectID": objectID,
		"roomID":   req.RoomID,
		"typeID":   req.TypeID,
		"x":        req.X,
		"y":        req.Y,
		"z":        req.Z,
	})
}

// ── PATCH/DELETE /api/objects/{id} and POST /api/objects/{id}/copy ───────────

func (s *Server) handleAPIObject(w http.ResponseWriter, r *http.Request) {
	// Parse object ID from path: /api/objects/123 or /api/objects/123/copy
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/objects/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 0 || parts[0] == "" {
		jsonError(w, "missing object ID", http.StatusBadRequest)
		return
	}

	objectID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		jsonError(w, "invalid object ID", http.StatusBadRequest)
		return
	}

	// /api/objects/{id}/copy
	if len(parts) > 1 && parts[1] == "copy" {
		s.handleAPIObjectCopy(w, r, objectID)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	switch r.Method {
	case http.MethodPatch:
		var req objectPatch
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if err := req.validate(); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := evedb.EditObject(ctx, s.db, objectID, req.toFields()); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"status": "ok"})

	case http.MethodDelete:
		if err := evedb.RemoveObject(ctx, s.db, objectID); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"status": "ok"})

	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// objectPatch is the editable part of a ball as the editor sends it: only the
// keys present are written. Shared by PATCH /api/objects/{id} and the batch.
type objectPatch struct {
	X               *float64 `json:"x"`
	Y               *float64 `json:"y"`
	Z               *float64 `json:"z"`
	Yaw             *float64 `json:"yaw"`
	Pitch           *float64 `json:"pitch"`
	Roll            *float64 `json:"roll"`
	Radius          *float64 `json:"radius"`
	AggressionRange *float64 `json:"aggressionRange"`
	Stance          *string  `json:"stance"`
	NPCFactionID    *int64   `json:"npcFactionID"`
	MusicUrl        *string  `json:"musicUrl"`
	LeashObjectID   *int64   `json:"leashObjectID"`
	OrbitDistance   *float64 `json:"orbitDistance"`
	Description     *string  `json:"description"`
	SpawnWave       *int64   `json:"spawnWave"`
	BallName        *string  `json:"ballName"`
	Invulnerable    *bool    `json:"invulnerable"`
	HPScale         *float64 `json:"hpScale"`
	Solid           *bool    `json:"solid"`
	RoomID          *int64   `json:"roomID"`
}

// validate refuses a stance that is not one of the three. Stored as-is it would
// read back as aggressive (npc.ParseStance) and the editor would show a value
// the server never acts on. "" clears the column (the aggressive default).
func (p objectPatch) validate() error {
	if p.Stance != nil && *p.Stance != "" && !npc.ValidStance(*p.Stance) {
		return fmt.Errorf("unknown stance %q (want aggressive, defensive or passive)", *p.Stance)
	}
	return nil
}

func (p objectPatch) toFields() evedb.EditObjectFields {
	return evedb.EditObjectFields{
		X: p.X, Y: p.Y, Z: p.Z,
		Yaw: p.Yaw, Pitch: p.Pitch, Roll: p.Roll,
		Radius:          p.Radius,
		AggressionRange: p.AggressionRange,
		Stance:          p.Stance,
		NPCFactionID:    p.NPCFactionID,
		MusicUrl:        p.MusicUrl,
		LeashObjectID:   p.LeashObjectID,
		OrbitDistance:   p.OrbitDistance,
		Description:     p.Description,
		SpawnWave:       p.SpawnWave,
		BallName:        p.BallName,
		Invulnerable:    p.Invulnerable,
		HPScale:         p.HPScale,
		Solid:           p.Solid,
		RoomID:          p.RoomID,
	}
}

// ── POST /api/objects/batch ─────────────────────────────────────────────────
// Every editor write in one transaction. See db/dunbatchdb.go for the undo
// model (snapshots out, restore at the same objectID back in).

func (s *Server) handleAPIObjectBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Restore []evedb.DunObjectSnapshot `json:"restore"`
		Insert  []struct {
			ClientKey      string                  `json:"clientKey"`
			LeashClientKey string                  `json:"leashClientKey"`
			Snapshot       evedb.DunObjectSnapshot `json:"snapshot"`
		} `json:"insert"`
		Create []struct {
			objectPatch
			ClientKey      string              `json:"clientKey"`
			TypeID         int64               `json:"typeID"`
			CopyLootFrom   int64               `json:"copyLootFrom"`
			Loot           []evedb.DunLootSpec `json:"loot"`
			LeashClientKey string              `json:"leashClientKey"`
		} `json:"create"`
		Replace []evedb.DunObjectSnapshot `json:"replace"`
		Update  []struct {
			objectPatch
			ObjectID int64 `json:"objectID"`
		} `json:"update"`
		Delete []int64 `json:"delete"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	b := evedb.ObjectBatch{Restore: req.Restore, Replace: req.Replace, Delete: req.Delete}
	for _, in := range req.Insert {
		b.Insert = append(b.Insert, evedb.ObjectInsert{
			ClientKey: in.ClientKey, LeashClientKey: in.LeashClientKey, Snapshot: in.Snapshot,
		})
	}
	for _, c := range req.Create {
		if err := c.validate(); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		fields := c.toFields()
		var roomID int64
		var x, y, z float64
		if c.RoomID != nil {
			roomID = *c.RoomID
		}
		if c.X != nil {
			x = *c.X
		}
		if c.Y != nil {
			y = *c.Y
		}
		if c.Z != nil {
			z = *c.Z
		}
		// Position and room go in through AddObject, not as an edit.
		fields.X, fields.Y, fields.Z, fields.RoomID = nil, nil, nil, nil
		b.Create = append(b.Create, evedb.ObjectCreate{
			ClientKey: c.ClientKey, RoomID: roomID, TypeID: c.TypeID,
			X: x, Y: y, Z: z, Fields: fields,
			CopyLootFrom: c.CopyLootFrom, Loot: c.Loot, LeashClientKey: c.LeashClientKey,
		})
	}
	for _, u := range req.Update {
		if err := u.validate(); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		b.Update = append(b.Update, evedb.ObjectUpdate{ObjectID: u.ObjectID, Fields: u.toFields()})
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	res, err := evedb.ApplyObjectBatch(ctx, s.db, b)
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, evedb.ErrObjectBatchConflict) {
			code = http.StatusConflict
		}
		jsonError(w, err.Error(), code)
		return
	}
	objects := make([]apiObject, len(res.Objects))
	for i, o := range res.Objects {
		objects[i] = espObjToAPI(o)
	}
	jsonOK(w, map[string]any{
		"created":          nonNil(res.Created),
		"createdSnapshots": nonNil(res.CreatedSnapshots),
		"updatedBefore":    nonNil(res.UpdatedBefore),
		"deleted":          nonNil(res.Deleted),
		"objects":          objects,
	})
}

// ── POST /api/objects/snapshot ──────────────────────────────────────────────
// Exact stored rows (NULLs and loot kept) for the clipboard and prefabs.

func (s *Server) handleAPIObjectSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ObjectIDs []int64 `json:"objectIDs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	snaps, err := evedb.SnapshotObjects(ctx, s.db, req.ObjectIDs)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, nonNil(snaps))
}

// nonNil keeps an empty result a JSON [] rather than null.
func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// ── GET /api/stances, GET /api/npc-factions ─────────────────────────────────

func (s *Server) handleAPIStances(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, npc.Stances())
}

func (s *Server) handleAPINPCFactions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	factions, err := evedb.ESPGetAllFactions(ctx, s.db)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type apiFaction struct {
		FactionID   int64  `json:"factionID"`
		FactionName string `json:"factionName"`
	}
	out := make([]apiFaction, len(factions))
	for i, f := range factions {
		out[i] = apiFaction{FactionID: f.FactionID, FactionName: f.FactionName}
	}
	jsonOK(w, out)
}

// ── POST /api/objects/{id}/copy ─────────────────────────────────────────────

func (s *Server) handleAPIObjectCopy(w http.ResponseWriter, r *http.Request, objectID int64) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		OffsetX float64 `json:"offsetX"`
		OffsetY float64 `json:"offsetY"`
		OffsetZ float64 `json:"offsetZ"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	newID, err := evedb.CopyObject(ctx, s.db, objectID, req.OffsetX, req.OffsetY, req.OffsetZ)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]int64{"objectID": newID})
}

// ── GET /api/types/search ───────────────────────────────────────────────────

func (s *Server) handleAPITypeSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := r.URL.Query().Get("q")
	if q == "" {
		jsonOK(w, []apiTypeResult{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// ?scope= widens the search past the editor's category exclusions: "market" for the NPC
	// supply/demand tool, "all" for contraband. Absent or unknown keeps the space-only list.
	rows, err := evedb.ESPSearchTypes(ctx, s.db, q, r.URL.Query().Get("scope"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]apiTypeResult, len(rows))
	for i, r := range rows {
		out[i] = apiTypeResult{
			TypeID:       r.TypeID,
			TypeName:     r.TypeName,
			GroupID:      r.GroupID,
			GroupName:    r.GroupName,
			CategoryID:   r.CategoryID,
			CategoryName: r.CategoryName,
			Radius:       r.Radius,
			Seedable:     r.Seedable,
			Bounty:       r.Bounty,
		}
	}
	jsonOK(w, out)
}

// ── POST /api/rooms ─────────────────────────────────────────────────────────

func (s *Server) handleAPIRoomCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DungeonVID int64  `json:"dungeonVID"`
		RoomName   string `json:"roomName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.DungeonVID == 0 {
		jsonError(w, "dungeonVID is required", http.StatusBadRequest)
		return
	}
	if req.RoomName == "" {
		req.RoomName = "New Room"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	roomID, err := evedb.CreateRoom(ctx, s.db, req.DungeonVID, req.RoomName)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := evedb.SeedRoomBeacon(ctx, s.db, roomID); err != nil {
		log.Printf("esp: SeedRoomBeacon room %d: %v", roomID, err)
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]int64{"roomID": roomID})
}

// ── POST /api/rooms/reorder ─────────────────────────────────────────────────

func (s *Server) handleAPIRoomReorder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		DungeonVID int64   `json:"dungeonVID"`
		RoomIDs    []int64 `json:"roomIDs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.DungeonVID == 0 || len(req.RoomIDs) == 0 {
		jsonError(w, "dungeonVID and roomIDs are required", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := evedb.ReorderRooms(ctx, s.db, req.DungeonVID, req.RoomIDs); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// ── DELETE /api/rooms/{id} ──────────────────────────────────────────────────

func (s *Server) handleAPIRoom(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/rooms/"), "/")
	roomID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		jsonError(w, "invalid room ID", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if len(parts) > 1 && parts[1] == "duplicate" {
		s.handleAPIRoomDuplicate(w, r, roomID)
		return
	}

	if r.Method == http.MethodPatch {
		var req struct {
			RoomName string `json:"roomName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.RoomName) == "" {
			jsonError(w, "roomName is required", http.StatusBadRequest)
			return
		}
		if err := evedb.RenameRoom(ctx, s.db, roomID, strings.TrimSpace(req.RoomName)); err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}
	if r.Method != http.MethodDelete {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := evedb.DeleteRoom(ctx, s.db, roomID); err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// ── GET /api/model/{typeID}.glb ─────────────────────────────────────────────
// Resolves typeID → url3D → finds .glb file in models directory and serves it.

func (s *Server) handleAPIModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Parse typeID from path: /api/model/12345.glb
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/model/")
	trimmed = strings.TrimSuffix(trimmed, ".glb")
	typeID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	url3D, err := evedb.ESPGetModelPath(ctx, s.db, typeID)
	if err != nil {
		log.Printf("[model] typeID %d: DB lookup failed: %v", typeID, err)
		http.NotFound(w, r)
		return
	}

	// url3D looks like "res:/Model/Ship/Amarr/af2/af2.blue". The model is composed from the
	// client's own files (modelbuild.go), falling back to the converter tree.
	s.serveModel(w, r, url3D)
}

// ── GET /api/nebulas ────────────────────────────────────────────────────────
// Returns available nebula skyboxes: pre-converted PNG directories plus
// client DDS cubemaps decoded on demand (see esp/nebula.go).

func (s *Server) handleAPINebulas(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	type nebulaEntry struct {
		Name string `json:"name"`
		Path string `json:"path"`
	}

	names := s.nebulaNames()
	nebulas := make([]nebulaEntry, 0, len(names))
	for _, name := range names {
		nebulas = append(nebulas, nebulaEntry{
			Name: name,
			Path: "/nebulas/" + name,
		})
	}
	jsonOK(w, nebulas)
}

// ── POST /api/rooms/{id}/duplicate ──────────────────────────────────────────

func (s *Server) handleAPIRoomDuplicate(w http.ResponseWriter, r *http.Request, roomID int64) {
	if r.Method != http.MethodPost {
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	newRoomID, err := evedb.DuplicateRoom(ctx, s.db, roomID)
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
	jsonOK(w, map[string]int64{"roomID": newRoomID})
}

// ── /api/prefabs ────────────────────────────────────────────────────────────
// GET lists, POST {name, category, objectIDs} captures, and /api/prefabs/{id}
// GETs one with its balls or DELETEs it. See db/dunprefabdb.go.

func (s *Server) handleAPIPrefabs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	idPart := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/prefabs"), "/")
	if idPart != "" {
		prefabID, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			jsonError(w, "invalid prefab ID", http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodGet:
			p, err := evedb.GetPrefab(ctx, s.db, prefabID)
			if err != nil {
				jsonError(w, err.Error(), http.StatusNotFound)
				return
			}
			jsonOK(w, p)
		case http.MethodDelete:
			if err := evedb.DeletePrefab(ctx, s.db, prefabID); err != nil {
				jsonError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			jsonOK(w, map[string]string{"status": "ok"})
		default:
			jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		list, err := evedb.ListPrefabs(ctx, s.db)
		if err != nil {
			jsonError(w, err.Error(), http.StatusInternalServerError)
			return
		}
		jsonOK(w, nonNil(list))
	case http.MethodPost:
		var req struct {
			Name      string  `json:"name"`
			Category  string  `json:"category"`
			ObjectIDs []int64 `json:"objectIDs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonError(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		id, err := evedb.CreatePrefabFromObjects(ctx, s.db, req.Name, req.Category, req.ObjectIDs)
		if err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, evedb.ErrObjectBatchConflict) {
				code = http.StatusBadRequest
			}
			jsonError(w, err.Error(), code)
			return
		}
		w.WriteHeader(http.StatusCreated)
		jsonOK(w, map[string]int64{"prefabID": id})
	default:
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
