package db

// Corporation shares: the treasury row, transfers, issuing and share-weighted votes.
// A holder is any owner, pilot or corporation, so crpcharshares.characterID is a holderID.
// See docs/db-7-notes.md#corporation-shares.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"time"
)

// CorpStartingShares is what AddCorporation issues into a new treasury.
const CorpStartingShares = 1000

// EnsureCorpShareTreasury writes the treasury row of every player corporation as
// issued minus held. Idempotent; called at boot.
func EnsureCorpShareTreasury(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx, `
		INSERT INTO crpcharshares (characterID, corporationID, shares)
		SELECT c.corporationID, c.corporationID, c.shares - COALESCE(h.held, 0)
		  FROM corporation c
		  LEFT JOIN (SELECT corporationID, SUM(shares) AS held
		               FROM crpcharshares
		              WHERE characterID <> corporationID
		              GROUP BY corporationID) h ON h.corporationID = c.corporationID
		 WHERE NOT EXISTS (SELECT 1 FROM crpNPCCorporations n WHERE n.corporationID = c.corporationID)
		   AND c.shares > COALESCE(h.held, 0)
		ON DUPLICATE KEY UPDATE shares = VALUES(shares)`)
	if err != nil {
		return fmt.Errorf("EnsureCorpShareTreasury: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		// Rows affected on an upsert: 0 unchanged, 1 inserted, 2 changed.
		log.Printf("[shares] treasury rows created/repaired (rows affected %d)", n)
	}
	return nil
}

// ShareHolding returns how many shares of corpID the holder owns.
func ShareHolding(ctx context.Context, db DBTX, holderID, corpID int64) int64 {
	var n int64
	_ = db.QueryRowContext(ctx,
		`SELECT shares FROM crpcharshares WHERE characterID = ? AND corporationID = ?`,
		holderID, corpID).Scan(&n)
	return n
}

// IsShareholder reports whether holderID owns any shares of corpID.
func IsShareholder(ctx context.Context, db *sql.DB, holderID, corpID int64) bool {
	return ShareHolding(ctx, db, holderID, corpID) > 0
}

// ShareOwnerKind classifies a prospective shareholder: "character" for a
// player pilot, "corporation" for a player corporation, "" for anything that
// cannot hold shares (NPC agents, NPC corps, unknown IDs).
func ShareOwnerKind(ctx context.Context, db *sql.DB, ownerID int64) string {
	var n int
	if db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE characterID = ? AND accountID > 0`,
		ownerID).Scan(&n) == nil && n > 0 {
		return "character"
	}
	if db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM corporation c
		 WHERE c.corporationID = ?
		   AND NOT EXISTS (SELECT 1 FROM crpNPCCorporations n WHERE n.corporationID = c.corporationID)`,
		ownerID).Scan(&n) == nil && n > 0 {
		return "corporation"
	}
	return ""
}

// ShareMove is both sides of a transfer, before and after, for the two
// OnShareChange pushes.
type ShareMove struct {
	FromOld, FromNew int64
	ToOld, ToNew     int64
}

// ErrNotEnoughShares is returned when the sender holds fewer than asked.
var ErrNotEnoughShares = errors.New("not enough shares")

// MoveShares transfers qty shares of corpID between holders in one transaction. Both
// rows lock in ascending holder order so opposite transfers cannot deadlock. The
// caller validates authority and the recipient.
func MoveShares(ctx context.Context, db *sql.DB, corpID, fromHolder, toHolder, qty int64) (ShareMove, error) {
	var m ShareMove
	if qty <= 0 {
		return m, fmt.Errorf("share quantity must be positive")
	}
	if fromHolder == toHolder {
		return m, fmt.Errorf("the shares are already there")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return m, fmt.Errorf("MoveShares begin: %w", err)
	}
	defer tx.Rollback()
	if m, err = moveSharesTx(ctx, tx, corpID, fromHolder, toHolder, qty); err != nil {
		return m, err
	}
	if err := tx.Commit(); err != nil {
		return m, fmt.Errorf("MoveShares commit: %w", err)
	}
	return m, nil
}

