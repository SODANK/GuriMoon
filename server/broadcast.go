package server

import (
	"log"

	"gurimoon/esp"
	"gurimoon/marshal"
)

// BroadcastNotification sends a notification to every session matching filter.
func BroadcastNotification(sessReg *esp.SessionRegistry, filter func(*esp.Session) bool,
	eventType, bcastIDType string, args marshal.PyRep) {
	sessReg.Broadcast(filter, func(s *esp.Session) {
		if s.Conn == nil {
			return
		}
		if err := SendNotification(s.Conn, s.AccountID, eventType, bcastIDType, args); err != nil {
			log.Printf("[%s] broadcast %s: %v", s.Addr, eventType, err)
		}
	})
}

// BroadcastNotificationExcept sends a notification to every matching session
// except the one identified by excludeAddr.
func BroadcastNotificationExcept(sessReg *esp.SessionRegistry, excludeAddr string,
	filter func(*esp.Session) bool, eventType, bcastIDType string, args marshal.PyRep) {
	sessReg.Broadcast(func(s *esp.Session) bool {
		return s.Addr != excludeAddr && filter(s)
	}, func(s *esp.Session) {
		if s.Conn == nil {
			return
		}
		if err := SendNotification(s.Conn, s.AccountID, eventType, bcastIDType, args); err != nil {
			log.Printf("[%s] broadcast %s: %v", s.Addr, eventType, err)
		}
	})
}

// NotifyStation sends a notification to all sessions at the given station.
func NotifyStation(sessReg *esp.SessionRegistry, stationID int64,
	eventType, bcastIDType string, args marshal.PyRep) {
	BroadcastNotification(sessReg, func(s *esp.Session) bool {
		return s.StationID == stationID
	}, eventType, bcastIDType, args)
}

// NotifyCharacter sends a notification to the session of a specific character.
func NotifyCharacter(sessReg *esp.SessionRegistry, charID int64,
	eventType, bcastIDType string, args marshal.PyRep) {
	s := sessReg.FindByCharID(charID)
	if s == nil || s.Conn == nil {
		return
	}
	if err := SendNotification(s.Conn, s.AccountID, eventType, bcastIDType, args); err != nil {
		log.Printf("[%s] notify char %d %s: %v", s.Addr, charID, eventType, err)
	}
}
