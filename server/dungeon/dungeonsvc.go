package dungeon

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "dungeon" service.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch cs.Method {
	case "DEGetFactions":
		return handleDEGetFactions(ctx, c, addr, pkt, accountID, sqldb)
	case "DEGetDungeons":
		return handleDEGetDungeons(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DEGetRooms":
		return handleDEGetRooms(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DEGetObjects":
		return handleDEGetObjects(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "EditObject":
		return handleEditObject(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "CopyObject":
		return handleCopyObject(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "RemoveObject":
		return handleRemoveObject(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DECreateDungeon", "CreateDungeon":
		return handleCreateDungeon(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DECreateRoom", "CreateRoom":
		return handleCreateRoom(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DEAddObject", "AddObject":
		return handleAddObject(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DEGetCategories":
		return handleDEGetCategories2(ctx, c, addr, pkt, accountID, sqldb)
	case "DEGetGroups":
		return handleDEGetGroups(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DEGetTypes":
		return handleDEGetTypes(ctx, c, addr, cs, pkt, accountID, sqldb)
	case "DESearchTypes":
		return handleDESearchTypes(ctx, c, addr, cs, pkt, accountID, sqldb)
	default:
		server.RecordUnhandled(addr, "dungeon", cs, true)
		log.Printf("[%s] dungeon: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

func handleDEGetFactions(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetDungeonFactions(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] dungeon.DEGetFactions: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DEGetFactions", addr)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleDEGetDungeons(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	// Args can come as positional or keyword.
	// Client calls DEGetDungeons(factionID=...) or DEGetDungeons(dungeonVID=...)
	var factionID, dungeonVID int64

	// Check kwargs first
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok {
				switch k.Value {
				case "factionID":
					if v, ok := e.Value.(marshal.PyInt); ok {
						factionID = int64(v.Value)
					}
				case "dungeonVID":
					if v, ok := e.Value.(marshal.PyInt); ok {
						dungeonVID = int64(v.Value)
					}
				}
			}
		}
	}
	// Fallback: positional arg[0] = factionID
	if factionID == 0 && dungeonVID == 0 && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			factionID = int64(v.Value)
		}
	}

	var result marshal.PyRep
	var err error
	if dungeonVID != 0 {
		result, err = evedb.GetDungeonByVID(ctx, sqldb, dungeonVID)
	} else {
		result, err = evedb.GetDungeons(ctx, sqldb, factionID)
	}
	if err != nil {
		log.Printf("[%s] dungeon.DEGetDungeons factionID=%d dungeonVID=%d: %v", addr, factionID, dungeonVID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DEGetDungeons factionID=%d dungeonVID=%d", addr, factionID, dungeonVID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleDEGetRooms(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var dungeonVID int64
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "dungeonVID" {
				if v, ok := e.Value.(marshal.PyInt); ok {
					dungeonVID = int64(v.Value)
				}
			}
		}
	}
	if dungeonVID == 0 && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			dungeonVID = int64(v.Value)
		}
	}

	result, err := evedb.GetRooms(ctx, sqldb, dungeonVID)
	if err != nil {
		log.Printf("[%s] dungeon.DEGetRooms dungeonVID=%d: %v", addr, dungeonVID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DEGetRooms dungeonVID=%d", addr, dungeonVID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleDEGetObjects(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var roomID int64
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "roomID" {
				if v, ok := e.Value.(marshal.PyInt); ok {
					roomID = int64(v.Value)
				}
			}
		}
	}
	if roomID == 0 && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			roomID = int64(v.Value)
		}
	}

	result, err := evedb.GetRoomObjectsRowset(ctx, sqldb, roomID)
	if err != nil {
		log.Printf("[%s] dungeon.DEGetObjects roomID=%d: %v", addr, roomID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DEGetObjects roomID=%d", addr, roomID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleEditObject(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	// Client calls: EditObject(objectID=..., x=..., y=..., z=..., yaw=..., pitch=..., roll=..., radius=...)
	// All kwargs; only some fields may be present per call.
	var objectID int64
	var f evedb.EditObjectFields

	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			k, ok := e.Key.(marshal.PyString)
			if !ok {
				continue
			}
			switch k.Value {
			case "objectID":
				if v, ok := e.Value.(marshal.PyInt); ok {
					objectID = int64(v.Value)
				}
			case "x":
				v := pyToFloat(e.Value)
				f.X = &v
			case "y":
				v := pyToFloat(e.Value)
				f.Y = &v
			case "z":
				v := pyToFloat(e.Value)
				f.Z = &v
			case "yaw":
				v := pyToFloat(e.Value)
				f.Yaw = &v
			case "pitch":
				v := pyToFloat(e.Value)
				f.Pitch = &v
			case "roll":
				v := pyToFloat(e.Value)
				f.Roll = &v
			case "radius":
				v := pyToFloat(e.Value)
				f.Radius = &v
			}
		}
	}

	if err := evedb.EditObject(ctx, sqldb, objectID, f); err != nil {
		log.Printf("[%s] dungeon.EditObject objectID=%d: %v", addr, objectID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → dungeon.EditObject objectID=%d", addr, objectID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleCopyObject(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	// Client calls: CopyObject(objectID, roomID, offsetX=..., offsetY=..., offsetZ=...)
	var objectID int64
	var offsetX, offsetY, offsetZ float64

	// Positional: objectID, roomID
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			objectID = int64(v.Value)
		}
	}
	// roomID (arg[1]) is unused: CopyObject inherits the room from the source object.

	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok {
				switch k.Value {
				case "offsetX":
					offsetX = pyToFloat(e.Value)
				case "offsetY":
					offsetY = pyToFloat(e.Value)
				case "offsetZ":
					offsetZ = pyToFloat(e.Value)
				}
			}
		}
	}

	newID, err := evedb.CopyObject(ctx, sqldb, objectID, offsetX, offsetY, offsetZ)
	if err != nil {
		log.Printf("[%s] dungeon.CopyObject objectID=%d: %v", addr, objectID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → dungeon.CopyObject objectID=%d → newID=%d", addr, objectID, newID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(newID)})
}

func handleRemoveObject(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var objectID int64
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			objectID = int64(v.Value)
		}
	}
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "objectID" {
				if v, ok := e.Value.(marshal.PyInt); ok {
					objectID = int64(v.Value)
				}
			}
		}
	}

	if err := evedb.RemoveObject(ctx, sqldb, objectID); err != nil {
		log.Printf("[%s] dungeon.RemoveObject objectID=%d: %v", addr, objectID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → dungeon.RemoveObject objectID=%d", addr, objectID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleCreateDungeon(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	// DECreateDungeon(dungeonVName, factionID) or kwargs
	var name string
	var factionID int64

	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			k, ok := e.Key.(marshal.PyString)
			if !ok {
				continue
			}
			switch k.Value {
			case "dungeonVName", "dungeonName", "name":
				if v, ok := e.Value.(marshal.PyString); ok {
					name = v.Value
				}
			case "factionID":
				if v, ok := e.Value.(marshal.PyInt); ok {
					factionID = int64(v.Value)
				}
			}
		}
	}
	if name == "" && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyString); ok {
			name = v.Value
		}
	}
	if factionID == 0 && len(cs.ArgTuple.Items) >= 2 {
		if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
			factionID = int64(v.Value)
		}
	}
	if name == "" {
		name = "New Dungeon"
	}

	newID, err := evedb.CreateDungeon(ctx, sqldb, name, factionID)
	if err != nil {
		log.Printf("[%s] dungeon.CreateDungeon: %v", addr, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → dungeon.CreateDungeon %q factionID=%d → dungeonVID=%d", addr, name, factionID, newID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(newID)})
}

func handleCreateRoom(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	// DECreateRoom(dungeonVID, roomName) or kwargs
	var dungeonVID int64
	var roomName string

	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			k, ok := e.Key.(marshal.PyString)
			if !ok {
				continue
			}
			switch k.Value {
			case "dungeonVID":
				if v, ok := e.Value.(marshal.PyInt); ok {
					dungeonVID = int64(v.Value)
				}
			case "roomName", "name":
				if v, ok := e.Value.(marshal.PyString); ok {
					roomName = v.Value
				}
			}
		}
	}
	if dungeonVID == 0 && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			dungeonVID = int64(v.Value)
		}
	}
	if roomName == "" && len(cs.ArgTuple.Items) >= 2 {
		if v, ok := cs.ArgTuple.Items[1].(marshal.PyString); ok {
			roomName = v.Value
		}
	}
	if roomName == "" {
		roomName = "New Room"
	}

	newID, err := evedb.CreateRoom(ctx, sqldb, dungeonVID, roomName)
	if err != nil {
		log.Printf("[%s] dungeon.CreateRoom dungeonVID=%d: %v", addr, dungeonVID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → dungeon.CreateRoom dungeonVID=%d %q → roomID=%d", addr, dungeonVID, roomName, newID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(newID)})
}

func handleAddObject(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	// DEAddObject(roomID, typeID, x, y, z), positional or kwargs
	var roomID, typeID int64
	var x, y, z float64

	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			k, ok := e.Key.(marshal.PyString)
			if !ok {
				continue
			}
			switch k.Value {
			case "roomID":
				if v, ok := e.Value.(marshal.PyInt); ok {
					roomID = int64(v.Value)
				}
			case "typeID":
				if v, ok := e.Value.(marshal.PyInt); ok {
					typeID = int64(v.Value)
				}
			case "x":
				x = pyToFloat(e.Value)
			case "y":
				y = pyToFloat(e.Value)
			case "z":
				z = pyToFloat(e.Value)
			}
		}
	}
	if roomID == 0 && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			roomID = int64(v.Value)
		}
	}
	if typeID == 0 && len(cs.ArgTuple.Items) >= 2 {
		if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
			typeID = int64(v.Value)
		}
	}
	if len(cs.ArgTuple.Items) >= 5 {
		x = pyToFloat(cs.ArgTuple.Items[2])
		y = pyToFloat(cs.ArgTuple.Items[3])
		z = pyToFloat(cs.ArgTuple.Items[4])
	}

	newID, err := evedb.AddObject(ctx, sqldb, roomID, typeID, x, y, z)
	if err != nil {
		log.Printf("[%s] dungeon.AddObject roomID=%d typeID=%d: %v", addr, roomID, typeID, err)
		return server.SendCallException(c, pkt, accountID, nil)
	}
	log.Printf("[%s] → dungeon.AddObject roomID=%d typeID=%d → objectID=%d", addr, roomID, typeID, newID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(newID)})
}

