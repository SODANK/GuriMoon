package db

// Alliance logo slots. The client draws an alliance logo from a literal allianceID dict in
// ui/util/uix.py GetAllianceLogo and draws a fallback for any other ID, so the ID chosen at
// creation is the logo. See docs/db-13-notes.md#alliance-logo-slots.

// AllianceLogoSlot is one reachable logo: the allianceID that selects it and
// the atlas tile it draws.
type AllianceLogoSlot struct {
	ID   int64
	Tile string // "AA_T": atlas allianceAA.dds, tile T (1=TL, 2=TR, 3=BL, 4=BR)
}

// AllianceLogoSlots are the 70 IDs the client maps to a logo, ordered by tile.
var AllianceLogoSlots = []AllianceLogoSlot{
	{ID: 1974662341, Tile: "01_02"},
	{ID: 1967302747, Tile: "01_04"},
	{ID: 1965875958, Tile: "02_02"},
	{ID: 1962981871, Tile: "02_03"},
	{ID: 1962870426, Tile: "02_04"},
	{ID: 1962835064, Tile: "03_02"},
	{ID: 1988009450, Tile: "03_03"},
	{ID: 1988009449, Tile: "03_04"},
	{ID: 1966049571, Tile: "04_01"},
	{ID: 1963166628, Tile: "04_02"},
	{ID: 2031941852, Tile: "04_03"},
	{ID: 1988009451, Tile: "04_04"},
	{ID: 1962872433, Tile: "05_01"},
	{ID: 937038506, Tile: "05_02"},
	{ID: 1439416485, Tile: "05_03"},
	{ID: 1374837394, Tile: "05_04"},
	{ID: 1099063355, Tile: "06_02"},
	{ID: 1041482450, Tile: "06_03"},
	{ID: 159826257, Tile: "06_04"},
	{ID: 342755901, Tile: "07_01"},
	{ID: 1026886508, Tile: "07_02"},
	{ID: 1220922756, Tile: "07_03"},
	{ID: 402446730, Tile: "07_04"},
	{ID: 537119222, Tile: "08_01"},
	{ID: 995013977, Tile: "08_02"},
	{ID: 239739827, Tile: "08_03"},
	{ID: 414616857, Tile: "08_04"},
	{ID: 213401527, Tile: "09_01"},
	{ID: 173739862, Tile: "09_02"},
	{ID: 1441788490, Tile: "09_03"},
	{ID: 978733650, Tile: "10_01"},
	{ID: 264952485, Tile: "10_02"},
	{ID: 373024435, Tile: "10_03"},
	{ID: 521381722, Tile: "10_04"},
	{ID: 370531594, Tile: "11_01"},
	{ID: 628083666, Tile: "11_02"},
	{ID: 881159574, Tile: "11_03"},
	{ID: 937904219, Tile: "11_04"},
	{ID: 755412150, Tile: "12_01"},
	{ID: 898521033, Tile: "12_02"},
	{ID: 693222392, Tile: "12_03"},
	{ID: 893275782, Tile: "12_04"},
	{ID: 741557221, Tile: "13_01"},
	{ID: 327815755, Tile: "13_02"},
	{ID: 295773986, Tile: "13_03"},
	{ID: 937099054, Tile: "14_01"},
	{ID: 618162213, Tile: "14_02"},
	{ID: 613888047, Tile: "14_03"},
	{ID: 632866070, Tile: "14_04"},
	{ID: 790581431, Tile: "15_01"},
	{ID: 701459600, Tile: "15_02"},
	{ID: 900209169, Tile: "15_03"},
	{ID: 898789246, Tile: "15_04"},
	{ID: 759477551, Tile: "16_01"},
	{ID: 1327233264, Tile: "16_02"},
	{ID: 794831862, Tile: "16_03"},
	{ID: 735932392, Tile: "16_04"},
	{ID: 830660878, Tile: "17_01"},
	{ID: 922541302, Tile: "17_02"},
	{ID: 845358580, Tile: "17_03"},
	{ID: 830476259, Tile: "17_04"},
	{ID: 294099361, Tile: "18_01"},
	{ID: 225947254, Tile: "18_02"},
	{ID: 873672554, Tile: "18_03"},
	{ID: 820673945, Tile: "18_04"},
	{ID: 780843225, Tile: "19_01"},
	{ID: 693547580, Tile: "19_02"},
	{ID: 521550159, Tile: "19_03"},
	{ID: 980676917, Tile: "19_04"},
	{ID: 819637246, Tile: "20_01"},
}

// AllianceLogoTile returns the atlas tile an allianceID draws in the client,
// and whether the ID is a real slot.  An unmapped ID draws "01_01".
func AllianceLogoTile(allianceID int64) (string, bool) {
	for _, s := range AllianceLogoSlots {
		if s.ID == allianceID {
			return s.Tile, true
		}
	}
	return "01_01", false
}
