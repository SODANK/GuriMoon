package db

// Battle reports (the related-kills view): kills in one system in a run of
// closely spaced losses form a battle, and its parties are split into two teams
// from the attack graph. Nothing is stored; a battle is recomputed per request.
// See docs/db-9-notes.md#battle-reports.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	settingBattleGapMinutes   = "battle.gapMinutes"
	settingBattleMaxSpanHours = "battle.maxSpanHours"
	settingBattleMinKills     = "battle.minKills"

	DefaultBattleGapMinutes   = 20
	DefaultBattleMaxSpanHours = 6
	DefaultBattleMinKills     = 4

	// BattleMaxKills bounds one battle. A cluster that reaches it is closed and the
	// next kill opens a new one, since the report loads every attacker of every kill.
	BattleMaxKills = 500

	// battleListScan caps the kills read per candidate system, battleListSystems
	// how many busy systems are read, and battleListCandidates how many clusters
	// are built in full to confirm two sides.
	battleListScan       = 3000
	battleListSystems    = 60
	battleListCandidates = 40

	// firstPlayerCorpID is where corporations stop being NPC; player corps come
	// from a counter starting at 2,000,000.
	firstPlayerCorpID = 2000000
)

// BattleOptions are the tunables, read from server_settings.
type BattleOptions struct {
	Gap      time.Duration // longest silence inside one battle
	MaxSpan  time.Duration // longest a single battle may run
	MinKills int           // fewest kills the list calls a battle
}

// LoadBattleOptions reads the battle.* settings, falling back to defaults.
func LoadBattleOptions(ctx context.Context, db *sql.DB) BattleOptions {
	return BattleOptions{
		Gap:      time.Duration(battleSettingInt(ctx, db, settingBattleGapMinutes, DefaultBattleGapMinutes, 1)) * time.Minute,
		MaxSpan:  time.Duration(battleSettingInt(ctx, db, settingBattleMaxSpanHours, DefaultBattleMaxSpanHours, 1)) * time.Hour,
		MinKills: battleSettingInt(ctx, db, settingBattleMinKills, DefaultBattleMinKills, 2),
	}
}

func battleSettingInt(ctx context.Context, db *sql.DB, name string, def, min int) int {
	v, ok, err := GetServerSetting(ctx, db, name)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < min {
		return def
	}
	return n
}

// ---- clustering ----

// ClusterBattleKills splits kills into battles: per solar system, a kill joins
// the open battle if it follows the previous kill within gap, the battle has not
// run past maxSpan, and it is not full. Newest battle first, ties by system.
// Each cluster is returned oldest kill first.
func ClusterBattleKills(rows []KillmailRow, gap, maxSpan time.Duration) [][]KillmailRow {
	bySystem := map[int64][]KillmailRow{}
	for _, k := range rows {
		bySystem[k.SolarSystemID] = append(bySystem[k.SolarSystemID], k)
	}
	var out [][]KillmailRow
	for _, list := range bySystem {
		sort.Slice(list, func(i, j int) bool {
			if !list[i].KillTime.Equal(list[j].KillTime) {
				return list[i].KillTime.Before(list[j].KillTime)
			}
			return list[i].KillID < list[j].KillID
		})
		var cur []KillmailRow
		for _, k := range list {
			if len(cur) > 0 {
				prev, first := cur[len(cur)-1], cur[0]
				if k.KillTime.Sub(prev.KillTime) > gap || k.KillTime.Sub(first.KillTime) > maxSpan ||
					len(cur) >= BattleMaxKills {
					out = append(out, cur)
					cur = nil
				}
			}
			cur = append(cur, k)
		}
		if len(cur) > 0 {
			out = append(out, cur)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i][0].KillTime, out[j][0].KillTime
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i][0].SolarSystemID < out[j][0].SolarSystemID
	})
	return out
}

// BattleContaining returns the cluster that holds killID, or nil.
func BattleContaining(clusters [][]KillmailRow, killID int64) []KillmailRow {
	for _, c := range clusters {
		for _, k := range c {
			if k.KillID == killID {
				return c
			}
		}
	}
	return nil
}

