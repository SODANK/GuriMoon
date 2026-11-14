package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// ---- Agent LP-store offers ----
//
// Offers fill the journal Offers tab: reward items for a payment of items, ISK and
// loyalty points. Authored in ESP; tables are created in EnsureMissionTables.

// retireStaticAgtOffers renames the shipped static agtOffers table to agtOffersStatic
// so the authored table can take the name. A table with an lpCost column is ours.
func retireStaticAgtOffers(ctx context.Context, db *sql.DB) error {
	exists, err := tableExists(ctx, db, "agtOffers")
	if err != nil || !exists {
		return err
	}
	ours, err := columnExists(ctx, db, "agtOffers", "lpCost")
	if err != nil || ours {
		return err
	}
	parked, err := tableExists(ctx, db, "agtOffersStatic")
	if err != nil {
		return err
	}
	if !parked {
		if _, err := db.ExecContext(ctx, `RENAME TABLE agtOffers TO agtOffersStatic`); err != nil {
			return fmt.Errorf("retireStaticAgtOffers rename: %w", err)
		}
		return nil
	}
	// A copy is already parked. Drop this one only when empty (the shipped dump has no
	// rows); otherwise stop for an operator.
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM agtOffers`).Scan(&n); err != nil {
		return fmt.Errorf("retireStaticAgtOffers count: %w", err)
	}
	if n > 0 {
		return fmt.Errorf("retireStaticAgtOffers: agtOffers holds %d static rows and agtOffersStatic already exists; rename or drop one by hand", n)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE agtOffers`); err != nil {
		return fmt.Errorf("retireStaticAgtOffers drop: %w", err)
	}
	return nil
}

// OfferItem is one reward or cost line of an offer.
type OfferItem struct {
	EntryID   int64
	TypeID    int64
	Quantity  int
	ExtraInfo string // free-text suffix, e.g. "single run copy, material level: 20"
}

// AgentOffer is one LP-store special offer. Availability is an allow-list per axis
// (corporation, factions, divisions); an empty axis means any and the axes AND.
// See docs/db-12-notes.md#agent-offers.
type AgentOffer struct {
	OfferID       int64
	OfferName     string
	Description   string
	CorporationID int64  // 0 = any corporation
	Factions      string // comma-separated factionIDs, empty = any
	Divisions     string // comma-separated divisionIDs, empty = any
	LPCost        int
	ISKCost       int64
	MinStanding   float64
	ExpiresDays   int
	Enabled       bool
	Rewards       []OfferItem
	Costs         []OfferItem
}

// offerSelectCols is the column list every offer query shares; scanOffers reads it in order.
const offerSelectCols = `offerID, offerName, COALESCE(description,''), COALESCE(corporationID,0),
	       COALESCE(allowedFactions,''), COALESCE(allowedDivisions,''),
	       lpCost, iskCost, minStanding, expiresDays, enabled`

func scanOffers(rows *sql.Rows) ([]*AgentOffer, error) {
	var offers []*AgentOffer
	for rows.Next() {
		o := &AgentOffer{}
		var enabled int
		if err := rows.Scan(&o.OfferID, &o.OfferName, &o.Description, &o.CorporationID,
			&o.Factions, &o.Divisions,
			&o.LPCost, &o.ISKCost, &o.MinStanding, &o.ExpiresDays, &enabled); err != nil {
			return nil, err
		}
		o.Enabled = enabled != 0
		offers = append(offers, o)
	}
	return offers, rows.Err()
}

