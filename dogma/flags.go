package dogma

// Inventory flag constants (entity.flag column / invFlags).
const (
	FlagHangar       int64 = 4
	FlagCargo        int64 = 5
	FlagSkill        int64 = 7
	FlagLoSlotFirst  int64 = 11
	FlagLoSlotLast   int64 = 18
	FlagMedSlotFirst int64 = 19
	FlagMedSlotLast  int64 = 26
	FlagHiSlotFirst  int64 = 27
	FlagHiSlotLast   int64 = 34
	FlagPilot        int64 = 57
	FlagSkillInTraining int64 = 61
	FlagDroneBay     int64 = 87
	FlagAutoFit      int64 = 0
)

// SlotTypeForFlag maps a module slot flag to its rack ("hi", "med", "lo").
// Returns "" for flags outside the module slot ranges.
func SlotTypeForFlag(flag int64) string {
	switch {
	case flag >= FlagLoSlotFirst && flag <= FlagLoSlotLast:
		return "lo"
	case flag >= FlagMedSlotFirst && flag <= FlagMedSlotLast:
		return "med"
	case flag >= FlagHiSlotFirst && flag <= FlagHiSlotLast:
		return "hi"
	}
	return ""
}

// SlotIndexForFlag returns the 0-based slot index within its rack, or -1 for
// non-slot flags.
func SlotIndexForFlag(flag int64) int64 {
	switch {
	case flag >= FlagLoSlotFirst && flag <= FlagLoSlotLast:
		return flag - FlagLoSlotFirst
	case flag >= FlagMedSlotFirst && flag <= FlagMedSlotLast:
		return flag - FlagMedSlotFirst
	case flag >= FlagHiSlotFirst && flag <= FlagHiSlotLast:
		return flag - FlagHiSlotFirst
	}
	return -1
}
