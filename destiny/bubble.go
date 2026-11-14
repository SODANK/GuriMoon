package destiny

import evenet "gurimoon/net"

// BubbleRadius is the visibility radius in metres (~250 km).
const BubbleRadius = 250_000.0

// Bubble is a spatial visibility group: entities within BubbleRadius of Center
// see each other. Anchored bubbles are pre-seeded at celestials at system init;
// dynamic ones are created for deep-space positions outside any anchored bubble.
type Bubble struct {
	ID       int
	Center   Vec3
	Entities map[uint32]*Entity
	AnchorID int64 // celestial itemID if anchored (0 = dynamic)
}

// NewBubble creates a bubble centered at pos with the given ID.
func NewBubble(id int, pos Vec3) *Bubble {
	return &Bubble{
		ID:       id,
		Center:   pos,
		Entities: make(map[uint32]*Entity),
	}
}

// Contains returns true if pos is within BubbleRadius of the bubble center.
func (b *Bubble) Contains(pos Vec3) bool {
	return b.Center.Sub(pos).LenSq() <= BubbleRadius*BubbleRadius
}

// PlayerConns returns the evenet.Conn for every player entity in this bubble.
func (b *Bubble) PlayerConns() []*evenet.Conn {
	var out []*evenet.Conn
	for _, e := range b.Entities {
		if e.IsPlayer && e.Conn != nil {
			out = append(out, e.Conn)
		}
	}
	return out
}

// PlayerEntities returns all player entities in this bubble.
func (b *Bubble) PlayerEntities() []*Entity {
	var out []*Entity
	for _, e := range b.Entities {
		if e.IsPlayer {
			out = append(out, e)
		}
	}
	return out
}

// PlayerEntitiesExcept returns all player entities except the one with excludeID.
func (b *Bubble) PlayerEntitiesExcept(excludeID uint32) []*Entity {
	var out []*Entity
	for _, e := range b.Entities {
		if e.IsPlayer && e.ID != excludeID {
			out = append(out, e)
		}
	}
	return out
}

// ConnectedPlayerEntities returns player entities that have a live connection.
// Bot/headless player entities (Conn == nil) are excluded.
func (b *Bubble) ConnectedPlayerEntities() []*Entity {
	var out []*Entity
	for _, e := range b.Entities {
		if e.IsPlayer && e.Conn != nil {
			out = append(out, e)
		}
	}
	return out
}

// ConnectedPlayerEntitiesExcept returns connected player entities except excludeID.
func (b *Bubble) ConnectedPlayerEntitiesExcept(excludeID uint32) []*Entity {
	var out []*Entity
	for _, e := range b.Entities {
		if e.IsPlayer && e.Conn != nil && e.ID != excludeID {
			out = append(out, e)
		}
	}
	return out
}

// AllEntities returns all entities in this bubble as a slice.
func (b *Bubble) AllEntities() []*Entity {
	out := make([]*Entity, 0, len(b.Entities))
	for _, e := range b.Entities {
		out = append(out, e)
	}
	return out
}

// NPCEntities returns non-player entities in this bubble (NPC ships).
func (b *Bubble) NPCEntities() []*Entity {
	var out []*Entity
	for _, e := range b.Entities {
		if !e.IsPlayer {
			out = append(out, e)
		}
	}
	return out
}