// loadOfferItems fills Rewards/Costs for each offer in the slice.
func loadOfferItems(ctx context.Context, db *sql.DB, offers []*AgentOffer) error {
	byID := make(map[int64]*AgentOffer, len(offers))
	for _, o := range offers {
		byID[o.OfferID] = o
	}
	if len(byID) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT entryID, offerID, role, typeID, quantity, COALESCE(extraInfo, '')
		FROM agtOfferItems ORDER BY entryID`)
	if err != nil {
		return fmt.Errorf("loadOfferItems: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it OfferItem
		var offerID int64
		var role string
		if err := rows.Scan(&it.EntryID, &offerID, &role, &it.TypeID, &it.Quantity, &it.ExtraInfo); err != nil {
			return err
		}
		o, ok := byID[offerID]
		if !ok {
			continue
		}
		if role == "cost" {
			o.Costs = append(o.Costs, it)
		} else {
			o.Rewards = append(o.Rewards, it)
		}
	}
	return rows.Err()
}

// GetOffersForAgent returns the enabled offers whose allow-lists admit the agent.
// A NULL or empty list on an axis admits everyone.
func GetOffersForAgent(ctx context.Context, db *sql.DB, corporationID, factionID, divisionID int64) ([]*AgentOffer, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+offerSelectCols+`
		FROM agtOffers
		WHERE enabled = 1
		  AND (corporationID IS NULL OR corporationID = ?)
		  AND (allowedFactions IS NULL OR allowedFactions = '' OR FIND_IN_SET(?, allowedFactions) > 0)
		  AND (allowedDivisions IS NULL OR allowedDivisions = '' OR FIND_IN_SET(?, allowedDivisions) > 0)
		ORDER BY offerID`, corporationID, factionID, divisionID)
	if err != nil {
		return nil, fmt.Errorf("GetOffersForAgent: %w", err)
	}
	defer rows.Close()
	offers, err := scanOffers(rows)
	if err != nil {
		return nil, err
	}
	if err := loadOfferItems(ctx, db, offers); err != nil {
		return nil, err
	}
	return offers, nil
}

// inAllowList reports whether id passes a comma-separated allow-list; an empty list
// admits everyone, the same reading GetOffersForAgent gives FIND_IN_SET.
func inAllowList(list string, id int64) bool {
	list = strings.TrimSpace(list)
	if list == "" {
		return true
	}
	want := strconv.FormatInt(id, 10)
	for _, part := range strings.Split(list, ",") {
		if strings.TrimSpace(part) == want {
			return true
		}
	}
	return false
}

// AvailableTo reports whether an agent in this corporation, faction and division
// presents this offer. It mirrors the WHERE clause of GetOffersForAgent.
func (o *AgentOffer) AvailableTo(corporationID, factionID, divisionID int64) bool {
	if !o.Enabled {
		return false
	}
	if o.CorporationID != 0 && o.CorporationID != corporationID {
		return false
	}
	return inAllowList(o.Factions, factionID) && inAllowList(o.Divisions, divisionID)
}

// LPAgent is an agent a character has a loyalty-point relationship with.
type LPAgent struct {
	AgentID       int64
	CorporationID int64
	DivisionID    int64
	Points        int
}

// GetCharLPAgents returns the agents a character holds loyalty points with, whose
// offers show in the journal Offers tab.
func GetCharLPAgents(ctx context.Context, db *sql.DB, charID int64) ([]LPAgent, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lp.agentID, COALESCE(a.corporationID, 0), COALESCE(a.divisionID, 0), lp.points
		FROM agentLoyaltyPoints lp
		LEFT JOIN agtAgents a ON a.agentID = lp.agentID
		WHERE lp.charID = ?`, charID)
	if err != nil {
		return nil, fmt.Errorf("GetCharLPAgents: %w", err)
	}
	defer rows.Close()
	var out []LPAgent
	for rows.Next() {
		var a LPAgent
		if err := rows.Scan(&a.AgentID, &a.CorporationID, &a.DivisionID, &a.Points); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetOfferByID returns a single offer with items, or nil if not found.
func GetOfferByID(ctx context.Context, db *sql.DB, offerID int64) (*AgentOffer, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+offerSelectCols+` FROM agtOffers WHERE offerID = ?`, offerID)
	if err != nil {
		return nil, fmt.Errorf("GetOfferByID: %w", err)
	}
	defer rows.Close()
	offers, err := scanOffers(rows)
	if err != nil {
		return nil, err
	}
	if len(offers) == 0 {
		return nil, nil
	}
	if err := loadOfferItems(ctx, db, offers); err != nil {
		return nil, err
	}
	return offers[0], nil
}

// GetAllOffers returns every offer (any enabled state) for the ESP editor.
func GetAllOffers(ctx context.Context, db *sql.DB) ([]*AgentOffer, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+offerSelectCols+` FROM agtOffers ORDER BY offerID`)
	if err != nil {
		return nil, fmt.Errorf("GetAllOffers: %w", err)
	}
	defer rows.Close()
	offers, err := scanOffers(rows)
	if err != nil {
		return nil, err
	}
	if err := loadOfferItems(ctx, db, offers); err != nil {
		return nil, err
	}
	return offers, nil
}

// CreateOffer inserts a new offer and returns its ID.
func CreateOffer(ctx context.Context, db *sql.DB, name string) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO agtOffers (offerName, description) VALUES (?, '')`, name)
	if err != nil {
		return 0, fmt.Errorf("CreateOffer: %w", err)
	}
	return res.LastInsertId()
}

// UpdateOffer saves the scalar fields of an offer.
func UpdateOffer(ctx context.Context, db *sql.DB, o *AgentOffer) error {
	var corpID interface{}
	if o.CorporationID > 0 {
		corpID = o.CorporationID
	}
	enabled := 0
	if o.Enabled {
		enabled = 1
	}
	_, err := db.ExecContext(ctx, `
		UPDATE agtOffers
		SET offerName = ?, description = ?, corporationID = ?,
		    allowedFactions = ?, allowedDivisions = ?,
		    lpCost = ?, iskCost = ?, minStanding = ?, expiresDays = ?, enabled = ?
		WHERE offerID = ?`,
		o.OfferName, o.Description, corpID, o.Factions, o.Divisions,
		o.LPCost, o.ISKCost, o.MinStanding, o.ExpiresDays, enabled, o.OfferID)
	if err != nil {
		return fmt.Errorf("UpdateOffer: %w", err)
	}
	return nil
}

// DeleteOffer removes an offer and its items.
func DeleteOffer(ctx context.Context, db *sql.DB, offerID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM agtOfferItems WHERE offerID = ?`, offerID); err != nil {
		return fmt.Errorf("DeleteOffer items: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM agtOffers WHERE offerID = ?`, offerID); err != nil {
		return fmt.Errorf("DeleteOffer: %w", err)
	}
	return nil
}

// AddOfferItem appends a reward/cost line to an offer.
func AddOfferItem(ctx context.Context, db *sql.DB, offerID int64, role string, typeID int64, qty int, extraInfo string) error {
	if role != "cost" {
		role = "reward"
	}
	var extra interface{}
	if extraInfo != "" {
		extra = extraInfo
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO agtOfferItems (offerID, role, typeID, quantity, extraInfo)
		VALUES (?, ?, ?, ?, ?)`, offerID, role, typeID, qty, extra)
	if err != nil {
		return fmt.Errorf("AddOfferItem: %w", err)
	}
	return nil
}

// DeleteOfferItem removes one reward/cost line.
func DeleteOfferItem(ctx context.Context, db *sql.DB, entryID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM agtOfferItems WHERE entryID = ?`, entryID)
	if err != nil {
		return fmt.Errorf("DeleteOfferItem: %w", err)
	}
	return nil
}
