// Package corp provides corporation role bitmask constants and enforcement
// helpers. Values are from the EVE client's const.py / evemu CorporationDB.
package corp

// Role bitmask constants.
const (
	RoleAll              uint64 = ^uint64(0) // 0xFFFFFFFFFFFFFFFF
	RoleDirector         uint64 = 1
	RolePersonnelManager uint64 = 128
	RoleAccountant       uint64 = 256
	RoleSecurityOfficer  uint64 = 512
	RoleFactoryManager   uint64 = 1024
	RoleStationManager   uint64 = 2048
	RoleAuditor          uint64 = 4096

	RoleHangarCanTake1 uint64 = 8192
	RoleHangarCanTake2 uint64 = 8192 << 1
	RoleHangarCanTake3 uint64 = 8192 << 2
	RoleHangarCanTake4 uint64 = 8192 << 3
	RoleHangarCanTake5 uint64 = 8192 << 4
	RoleHangarCanTake6 uint64 = 8192 << 5
	RoleHangarCanTake7 uint64 = 8192 << 6

	RoleHangarCanQuery1 uint64 = 1048576
	RoleHangarCanQuery2 uint64 = 1048576 << 1
	RoleHangarCanQuery3 uint64 = 1048576 << 2
	RoleHangarCanQuery4 uint64 = 1048576 << 3
	RoleHangarCanQuery5 uint64 = 1048576 << 4
	RoleHangarCanQuery6 uint64 = 1048576 << 5
	RoleHangarCanQuery7 uint64 = 1048576 << 6

	RoleAccountCanTake1 uint64 = 134217728
	RoleAccountCanTake2 uint64 = 134217728 << 1
	RoleAccountCanTake3 uint64 = 134217728 << 2
	RoleAccountCanTake4 uint64 = 134217728 << 3
	RoleAccountCanTake5 uint64 = 134217728 << 4
	RoleAccountCanTake6 uint64 = 134217728 << 5
	RoleAccountCanTake7 uint64 = 134217728 << 6

	RoleAccountCanQuery1 uint64 = 17179869184
	RoleAccountCanQuery2 uint64 = 17179869184 << 1
	RoleAccountCanQuery3 uint64 = 17179869184 << 2
	RoleAccountCanQuery4 uint64 = 17179869184 << 3
	RoleAccountCanQuery5 uint64 = 17179869184 << 4
	RoleAccountCanQuery6 uint64 = 17179869184 << 5
	RoleAccountCanQuery7 uint64 = 17179869184 << 6

	RoleEquipmentConfig uint64 = 2199023255552

	RoleContainerCanTake1 uint64 = 4398046511104
	RoleContainerCanTake2 uint64 = 4398046511104 << 1
	RoleContainerCanTake3 uint64 = 4398046511104 << 2
	RoleContainerCanTake4 uint64 = 4398046511104 << 3
	RoleContainerCanTake5 uint64 = 4398046511104 << 4
	RoleContainerCanTake6 uint64 = 4398046511104 << 5
	RoleContainerCanTake7 uint64 = 4398046511104 << 6

	RoleCanRentOffice  uint64 = 562949953421312
	RoleStarbaseConfig uint64 = 9007199254740992
	RoleTrader         uint64 = 18014398509481984
	RoleChatManager    uint64 = 36028797018963968
)

// CorpAccountKey is the master wallet division key.
const CorpAccountKey int64 = 1000

// DefaultNewMemberRoles is the role mask granted to new corporation members. It is zero because
// the client gates the CEO/director UI tabs on the session role bits. CEOs get RoleAll via the
// ceoID check in JoinCorporation.
const DefaultNewMemberRoles uint64 = 0

// HasRole returns true if roles contains all bits in required.
func HasRole(roles, required uint64) bool {
	return roles&required == required
}

// HasAnyRole returns true if roles contains at least one of the given role bits.
func HasAnyRole(roles uint64, any ...uint64) bool {
	for _, r := range any {
		if roles&r != 0 {
			return true
		}
	}
	return false
}

// IsCEOOrDirector returns true if roles has the Director bit set.
// (The CEO always has RoleAll, which includes Director.)
func IsCEOOrDirector(roles uint64) bool {
	return HasRole(roles, RoleDirector)
}

// Three roles the shipped eveConstants dump cannot express: its corpRole* values were truncated
// to uint32, so every bit above 2^31 reads 4294967295. These sit in the gap between CanRentOffice
// (2^49) and StarbaseConfig (2^53).
const (
	RoleCanRentFactorySlot  uint64 = 1 << 50
	RoleCanRentResearchSlot uint64 = 1 << 51
	RoleJuniorAccountant    uint64 = 1 << 52
)