// moveSharesTx is MoveShares' body inside a caller's transaction.
func moveSharesTx(ctx context.Context, tx DBTX, corpID, fromHolder, toHolder, qty int64) (ShareMove, error) {
	var m ShareMove
	lock := func(holder int64) (int64, error) {
		var n int64
		err := tx.QueryRowContext(ctx,
			`SELECT shares FROM crpcharshares WHERE characterID = ? AND corporationID = ? FOR UPDATE`,
			holder, corpID).Scan(&n)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return n, err
	}
	first, second := fromHolder, toHolder
	if second < first {
		first, second = second, first
	}
	held := map[int64]int64{}
	for _, h := range []int64{first, second} {
		n, err := lock(h)
		if err != nil {
			return m, fmt.Errorf("MoveShares lock: %w", err)
		}
		held[h] = n
	}
	m.FromOld, m.ToOld = held[fromHolder], held[toHolder]
	if m.FromOld < qty {
		return m, fmt.Errorf("%w: only %d held", ErrNotEnoughShares, m.FromOld)
	}
	var err error
	// shares is unsigned: delete the row rather than debit through zero.
	if m.FromOld == qty {
		_, err = tx.ExecContext(ctx,
			`DELETE FROM crpcharshares WHERE characterID = ? AND corporationID = ?`, fromHolder, corpID)
	} else {
		_, err = tx.ExecContext(ctx,
			`UPDATE crpcharshares SET shares = shares - ? WHERE characterID = ? AND corporationID = ?`,
			qty, fromHolder, corpID)
	}
	if err != nil {
		return m, fmt.Errorf("MoveShares debit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO crpcharshares (characterID, corporationID, shares) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE shares = shares + VALUES(shares)`, toHolder, corpID, qty); err != nil {
		return m, fmt.Errorf("MoveShares credit: %w", err)
	}
	m.FromNew, m.ToNew = m.FromOld-qty, m.ToOld+qty
	return m, nil
}

// TradeShares sells qty shares of corpID from fromHolder to toHolder for isk (toHolder's
// wallet to fromHolder's) in one transaction: money, both journal rows and holdings
// commit together. ErrInsufficientFunds or ErrNotEnoughShares leaves nothing moved;
// isk 0 is a plain move. Covers a member buying from the treasury and a buyback into it.
func TradeShares(ctx context.Context, db *sql.DB, corpID, fromHolder, toHolder, qty int64,
	isk float64, refTypeID int, reason string) (ShareMove, error) {
	var m ShareMove
	if qty <= 0 {
		return m, fmt.Errorf("share quantity must be positive")
	}
	if fromHolder == toHolder {
		return m, fmt.Errorf("the shares are already there")
	}
	if isk < 0 {
		return m, fmt.Errorf("TradeShares: negative price")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return m, fmt.Errorf("TradeShares begin: %w", err)
	}
	defer tx.Rollback()
	if m, err = moveSharesTx(ctx, tx, corpID, fromHolder, toHolder, qty); err != nil {
		return m, err
	}
	if isk > 0 {
		if _, _, err := transferISKTx(ctx, tx, toHolder, fromHolder, isk, refTypeID, reason); err != nil {
			return m, err
		}
	}
	if err := tx.Commit(); err != nil {
		return m, fmt.Errorf("TradeShares commit: %w", err)
	}
	return m, nil
}

// ShareHoldingRow is one row of a corporation's register, as plain values.
type ShareHoldingRow struct {
	HolderID int64
	Shares   int64
}

// ShareHolders lists every holder of corpID's shares, the treasury included
// (HolderID == corpID), largest first.
func ShareHolders(ctx context.Context, db DBTX, corpID int64) ([]ShareHoldingRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT characterID, shares FROM crpcharshares
		 WHERE corporationID = ? AND shares > 0
		 ORDER BY shares DESC, characterID ASC`, corpID)
	if err != nil {
		return nil, fmt.Errorf("ShareHolders: %w", err)
	}
	defer rows.Close()
	var out []ShareHoldingRow
	for rows.Next() {
		var r ShareHoldingRow
		if err := rows.Scan(&r.HolderID, &r.Shares); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ShareIssue is the before/after of an issue: the corporation's total (the
// home tab's "Shares" line) and its treasury holding (the corp wallet voucher).
type ShareIssue struct {
	OldTotal, NewTotal       int64
	OldTreasury, NewTreasury int64
}

// IssueCorpShares creates qty new shares of corpID into its treasury: the execution
// of a passed voteShares, and the ESP GM tool.
func IssueCorpShares(ctx context.Context, db *sql.DB, corpID, qty int64) (ShareIssue, error) {
	var r ShareIssue
	if qty <= 0 {
		return r, fmt.Errorf("share quantity must be positive")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return r, fmt.Errorf("IssueCorpShares begin: %w", err)
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx,
		`SELECT shares FROM corporation WHERE corporationID = ? FOR UPDATE`, corpID).Scan(&r.OldTotal); err != nil {
		return r, fmt.Errorf("IssueCorpShares read: %w", err)
	}
	r.OldTreasury = ShareHolding(ctx, tx, corpID, corpID)
	if _, err := tx.ExecContext(ctx,
		`UPDATE corporation SET shares = shares + ? WHERE corporationID = ?`, qty, corpID); err != nil {
		return r, fmt.Errorf("IssueCorpShares total: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO crpcharshares (characterID, corporationID, shares) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE shares = shares + VALUES(shares)`, corpID, corpID, qty); err != nil {
		return r, fmt.Errorf("IssueCorpShares treasury: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return r, fmt.Errorf("IssueCorpShares commit: %w", err)
	}
	r.NewTotal, r.NewTreasury = r.OldTotal+qty, r.OldTreasury+qty
	return r, nil
}

// DepositoryHolderID is the holder that keeps shares parked behind open exchange sell
// orders (the Secure Commerce Commission). They still belong to the seller: see
// BeneficialShareHolders.
const DepositoryHolderID = 1000132

// ShareHolderRetireHook runs first in ReturnSharesToIssuers, in the same transaction. The
// SCC module installs it to cancel the holder's orders (and a corporation's whole book), so
// depository shares and escrowed ISK come home before holdings are read. Nil without it.
var ShareHolderRetireHook func(ctx context.Context, db DBTX, holderID int64) error

// ReturnSharesToIssuers moves every holding of a holder being deleted back into each
// issuer's treasury and returns the issuers that grew. A corporation's own treasury is
// dropped.
func ReturnSharesToIssuers(ctx context.Context, db DBTX, holderID int64) ([]int64, error) {
	if ShareHolderRetireHook != nil {
		if err := ShareHolderRetireHook(ctx, db, holderID); err != nil {
			return nil, fmt.Errorf("ReturnSharesToIssuers retire: %w", err)
		}
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.corporationID, s.shares FROM crpcharshares s
		 WHERE s.characterID = ? AND s.corporationID <> ?
		   AND EXISTS (SELECT 1 FROM corporation c WHERE c.corporationID = s.corporationID)`,
		holderID, holderID)
	if err != nil {
		return nil, fmt.Errorf("ReturnSharesToIssuers read: %w", err)
	}
	type holding struct{ corpID, shares int64 }
	var hs []holding
	for rows.Next() {
		var h holding
		if rows.Scan(&h.corpID, &h.shares) == nil && h.shares > 0 {
			hs = append(hs, h)
		}
	}
	rows.Close()
	for _, h := range hs {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO crpcharshares (characterID, corporationID, shares) VALUES (?, ?, ?)
			ON DUPLICATE KEY UPDATE shares = shares + VALUES(shares)`, h.corpID, h.corpID, h.shares); err != nil {
			return nil, fmt.Errorf("ReturnSharesToIssuers credit %d: %w", h.corpID, err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM crpcharshares WHERE characterID = ?`, holderID); err != nil {
		return nil, fmt.Errorf("ReturnSharesToIssuers delete: %w", err)
	}
	issuers := make([]int64, 0, len(hs))
	for _, h := range hs {
		issuers = append(issuers, h.corpID)
	}
	return issuers, nil
}

// ShareVoteWeight is the shares a ballot by voterID carries in corpID's votes: the
// voter's holding plus every holding of a corporation they are CEO of. Shares behind
// open SCC sell orders still vote for their escrower. The escrow query is separate so
// a database without the exchange tables still returns the register half.
func ShareVoteWeight(ctx context.Context, db DBTX, voterID, corpID int64) int64 {
	var n, escrowed int64
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(s.shares), 0) FROM crpcharshares s
		 WHERE s.corporationID = ?
		   AND (s.characterID = ?
		        OR s.characterID IN (SELECT corporationID FROM corporation WHERE ceoID = ?))`,
		corpID, voterID, voterID).Scan(&n)
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(o.qtyOpen), 0) FROM sccOrders o
		 WHERE o.corpID = ? AND o.isBuy = 0 AND o.state = 0
		   AND (o.ownerID = ?
		        OR o.ownerID IN (SELECT corporationID FROM corporation WHERE ceoID = ?))`,
		corpID, voterID, voterID).Scan(&escrowed)
	return n + escrowed
}

// CanCastVote mirrors bco_voting.CanVote: a shareholder (directly or through a
// corporation they run), or the corporation's own CEO.
func CanCastVote(ctx context.Context, db *sql.DB, voterID, corpID int64) bool {
	if ShareVoteWeight(ctx, db, voterID, corpID) > 0 {
		return true
	}
	var ceo int64
	_ = db.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceo)
	return ceo != 0 && ceo == voterID
}

// VoteOptionChange is one option whose share tally moved.
type VoteOptionChange struct {
	VoteCaseID, OptionID int64
	Old, New             int64
}

// RetallyOpenVotes recomputes votesFor, in shares, on every still-open case of
// corpID and returns the options that moved (for OnCorporationVoteCaseOptionChanged).
// Closed cases are never touched: their tally is what it was when voting ended.
func RetallyOpenVotes(ctx context.Context, db *sql.DB, corpID, now int64) ([]VoteOptionChange, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT voteCaseID FROM crpVoteCases WHERE corporationID = ? AND endDateTime > ?`, corpID, now)
	if err != nil {
		return nil, fmt.Errorf("RetallyOpenVotes cases: %w", err)
	}
	var cases []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			cases = append(cases, id)
		}
	}
	rows.Close()

	weights := map[int64]int64{}
	var out []VoteOptionChange
	for _, caseID := range cases {
		sums := map[int64]int64{}
		vrows, err := db.QueryContext(ctx,
			`SELECT characterID, optionID FROM crpVotes WHERE voteCaseID = ?`, caseID)
		if err != nil {
			return out, fmt.Errorf("RetallyOpenVotes votes: %w", err)
		}
		var ballots [][2]int64
		for vrows.Next() {
			var voter, opt int64
			if vrows.Scan(&voter, &opt) == nil {
				ballots = append(ballots, [2]int64{voter, opt})
			}
		}
		vrows.Close()
		for _, b := range ballots {
			w, ok := weights[b[0]]
			if !ok {
				w = ShareVoteWeight(ctx, db, b[0], corpID)
				weights[b[0]] = w
			}
			sums[b[1]] += w
		}

		orows, err := db.QueryContext(ctx,
			`SELECT optionID, votesFor FROM crpVoteOptions WHERE voteCaseID = ?`, caseID)
		if err != nil {
			return out, fmt.Errorf("RetallyOpenVotes options: %w", err)
		}
		var moved []VoteOptionChange
		for orows.Next() {
			var opt, old int64
			if orows.Scan(&opt, &old) == nil && sums[opt] != old {
				moved = append(moved, VoteOptionChange{caseID, opt, old, sums[opt]})
			}
		}
		orows.Close()
		for _, c := range moved {
			if _, err := db.ExecContext(ctx,
				`UPDATE crpVoteOptions SET votesFor = ? WHERE voteCaseID = ? AND optionID = ?`,
				c.New, c.VoteCaseID, c.OptionID); err != nil {
				return out, fmt.Errorf("RetallyOpenVotes update: %w", err)
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// voteTally is one option's result, for pickWinningOption.
type voteTally struct {
	OptionID, VotesFor int64
}

// pickWinningOption returns the option with strictly the most shares. An empty ballot
// or a tie at the top has no winner, so an unvoted case never resolves to option 0,
// the client's "yes" line.
func pickWinningOption(t []voteTally) (int64, bool) {
	var best, bestVotes int64
	found, tie := false, false
	for _, o := range t {
		switch {
		case o.VotesFor > bestVotes:
			best, bestVotes, found, tie = o.OptionID, o.VotesFor, true, false
		case o.VotesFor == bestVotes && o.VotesFor > 0:
			tie = true
		}
	}
	if !found || tie {
		return 0, false
	}
	return best, true
}

// GetMemberIDsWithMoreThanAvgShares lists members holding more of their own corp than an
// even split among its pilots: the kick-vote candidate pool. The treasury is left out of
// the average and the divisor is the real pilot count, not corporation.memberCount.
func GetMemberIDsWithMoreThanAvgShares(ctx context.Context, db *sql.DB, corpID int64) []int64 {
	var distributed, members int64
	_ = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(shares), 0) FROM crpcharshares
		 WHERE corporationID = ? AND characterID <> corporationID`, corpID).Scan(&distributed)
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE corporationID = ? AND accountID > 0`, corpID).Scan(&members)
	if distributed == 0 || members == 0 {
		return nil
	}
	avg := distributed / members
	rows, err := db.QueryContext(ctx, `
		SELECT s.characterID FROM crpcharshares s
		JOIN character_ c ON c.characterID = s.characterID AND c.corporationID = s.corporationID
		WHERE s.corporationID = ? AND s.shares > ?`, corpID, avg)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// CorpShareholder is one row of the ESP shareholder panel.
type CorpShareholder struct {
	HolderID int64
	Name     string
	Kind     string // "treasury", "member", "pilot", "corporation"
	Shares   int64
	Pct      float64 // of issued
	Weight   int64   // what this holder's ballot carries if they are a pilot
}

// ESPCorpShareholders lists every holder of corpID, treasury first. Weight is computed
// for pilots only: a corporation's stake is voted by its CEO and counts in that
// pilot's weight.
func ESPCorpShareholders(ctx context.Context, db *sql.DB, corpID int64) ([]CorpShareholder, int64, error) {
	var issued int64
	if err := db.QueryRowContext(ctx,
		`SELECT shares FROM corporation WHERE corporationID = ?`, corpID).Scan(&issued); err != nil {
		return nil, 0, fmt.Errorf("ESPCorpShareholders issued: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.characterID, s.shares,
		       COALESCE(ch.characterName, co.corporationName, CONCAT('#', s.characterID)),
		       ch.corporationID, co.corporationID IS NOT NULL
		  FROM crpcharshares s
		  LEFT JOIN character_ ch ON ch.characterID = s.characterID
		  LEFT JOIN corporation co ON co.corporationID = s.characterID
		 WHERE s.corporationID = ?
		 ORDER BY (s.characterID = s.corporationID) DESC, s.shares DESC`, corpID)
	if err != nil {
		return nil, issued, fmt.Errorf("ESPCorpShareholders: %w", err)
	}
	var out []CorpShareholder
	for rows.Next() {
		var h CorpShareholder
		var chCorp sql.NullInt64
		var isCorp bool
		if err := rows.Scan(&h.HolderID, &h.Shares, &h.Name, &chCorp, &isCorp); err != nil {
			continue
		}
		switch {
		case h.HolderID == corpID:
			h.Kind = "treasury"
		case isCorp:
			h.Kind = "corporation"
		case chCorp.Valid && chCorp.Int64 == corpID:
			h.Kind = "member"
		default:
			h.Kind = "pilot"
		}
		if issued > 0 {
			h.Pct = float64(h.Shares) * 100 / float64(issued)
		}
		out = append(out, h)
	}
	rows.Close()
	for i := range out {
		if out[i].Kind == "member" || out[i].Kind == "pilot" {
			out[i].Weight = ShareVoteWeight(ctx, db, out[i].HolderID, corpID)
		}
	}
	return out, issued, nil
}

// ShareDelta is one holder's before/after, the unit every share push is
// built from (in-game transfers, issuing, and the ESP GM tools alike).
type ShareDelta struct {
	HolderID, Old, New int64
}

// Deltas returns both sides of a transfer.
func (m ShareMove) Deltas(fromHolder, toHolder int64) []ShareDelta {
	return []ShareDelta{
		{HolderID: fromHolder, Old: m.FromOld, New: m.FromNew},
		{HolderID: toHolder, Old: m.ToOld, New: m.ToNew},
	}
}

// ResolveShareOwner turns an admin's input (an owner ID, or a pilot's or player
// corporation's exact name) into a holder that can own shares.
func ResolveShareOwner(ctx context.Context, db *sql.DB, text string) (int64, error) {
	var id int64
	if _, err := fmt.Sscan(text, &id); err == nil && fmt.Sprint(id) == text {
		if ShareOwnerKind(ctx, db, id) == "" {
			return 0, fmt.Errorf("%d is neither a pilot nor a player corporation", id)
		}
		return id, nil
	}
	if charID, _, err := FindPlayerCharByName(ctx, db, text); err == nil {
		return charID, nil
	}
	err := db.QueryRowContext(ctx, `
		SELECT c.corporationID FROM corporation c
		 WHERE c.corporationName = ?
		   AND NOT EXISTS (SELECT 1 FROM crpNPCCorporations n WHERE n.corporationID = c.corporationID)
		 LIMIT 1`, text).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("no pilot or player corporation named %q", text)
	}
	return id, nil
}

// ─── dividends ─────────────────────────────────────────────────────────────

const (
	// RefCorporationDividendPayment is the journal refType the client's
	// wallet prints as a dividend (util/format.py): the paying corporation's
	// row reads "<corp> paying dividends." when ownerID2 is ownerBank, and
	// each payee's "<payee> receiving dividend payment from <argID1>".
	RefCorporationDividendPayment = 38
	// ownerBank is const.ownerBank, the counterparty the payer's row names.
	ownerBank = 2
)

// DividendPayout is one payee's cut of a dividend.
type DividendPayout struct {
	PayeeID int64
	Amount  float64
}

// DividendPaid is one payee after the payout: IsChar says whose wallet it was
// (a pilot's, or a corporate shareholder's), Balance is its new balance.
type DividendPaid struct {
	PayeeID int64
	Amount  float64
	Balance float64
	IsChar  bool
}

// SplitDividendByShares divides total pro rata over holders, skipping the treasury
// (holder == corpID, the corporation does not pay itself). Each cut is floored to
// whole ISK, so the payout never exceeds total. Pure.
func SplitDividendByShares(total float64, corpID int64, holders []ShareHoldingRow) []DividendPayout {
	var sum int64
	for _, h := range holders {
		if h.HolderID != corpID && h.Shares > 0 {
			sum += h.Shares
		}
	}
	if sum == 0 || total < 1 {
		return nil
	}
	var out []DividendPayout
	for _, h := range holders {
		if h.HolderID == corpID || h.Shares <= 0 {
			continue
		}
		if a := math.Floor(total * float64(h.Shares) / float64(sum)); a >= 1 {
			out = append(out, DividendPayout{h.HolderID, a})
		}
	}
	return out
}

// SplitDividendEqually divides total evenly over payees (the client's
// "members" dividend), floored to whole ISK. Pure.
func SplitDividendEqually(total float64, payees []int64) []DividendPayout {
	if len(payees) == 0 {
		return nil
	}
	a := math.Floor(total / float64(len(payees)))
	if a < 1 {
		return nil
	}
	out := make([]DividendPayout, 0, len(payees))
	for _, id := range payees {
		out = append(out, DividendPayout{id, a})
	}
	return out
}

// PayDividend pays a dividend out of corpID's wallet in one transaction: a conditional
// debit of the whole payout (ErrInsufficientFunds, nothing moved, when the wallet cannot
// cover it), a credit to every payee's wallet and the journal rows the client renders as
// a dividend. Returns the total paid, each payee's new balance and the corporation's.
func PayDividend(ctx context.Context, db *sql.DB, corpID int64, payouts []DividendPayout, reason string) (total float64, paid []DividendPaid, corpBal float64, err error) {
	for _, p := range payouts {
		if p.PayeeID == corpID || p.Amount <= 0 {
			return 0, nil, 0, fmt.Errorf("PayDividend: bad payout %+v", p)
		}
		total += p.Amount
	}
	if total <= 0 {
		return 0, nil, 0, fmt.Errorf("PayDividend: nothing to pay")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("PayDividend begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`UPDATE corporation SET balance = balance - ? WHERE corporationID = ? AND balance >= ?`,
		total, corpID, total)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("PayDividend debit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, nil, 0, ErrInsufficientFunds
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM corporation WHERE corporationID = ?`, corpID).Scan(&corpBal); err != nil {
		return 0, nil, 0, fmt.Errorf("PayDividend balance: %w", err)
	}
	reason = JournalReason(reason)
	now := win32TimeNow()
	arg := strconv.FormatInt(corpID, 10)
	const journal = `INSERT INTO market_journal
		   (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		    accountID, amount, balance, reason, accountKey)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 1000)`
	if _, err := tx.ExecContext(ctx, journal, corpID, now, RefCorporationDividendPayment,
		corpID, ownerBank, arg, -total, corpBal, reason); err != nil {
		return 0, nil, 0, fmt.Errorf("PayDividend corp journal: %w", err)
	}
	for _, p := range payouts {
		table, key, ok, err := walletTable(ctx, tx, p.PayeeID)
		if err != nil {
			return 0, nil, 0, fmt.Errorf("PayDividend payee %d: %w", p.PayeeID, err)
		}
		if !ok {
			return 0, nil, 0, fmt.Errorf("PayDividend: no wallet for payee %d", p.PayeeID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE `+table+` SET balance = balance + ? WHERE `+key+` = ?`, p.Amount, p.PayeeID); err != nil {
			return 0, nil, 0, fmt.Errorf("PayDividend credit %d: %w", p.PayeeID, err)
		}
		d := DividendPaid{PayeeID: p.PayeeID, Amount: p.Amount, IsChar: table == "character_"}
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM `+table+` WHERE `+key+` = ?`, p.PayeeID).Scan(&d.Balance); err != nil {
			return 0, nil, 0, fmt.Errorf("PayDividend payee balance %d: %w", p.PayeeID, err)
		}
		if _, err := tx.ExecContext(ctx, journal, p.PayeeID, now, RefCorporationDividendPayment,
			corpID, p.PayeeID, arg, p.Amount, d.Balance, reason); err != nil {
			return 0, nil, 0, fmt.Errorf("PayDividend payee journal %d: %w", p.PayeeID, err)
		}
		paid = append(paid, d)
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, 0, fmt.Errorf("PayDividend commit: %w", err)
	}
	EmitWorldEvent(CorpDividendPaidEvent{At: time.Now(), CorpID: corpID, Total: total})
	return total, paid, corpBal, nil
}

// tableMissing reports a query against a table this database does not have.
func tableMissing(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "Error 1146") || strings.Contains(err.Error(), "doesn't exist"))
}

// EscrowedShares is how many of corpID's shares holderID has parked behind
// open sell orders.
func EscrowedShares(ctx context.Context, db DBTX, holderID, corpID int64) int64 {
	var n int64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(SUM(qtyOpen), 0) FROM sccOrders
		WHERE corpID = ? AND ownerID = ? AND isBuy = 0 AND state = 0`, corpID, holderID).Scan(&n)
	return n
}

// BeneficialShareHolders is the register as owners see it: the depository row
// replaced by each seller's escrowed shares (the treasury's own asks go back
// to the treasury row). Dividends are paid over this, never over ShareHolders.
func BeneficialShareHolders(ctx context.Context, db DBTX, corpID int64) ([]ShareHoldingRow, error) {
	held, err := ShareHolders(ctx, db, corpID)
	if err != nil {
		return nil, err
	}
	by := map[int64]int64{}
	var order []int64
	add := func(h, n int64) {
		if _, ok := by[h]; !ok {
			order = append(order, h)
		}
		by[h] += n
	}
	for _, r := range held {
		if r.HolderID != DepositoryHolderID {
			add(r.HolderID, r.Shares)
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT ownerID, SUM(qtyOpen) FROM sccOrders
		WHERE corpID = ? AND isBuy = 0 AND state = 0 GROUP BY ownerID`, corpID)
	if err != nil && !tableMissing(err) {
		return nil, fmt.Errorf("BeneficialShareHolders: %w", err)
	}
	if err == nil {
		for rows.Next() {
			var h, n int64
			if rows.Scan(&h, &n) == nil && n > 0 {
				add(h, n)
			}
		}
		rows.Close()
	}
	out := make([]ShareHoldingRow, 0, len(order))
	for _, h := range order {
		if by[h] > 0 {
			out = append(out, ShareHoldingRow{HolderID: h, Shares: by[h]})
		}
	}
	sortHoldings(out)
	return out, nil
}

func sortHoldings(r []ShareHoldingRow) {
	for i := 1; i < len(r); i++ {
		for j := i; j > 0 && (r[j].Shares > r[j-1].Shares ||
			r[j].Shares == r[j-1].Shares && r[j].HolderID < r[j-1].HolderID); j-- {
			r[j], r[j-1] = r[j-1], r[j]
		}
	}
}