// BattleKillsAround loads the battle a kill belongs to: the kills in its system
// within maxSpan either side, chained by the gap rule. Nil when the kill does
// not exist or HideBots filters it out.
func BattleKillsAround(ctx context.Context, db *sql.DB, killID int64, opt BattleOptions, includeBots bool) ([]KillmailRow, error) {
	seed, err := GetKillmail(ctx, db, killID)
	if err != nil || seed == nil {
		return nil, err
	}
	if seed.VictimIsBot && !includeBots {
		return nil, nil
	}
	rows, err := ListKillmails(ctx, db, KillmailFilter{
		SolarSystemID: seed.SolarSystemID,
		Since:         seed.KillTime.Add(-opt.MaxSpan),
		Until:         seed.KillTime.Add(opt.MaxSpan),
		HideBots:      !includeBots,
		Limit:         BattleMaxKills * 4,
	})
	if err != nil {
		return nil, err
	}
	// The seed may be an NPC-only kill the list filter drops; it stays the anchor.
	have := false
	for _, k := range rows {
		if k.KillID == killID {
			have = true
			break
		}
	}
	if !have {
		rows = append(rows, seed.KillmailRow)
	}
	return BattleContaining(ClusterBattleKills(rows, opt.Gap, opt.MaxSpan), killID), nil
}

// ---- loading ----

func battleInList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	ph := make([]string, len(ids))
	for i, id := range ids {
		args[i], ph[i] = id, "?"
	}
	return "(" + strings.Join(ph, ",") + ")", args
}

