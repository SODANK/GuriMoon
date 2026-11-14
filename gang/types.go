package gang

// GangCorpEntry holds a gang-corp association for SetState wire format.
type GangCorpEntry struct {
	GangID int64
	CorpID int64
}

// CharGangEntry holds a char-gang association for SetState wire format.
type CharGangEntry struct {
	CharID int64
	GangID int64
}