func handleDEGetCategories2(ctx context.Context, c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	result, err := evedb.GetDECategories(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] dungeon.DEGetCategories: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DEGetCategories", addr)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleDEGetGroups(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var categoryID int64
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "categoryID" {
				if v, ok := e.Value.(marshal.PyInt); ok {
					categoryID = int64(v.Value)
				}
			}
		}
	}
	if categoryID == 0 && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			categoryID = int64(v.Value)
		}
	}

	result, err := evedb.GetDEGroups(ctx, sqldb, categoryID)
	if err != nil {
		log.Printf("[%s] dungeon.DEGetGroups categoryID=%d: %v", addr, categoryID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DEGetGroups categoryID=%d", addr, categoryID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleDEGetTypes(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var groupID int64
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "groupID" {
				if v, ok := e.Value.(marshal.PyInt); ok {
					groupID = int64(v.Value)
				}
			}
		}
	}
	if groupID == 0 && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			groupID = int64(v.Value)
		}
	}

	result, err := evedb.GetDETypes(ctx, sqldb, groupID)
	if err != nil {
		log.Printf("[%s] dungeon.DEGetTypes groupID=%d: %v", addr, groupID, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DEGetTypes groupID=%d", addr, groupID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleDESearchTypes(ctx context.Context, c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket, accountID int64, sqldb *sql.DB) error {
	var query string
	if cs.ArgDict != nil {
		for _, e := range cs.ArgDict.Items {
			if k, ok := e.Key.(marshal.PyString); ok && k.Value == "query" {
				if v, ok := e.Value.(marshal.PyString); ok {
					query = v.Value
				}
			}
		}
	}
	if query == "" && len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyString); ok {
			query = v.Value
		}
	}

	result, err := evedb.SearchDETypes(ctx, sqldb, query)
	if err != nil {
		log.Printf("[%s] dungeon.DESearchTypes query=%q: %v", addr, query, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{})
	}
	log.Printf("[%s] → dungeon.DESearchTypes query=%q", addr, query)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// pyToFloat extracts a float64 from a PyReal or PyInt.
func pyToFloat(r marshal.PyRep) float64 {
	switch v := r.(type) {
	case marshal.PyReal:
		return v.Value
	case marshal.PyInt:
		return float64(v.Value)
	}
	return 0
}

