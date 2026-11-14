package main

// Crystal wear shared by mining lasers and laser turrets. A crystal is never used
// up by a cycle; one with crystalsGetDamaged (786) gains volatility damage and
// shatters at its hp.

import (
	"context"
	"database/sql"
	"log"
	"math/rand"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// newCrystalState reads the wear attributes of the crystal item itemID of type
// typeID. yieldMult starts at 1; the mining path sets it.
func newCrystalState(ctx context.Context, sqldb *sql.DB, itemID, typeID int64, attrs map[int64]float64) *miningCrystalState {
	hp := attrs[dogma.AttrHP]
	if hp <= 0 {
		hp = 1
	}
	return &miningCrystalState{
		itemID:      itemID,
		typeID:      typeID,
		yieldMult:   1,
		getsDamaged: attrs[dogma.AttrCrystalsGetDamaged] != 0,
		volChance:   attrs[dogma.AttrCrystalVolatilityChance],
		volDamage:   attrs[dogma.AttrCrystalVolatilityDamage],
		hp:          hp,
		damage:      evedb.GetItemAttrFloat(ctx, sqldb, itemID, dogma.AttrDamage, 0),
	}
}

// wear rolls volatility for one completed cycle. It persists the new damage and
// pushes it to the client, or deletes the crystal when it shatters. It reports
// whether the crystal is broken. label prefixes the log lines.
func (crystal *miningCrystalState) wear(c *evenet.Conn, addr string, accountID, charID int64, sqldb *sql.DB, label string) bool {
	if crystal == nil || crystal.broken {
		return crystal != nil && crystal.broken
	}
	if !crystal.getsDamaged || sqldb == nil || rand.Float64() >= crystal.volChance {
		return false
	}
	oldDmg := crystal.damage
	crystal.damage += crystal.volDamage
	wearCtx, wearCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer wearCancel()
	if crystal.damage < crystal.hp {
		if setErr := evedb.SetItemAttrFloat(wearCtx, sqldb, crystal.itemID, dogma.AttrDamage, crystal.damage); setErr != nil {
			log.Printf("[%s] %s crystal wear persist: %v", addr, label, setErr)
		}
		sendModuleAttributeChange(c, accountID, charID, crystal.itemID, dogma.AttrDamage, crystal.damage, oldDmg)
		crystal.markUsed(wearCtx, c, addr, accountID, sqldb, label)
		return false
	}
	crystal.broken = true
	// Shatter: remove the crystal item and tell the client.
	if er, erErr := evedb.GetEntityRow(wearCtx, sqldb, crystal.itemID); erErr == nil && er != nil {
		oldLoc := er.LocationID
		if delErr := evedb.DeleteItemWithAttrs(wearCtx, sqldb, crystal.itemID); delErr != nil {
			log.Printf("[%s] %s crystal shatter delete: %v", addr, label, delErr)
		} else {
			er.LocationID = 6 // system junkyard: client drops the item
			shatterArgs := marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(er),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(oldLoc)}},
				}},
			}}
			if nErr := server.SendNotification(c, accountID, "OnItemChange", "charid", shatterArgs); nErr != nil {
				log.Printf("[%s] %s crystal shatter OnItemChange: %v", addr, label, nErr)
			}
		}
	}
	log.Printf("[%s] %s crystal %d (type %d) shattered", addr, label, crystal.itemID, crystal.typeID)
	return true
}

// markUsed makes a worn crystal a singleton, so stack merges keep its damage apart
// and the client's ammo menu lists it as "(used)" (shipmodulebutton.py). Runs once
// per activation; the change dict names ixSingleton (6) with its old value.
func (crystal *miningCrystalState) markUsed(ctx context.Context, c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB, label string) {
	if crystal.used {
		return
	}
	crystal.used = true
	changed, err := evedb.MarkItemSingleton(ctx, sqldb, crystal.itemID)
	if err != nil {
		log.Printf("[%s] %s crystal mark used: %v", addr, label, err)
		return
	}
	if !changed {
		return
	}
	if er, erErr := evedb.GetEntityRow(ctx, sqldb, crystal.itemID); erErr == nil && er != nil {
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(er),
			marshal.PyDict{Items: []marshal.DictEntry{{Key: marshal.PyInt{Value: 6}, Value: marshal.PyInt{Value: 0}}}},
		}})
	}
}
