package contentpkg

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// bountyAttr is the dogma attribute holding an NPC's kill bounty.
const bountyAttr = 481

// review summarises what a package pays out, so the admin can spot an ISK or item faucet
// before installing: the largest mission rewards, the highest custom NPC bounty, every item it
// can hand out with its largest quantity, and how many live sites its spawners keep.
func review(ctx context.Context, q queryer, p *Package) []string {
	var out []string

	var isk, bonus, lp int64
	for _, m := range p.Missions {
		isk = max(isk, intOf(m["baseRewardISK"]))
		bonus = max(bonus, intOf(m["baseBonusISK"]))
		lp = max(lp, intOf(m["baseRewardLP"]))
	}
	if len(p.Missions) > 0 {
		out = append(out, fmt.Sprintf("Missions pay at most %s ISK, %s ISK time bonus and %s LP each.",
			commas(isk), commas(bonus), commas(lp)))
	}

	var bounty float64
	bountyName := ""
	for _, ct := range p.Types {
		for _, a := range ct.Attributes {
			if intOf(a["attributeID"]) == bountyAttr {
				b := floatOf(a["valueFloat"])
				if a["valueFloat"] == nil {
					b = float64(intOf(a["valueInt"]))
				}
				if b > bounty {
					bounty, bountyName = b, asString(ct.Type["typeName"])
				}
			}
		}
	}
	if bountyName != "" {
		out = append(out, fmt.Sprintf("Highest custom NPC bounty: %s ISK (%s), before the server's bounty rate.",
			commas(int64(bounty)), bountyName))
	}

	// Items handed out, with the largest quantity of each.
	items := map[string]int64{}
	add := func(typeVal, qty any) {
		if typeVal == nil {
			return
		}
		k := asString(typeVal)
		items[k] = max(items[k], max(intOf(qty), 1))
	}
	for _, d := range p.Dungeons {
		for _, r := range d.Loot {
			add(r["itemTypeID"], r["maxQty"])
		}
	}
	for _, ct := range p.Types {
		for _, r := range ct.Loot {
			add(r["itemTypeID"], r["maxQty"])
		}
	}
	for _, m := range p.Missions {
		add(m["baseRewardItemTypeID"], m["baseRewardItemQty"])
		add(m["bonusRewardItemTypeID"], m["bonusRewardItemQty"])
	}
	if len(items) > 0 {
		var ids []int64
		pkgNames := map[string]string{}
		for _, ct := range p.Types {
			pkgNames[refValue(ct.Type.Key())] = asString(ct.Type["typeName"])
		}
		for k := range items {
			if id, err := strconv.ParseInt(k, 10, 64); err == nil {
				ids = append(ids, id)
			}
		}
		names := lookupNames(ctx, q, "invTypes", "typeID", "typeName", ids)
		var list []string
		for k, n := range items {
			name := pkgNames[k]
			if id, err := strconv.ParseInt(k, 10, 64); err == nil {
				name = names[id]
				if name == "" {
					name = "type " + k
				}
			}
			list = append(list, fmt.Sprintf("%s ×%s", name, commas(n)))
		}
		sort.Strings(list)
		const show = 40
		more := ""
		if len(list) > show {
			more = fmt.Sprintf(" and %d more", len(list)-show)
			list = list[:show]
		}
		out = append(out, fmt.Sprintf("Items it can hand out (largest quantity): %s%s.", strings.Join(list, ", "), more))
	}

	var sites int64
	for _, s := range p.Spawners {
		sites += intOf(s["targetActive"])
	}
	if len(p.Spawners) > 0 {
		out = append(out, fmt.Sprintf("Spawners keep up to %s live sites across the map.", commas(sites)))
	}
	return out
}

// commas formats an integer with thousands separators.
func commas(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}