// ListKillmailAttackers returns the stored attackers of many kills in as few
// queries as the IN-list allows, keyed by killID, each in seq order.
func ListKillmailAttackers(ctx context.Context, db *sql.DB, killIDs []int64) (map[int64][]KillmailAttackerRow, error) {
	out := make(map[int64][]KillmailAttackerRow, len(killIDs))
	const chunk = 500
	for lo := 0; lo < len(killIDs); lo += chunk {
		hi := min(lo+chunk, len(killIDs))
		in, args := battleInList(killIDs[lo:hi])
		rows, err := db.QueryContext(ctx, `
			SELECT killID, seq, charID, name, corpID, corpName, allianceID, allianceName,
			       secStatus, shipTypeID, shipTypeName, weaponTypeID, weaponName,
			       damage, isNPC, finalBlow, lastHit
			  FROM killmail_attackers WHERE killID IN `+in+` ORDER BY killID, seq`, args...)
		if err != nil {
			return nil, fmt.Errorf("ListKillmailAttackers: %w", err)
		}
		for rows.Next() {
			var id int64
			var a KillmailAttackerRow
			var npc, fb int
			var last int64
			if err := rows.Scan(&id, &a.Seq, &a.CharID, &a.Name, &a.CorpID, &a.CorpName, &a.AllianceID, &a.AllianceName,
				&a.SecStatus, &a.ShipTypeID, &a.ShipTypeName, &a.WeaponTypeID, &a.WeaponName,
				&a.Damage, &npc, &fb, &last); err != nil {
				rows.Close()
				return nil, fmt.Errorf("ListKillmailAttackers scan: %w", err)
			}
			a.IsNPC, a.FinalBlow, a.LastHit = npc != 0, fb != 0, time.Unix(last, 0)
			out[id] = append(out[id], a)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// KillmailShipClasses names the hull class (invGroups.groupName) each victim
// ship belonged to, keyed by killID. The group snapshotted on the killmail wins;
// an old row without one falls back to the live type.
func KillmailShipClasses(ctx context.Context, db *sql.DB, killIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(killIDs))
	const chunk = 500
	for lo := 0; lo < len(killIDs); lo += chunk {
		hi := min(lo+chunk, len(killIDs))
		in, args := battleInList(killIDs[lo:hi])
		rows, err := db.QueryContext(ctx, `
			SELECT k.killID, COALESCE(g.groupName, '')
			  FROM killmails k
			  LEFT JOIN invTypes  t ON t.typeID  = k.shipTypeID
			  LEFT JOIN invGroups g ON g.groupID = COALESCE(NULLIF(k.shipGroupID, 0), t.groupID)
			 WHERE k.killID IN `+in, args...)
		if err != nil {
			return nil, fmt.Errorf("KillmailShipClasses: %w", err)
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, fmt.Errorf("KillmailShipClasses scan: %w", err)
			}
			out[id] = name
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// LoadBattle assembles the full report for one cluster of kills.
func LoadBattle(ctx context.Context, db *sql.DB, kills []KillmailRow) (*Battle, error) {
	if len(kills) == 0 {
		return nil, nil
	}
	ids := make([]int64, len(kills))
	for i, k := range kills {
		ids[i] = k.KillID
	}
	atk, err := ListKillmailAttackers(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	classes, err := KillmailShipClasses(ctx, db, ids)
	if err != nil {
		return nil, err
	}
	details := make([]KillmailDetail, len(kills))
	for i, k := range kills {
		details[i] = KillmailDetail{KillmailRow: k, Attackers: atk[k.KillID]}
	}
	return BuildBattle(details, classes), nil
}

// ---- the model ----

// BattleNode is one corporation or alliance on a team.
type BattleNode struct {
	Alliance   bool
	ID         int64
	Name       string
	LogoCorpID int64 // the member corporation with the most capsuleers here
	Pilots     int
}

// BattleLoss is one ship a capsuleer lost.
type BattleLoss struct {
	KillID     int64
	ShipTypeID int64
	ShipName   string
	IsPod      bool
	Value      float64
}

// BattlePilot is one capsuleer on a team.
type BattlePilot struct {
	CharID       int64
	Name         string
	CorpID       int64
	CorpName     string
	AllianceID   int64
	AllianceName string
	ShipTypeID   int64 // the hull flown
	ShipName     string
	Losses       []BattleLoss
	Damage       float64
	LostISK      float64
}

// Died reports whether the capsuleer lost anything in the battle.
func (p BattlePilot) Died() bool { return len(p.Losses) > 0 }

// BattleClass is one row of a team's ship-class table.
type BattleClass struct {
	Name string
	Lost int
	ISK  float64
}

// BattleTeam is one side of the fight.
type BattleTeam struct {
	Nodes      []BattleNode
	Pilots     []BattlePilot
	Classes    []BattleClass
	ShipsLost  int
	ISKLost    float64
	ISKDropped float64 // the part of ISKLost that survived into wrecks
}

// Involved is the number of distinct capsuleers on the team.
func (t BattleTeam) Involved() int { return len(t.Pilots) }

// Title names the team after its biggest node.
func (t BattleTeam) Title() string {
	if len(t.Nodes) == 0 {
		return "Unknown"
	}
	return t.Nodes[0].Name
}

// Battle is one reconstructed fight. Teams[0] is the side that took the single
// heaviest loss; the order is otherwise arbitrary but stable.
type Battle struct {
	SeedKillID      int64
	SystemID        int64
	SystemName      string
	Security        float64
	RegionName      string
	Start, End      time.Time
	Kills           int // every kill in the cluster, aligned or not
	Teams           [2]BattleTeam
	UninvolvedKills int // kills whose victim fell outside both teams
	NoAttackerKills int // kills filed with no attacker rows (retention, or a bare record)
}

// Valid reports whether this is really a fight: both sides hold a capsuleer and
// at least one ship was lost.
func (b *Battle) Valid() bool {
	return b != nil && b.Teams[0].Involved() > 0 && b.Teams[1].Involved() > 0 &&
		b.Teams[0].ShipsLost+b.Teams[1].ShipsLost > 0
}

// ISKLost is both sides' loss together.
func (b *Battle) ISKLost() float64 { return b.Teams[0].ISKLost + b.Teams[1].ISKLost }

// ShipsLost is both sides' losses together.
func (b *Battle) ShipsLost() int { return b.Teams[0].ShipsLost + b.Teams[1].ShipsLost }

// Involved is every capsuleer on either side.
func (b *Battle) Involved() int { return b.Teams[0].Involved() + b.Teams[1].Involved() }

// ---- team building ----

type nodeKind int

const (
	nodeAlliance nodeKind = iota
	nodeCorp
	nodeChar
)

type nodeKey struct {
	kind nodeKind
	id   int64
}

func (a nodeKey) less(b nodeKey) bool {
	if a.kind != b.kind {
		return a.kind < b.kind
	}
	return a.id < b.id
}

func battleNodeOf(charID, corpID, allianceID int64) nodeKey {
	switch {
	case allianceID != 0:
		return nodeKey{nodeAlliance, allianceID}
	case corpID >= firstPlayerCorpID:
		return nodeKey{nodeCorp, corpID}
	default:
		return nodeKey{nodeChar, charID}
	}
}

type nodeInfo struct {
	key      nodeKey
	name     string
	corpMen  map[int64]int // corpID to capsuleers seen
	pilots   map[int64]struct{}
	iskLost  float64
	degree   float64
	side     int
	assigned bool
}

// BuildBattle splits the kills' parties into two teams. classOf maps killID to
// the victim's hull class; a missing entry reads as "Unknown".
func BuildBattle(kills []KillmailDetail, classOf map[int64]string) *Battle {
	b := &Battle{Kills: len(kills)}
	if len(kills) == 0 {
		return b
	}

	ordered := append([]KillmailDetail(nil), kills...)
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].KillTime.Equal(ordered[j].KillTime) {
			return ordered[i].KillTime.Before(ordered[j].KillTime)
		}
		return ordered[i].KillID < ordered[j].KillID
	})

	head := ordered[0]
	b.SystemID, b.SystemName, b.Security, b.RegionName = head.SolarSystemID, head.SystemName, head.Security, head.RegionName
	b.Start, b.End = head.KillTime, ordered[len(ordered)-1].KillTime
	var bestVal float64 = -1
	for _, k := range ordered {
		if k.TotalValue > bestVal {
			bestVal, b.SeedKillID = k.TotalValue, k.KillID
		}
	}

	nodes := map[nodeKey]*nodeInfo{}
	pilots := map[int64]*BattlePilot{}
	pilotNode := map[int64]nodeKey{}
	var pilotOrder []int64
	bestHit := map[int64]float64{}

	node := func(key nodeKey, name string) *nodeInfo {
		n := nodes[key]
		if n == nil {
			n = &nodeInfo{key: key, corpMen: map[int64]int{}, pilots: map[int64]struct{}{}}
			nodes[key] = n
		}
		if n.name == "" {
			n.name = name
		}
		return n
	}
	// register files a capsuleer the first time they are seen. Their node is the
	// one they were first seen under, so a mid-fight corporation change cannot
	// split them across teams.
	register := func(charID int64, name string, corpID int64, corpName string, allianceID int64, allianceName string) *BattlePilot {
		if p := pilots[charID]; p != nil {
			if p.Name == "" {
				p.Name = name
			}
			return p
		}
		p := &BattlePilot{CharID: charID, Name: name, CorpID: corpID, CorpName: corpName,
			AllianceID: allianceID, AllianceName: allianceName}
		pilots[charID] = p
		pilotOrder = append(pilotOrder, charID)
		key := battleNodeOf(charID, corpID, allianceID)
		pilotNode[charID] = key
		label := corpName
		switch key.kind {
		case nodeAlliance:
			label = allianceName
		case nodeChar:
			label = name
		}
		n := node(key, label)
		n.pilots[charID] = struct{}{}
		if corpID != 0 {
			n.corpMen[corpID]++
		}
		return p
	}

	// weights is the undirected hostile weight between two nodes.
	weights := map[nodeKey]map[nodeKey]float64{}
	link := func(a, c nodeKey, w float64) {
		if weights[a] == nil {
			weights[a] = map[nodeKey]float64{}
		}
		if weights[c] == nil {
			weights[c] = map[nodeKey]float64{}
		}
		weights[a][c] += w
		weights[c][a] += w
	}

	type lossAt struct {
		km   *KillmailDetail
		node nodeKey
	}
	var losses []lossAt
	for i := range ordered {
		km := &ordered[i]
		var victim *BattlePilot
		if km.VictimCharID != 0 {
			victim = register(km.VictimCharID, km.VictimName, km.VictimCorpID, km.VictimCorpName,
				km.VictimAllianceID, km.VictimAllianceName)
			vn := pilotNode[km.VictimCharID]
			victim.Losses = append(victim.Losses, BattleLoss{KillID: km.KillID, ShipTypeID: km.ShipTypeID,
				ShipName: km.ShipTypeName, IsPod: km.IsPod, Value: km.TotalValue})
			victim.LostISK += km.TotalValue
			nodes[vn].iskLost += km.TotalValue
			losses = append(losses, lossAt{km, vn})
		}
		if len(km.Attackers) == 0 {
			b.NoAttackerKills++
		}
		for _, a := range km.Attackers {
			if a.IsNPC || a.CharID == 0 {
				continue
			}
			p := register(a.CharID, a.Name, a.CorpID, a.CorpName, a.AllianceID, a.AllianceName)
			p.Damage += a.Damage
			// The hull flown is the one behind their heaviest hit; a ship they lost
			// overrides it when the teams are written out.
			if a.ShipTypeID != 0 && (p.ShipTypeID == 0 || a.Damage > bestHit[a.CharID]) {
				p.ShipTypeID, p.ShipName = a.ShipTypeID, a.ShipTypeName
				bestHit[a.CharID] = a.Damage
			}
			if victim != nil {
				an, vn := pilotNode[a.CharID], pilotNode[km.VictimCharID]
				if an != vn {
					link(an, vn, max(a.Damage, 1))
				}
			}
		}
	}

	// Partition.
	keys := make([]nodeKey, 0, len(nodes))
	for k := range nodes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].less(keys[j]) })
	for _, k := range keys {
		for _, w := range weights[k] {
			nodes[k].degree += w
		}
	}

	var seed *nodeInfo
	for _, k := range keys {
		n := nodes[k]
		if n.degree == 0 {
			continue
		}
		if seed == nil || n.iskLost > seed.iskLost || (n.iskLost == seed.iskLost && n.degree > seed.degree) {
			seed = n
		}
	}
	if seed == nil {
		b.UninvolvedKills = len(losses)
		return b
	}
	seed.side, seed.assigned = 0, true
	for {
		var best *nodeInfo
		var bestW float64
		for _, k := range keys {
			n := nodes[k]
			if n.assigned {
				continue
			}
			var w float64
			for o, ow := range weights[k] {
				if nodes[o].assigned {
					w += ow
				}
			}
			if w > bestW {
				best, bestW = n, w
			}
		}
		if best == nil {
			break
		}
		var w [2]float64
		for o, ow := range weights[best.key] {
			if nodes[o].assigned {
				w[nodes[o].side] += ow
			}
		}
		// Sit opposite whichever side this node fought harder; a tie goes to 1.
		if w[0] >= w[1] {
			best.side = 1
		} else {
			best.side = 0
		}
		best.assigned = true
	}
	// Local search: flip any node that fought its own side more than the other.
	for pass := 0; pass < 10; pass++ {
		changed := false
		for _, k := range keys {
			n := nodes[k]
			if !n.assigned {
				continue
			}
			var same, cross float64
			for o, ow := range weights[k] {
				if !nodes[o].assigned {
					continue
				}
				if nodes[o].side == n.side {
					same += ow
				} else {
					cross += ow
				}
			}
			if same > cross {
				n.side = 1 - n.side
				changed = true
			}
		}
		if !changed {
			break
		}
	}
	// Team 0 is the side of the heaviest single loss, after local search.
	flip := 0
	if seed.assigned && seed.side == 1 {
		flip = 1
	}
	sideOf := func(k nodeKey) (int, bool) {
		n := nodes[k]
		if n == nil || !n.assigned {
			return 0, false
		}
		return n.side ^ flip, true
	}

	// Teams.
	classAgg := [2]map[string]*BattleClass{{}, {}}
	for _, id := range pilotOrder {
		p := pilots[id]
		s, ok := sideOf(pilotNode[id])
		if !ok {
			continue
		}
		// A capsuleer who died flew what they died in; a pod alone leaves the hull
		// unknown, so the attacker's hull is kept if there was one.
		for _, l := range p.Losses {
			if !l.IsPod {
				p.ShipTypeID, p.ShipName = l.ShipTypeID, l.ShipName
				break
			}
		}
		if p.ShipTypeID == 0 && len(p.Losses) > 0 {
			p.ShipTypeID, p.ShipName = p.Losses[0].ShipTypeID, p.Losses[0].ShipName
		}
		b.Teams[s].Pilots = append(b.Teams[s].Pilots, *p)
	}
	for _, l := range losses {
		s, ok := sideOf(l.node)
		if !ok {
			b.UninvolvedKills++
			continue
		}
		t := &b.Teams[s]
		t.ShipsLost++
		t.ISKLost += l.km.TotalValue
		t.ISKDropped += l.km.DroppedValue
		cls := classOf[l.km.KillID]
		if cls == "" {
			cls = "Unknown"
		}
		c := classAgg[s][cls]
		if c == nil {
			c = &BattleClass{Name: cls}
			classAgg[s][cls] = c
		}
		c.Lost++
		c.ISK += l.km.TotalValue
	}
	for s := 0; s < 2; s++ {
		t := &b.Teams[s]
		for _, c := range classAgg[s] {
			t.Classes = append(t.Classes, *c)
		}
		sort.Slice(t.Classes, func(i, j int) bool {
			if t.Classes[i].ISK != t.Classes[j].ISK {
				return t.Classes[i].ISK > t.Classes[j].ISK
			}
			return t.Classes[i].Name < t.Classes[j].Name
		})
		sort.SliceStable(t.Pilots, func(i, j int) bool {
			pi, pj := t.Pilots[i], t.Pilots[j]
			if pi.Died() != pj.Died() {
				return pi.Died()
			}
			if pi.Died() && pi.LostISK != pj.LostISK {
				return pi.LostISK > pj.LostISK
			}
			if pi.Damage != pj.Damage {
				return pi.Damage > pj.Damage
			}
			return pi.CharID < pj.CharID
		})
		// Nodes, biggest first.
		for _, k := range keys {
			n := nodes[k]
			if ns, ok := sideOf(k); !ok || ns != s {
				continue
			}
			bn := BattleNode{Alliance: k.kind == nodeAlliance, ID: k.id, Name: n.name, Pilots: len(n.pilots)}
			var most int
			for corp, c := range n.corpMen {
				if c > most || (c == most && corp < bn.LogoCorpID) {
					most, bn.LogoCorpID = c, corp
				}
			}
			if k.kind == nodeChar {
				bn.LogoCorpID = 0
			}
			t.Nodes = append(t.Nodes, bn)
		}
		sort.SliceStable(t.Nodes, func(i, j int) bool { return t.Nodes[i].Pilots > t.Nodes[j].Pilots })
	}
	return b
}

