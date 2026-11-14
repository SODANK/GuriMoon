package db

import (
	"context"
	"database/sql"
	"fmt"
)

// Journal refTypes for agent pay.
const (
	RefAgentMissionReward    = 33 // the mission's base ISK reward
	RefAgentMissionTimeBonus = 34 // the time bonus, paid when turned in before the bonus deadline
)

// PayMissionISK pays an agent mission reward or time bonus (refType 33 or 34) to charID, less the
// tax rate of the character's corporation. The corporation's cut goes to its wallet with a journal
// row of the same refType, as the NPC bounty split does. Players and rolebots both turn in through
// this, so both pay corp tax. A missing corporation or an out-of-range rate pays the whole amount.
// Returns the character's share and new balance.
func PayMissionISK(ctx context.Context, db *sql.DB, charID, agentID int64, refType int, amount float64) (charShare, newBalance float64, err error) {
	if amount <= 0 {
		bal, _ := GetCharacterBalance(ctx, db, charID)
		return 0, bal, nil
	}
	corpID, _ := GetCharCorpID(ctx, db, charID)
	taxRate := 0.0
	if corpID != 0 {
		if tr := GetCorpTaxRate(ctx, db, corpID); tr > 0 && tr <= 1 {
			taxRate = tr
		}
	}
	corpShare := amount * taxRate
	charShare = amount - corpShare

	if err := GiveISK(ctx, db, charID, charShare); err != nil {
		return 0, 0, fmt.Errorf("PayMissionISK: %w", err)
	}
	newBalance, err = GetCharacterBalance(ctx, db, charID)
	if err != nil {
		return charShare, 0, fmt.Errorf("PayMissionISK: %w", err)
	}
	if err := InsertJournalEntry(ctx, db, charID, refType, charID, agentID, charShare, newBalance, ""); err != nil {
		return charShare, newBalance, fmt.Errorf("PayMissionISK: %w", err)
	}

	if corpShare > 0 {
		if err := CreditCorpWallet(ctx, db, corpID, corpShare); err != nil {
			return charShare, newBalance, fmt.Errorf("PayMissionISK: %w", err)
		}
		corpBal, _ := GetCorpBalance(ctx, db, corpID)
		if err := InsertJournalEntry(ctx, db, corpID, refType, charID, agentID, corpShare, corpBal, ""); err != nil {
			return charShare, newBalance, fmt.Errorf("PayMissionISK: %w", err)
		}
	}
	return charShare, newBalance, nil
}