// ---- the list ----

// BattleListQuery narrows the list. Window is how far back to look.
type BattleListQuery struct {
	Window      time.Duration
	RegionID    int64
	SystemID    int64
	IncludeBots bool
}

// BattleSummary is one line of the list.
type BattleSummary struct {
	SeedKillID int64
	SystemID   int64
	SystemName string
	Security   float64
	RegionName string
	Start, End time.Time
	Kills      int
	Involved   [2]int
	ShipsLost  [2]int
	ISKLost    [2]float64
	Lead       [2]string
}

var battleListCache = struct {
	sync.Mutex
	m map[BattleListQuery]battleListEntry
}{m: map[BattleListQuery]battleListEntry{}}

type battleListEntry struct {
	at   time.Time
	list []BattleSummary
}

const battleListTTL = 60 * time.Second

// ListBattles finds recent fights: it clusters the window's kills, takes the
// newest clusters that are big enough, builds each in full and keeps those with
// two sides. Cached for a minute (it costs a query per battle).
func ListBattles(ctx context.Context, db *sql.DB, q BattleListQuery, limit int) ([]BattleSummary, error) {
	if limit <= 0 {
		limit = 25
	}
	battleListCache.Lock()
	if e, ok := battleListCache.m[q]; ok && time.Since(e.at) < battleListTTL {
		battleListCache.Unlock()
		return clipBattles(e.list, limit), nil
	}
	battleListCache.Unlock()

	opt := LoadBattleOptions(ctx, db)
	since := time.Now().Add(-q.Window)
	systems, err := battleCandidateSystems(ctx, db, since, q, opt.MinKills)
	if err != nil {
		return nil, err
	}
	// Read the busy systems one at a time: a single newest-first scan over the
	// window would be consumed by one noisy system.
	var rows []KillmailRow
	for _, sys := range systems {
		got, err := ListKillmails(ctx, db, KillmailFilter{
			Since: since, SolarSystemID: sys, HideBots: !q.IncludeBots, Limit: battleListScan,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	var out []BattleSummary
	built := 0
	for _, c := range ClusterBattleKills(rows, opt.Gap, opt.MaxSpan) {
		if len(c) < opt.MinKills {
			continue
		}
		if built >= battleListCandidates {
			break
		}
		built++
		bt, err := LoadBattle(ctx, db, c)
		if err != nil {
			return nil, err
		}
		if !bt.Valid() {
			continue
		}
		s := BattleSummary{SeedKillID: bt.SeedKillID, SystemID: bt.SystemID, SystemName: bt.SystemName,
			Security: bt.Security, RegionName: bt.RegionName, Start: bt.Start, End: bt.End, Kills: bt.Kills}
		for i := 0; i < 2; i++ {
			s.Involved[i], s.ShipsLost[i], s.ISKLost[i], s.Lead[i] =
				bt.Teams[i].Involved(), bt.Teams[i].ShipsLost, bt.Teams[i].ISKLost, bt.Teams[i].Title()
		}
		out = append(out, s)
	}

	battleListCache.Lock()
	if len(battleListCache.m) > 64 {
		battleListCache.m = map[BattleListQuery]battleListEntry{}
	}
	battleListCache.m[q] = battleListEntry{at: time.Now(), list: out}
	battleListCache.Unlock()
	return clipBattles(out, limit), nil
}

// battleCandidateSystems are the systems with at least minKills losses in the
// window, busiest first. A pre-filter only; clustering decides what is a battle.
func battleCandidateSystems(ctx context.Context, db *sql.DB, since time.Time, q BattleListQuery, minKills int) ([]int64, error) {
	where := "killTime >= ? AND npcOnly = 0"
	args := []any{since.Unix()}
	if !q.IncludeBots {
		where += " AND victimIsBot = 0"
	}
	if q.RegionID != 0 {
		where += " AND regionID = ?"
		args = append(args, q.RegionID)
	}
	if q.SystemID != 0 {
		where += " AND solarSystemID = ?"
		args = append(args, q.SystemID)
	}
	args = append(args, minKills, battleListSystems)
	rows, err := db.QueryContext(ctx,
		`SELECT solarSystemID FROM killmails WHERE `+where+
			` GROUP BY solarSystemID HAVING COUNT(*) >= ? ORDER BY COUNT(*) DESC, solarSystemID LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("battleCandidateSystems: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func clipBattles(l []BattleSummary, n int) []BattleSummary {
	if len(l) > n {
		return l[:n]
	}
	return l
}
