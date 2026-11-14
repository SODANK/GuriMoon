package corp

import "fmt"

// This file is the role catalogue behind corpRegistry.GetRoles and GetRoleGroups. The client
// joins the two rowsets itself (base_corporation.py GetRoleGroupings), so:
//
//   - A role whose Name ends in a digit 1-7 is divisional. GetRoleGroupings runs
//     int(roleName[-1:]) on every role a divisional group's mask matches, so that mask must
//     cover all seven divisions and no general role's name may end in a digit.
//   - "CanTake" / "CanQuery" in a name becomes the Take / Query column label.
//   - Short is the on-screen label; the client sorts the general roles by it.

// Role is one row of the GetRoles catalogue.
type Role struct {
	ID          uint64
	Name        string // internal name; divisional roles end in the division digit
	Short       string // shortDescription: the label the client draws
	Description string
}

// RoleGroup is one row of the GetRoleGroups catalogue.  AppliesTo and
// AppliesToGrantable name the member/title columns the group's checkboxes
// read and write (corp_ui_members.py, corp_ui_member_titles.py).
type RoleGroup struct {
	ID                 int64
	Name               string
	IsDivisional       bool
	Mask               uint64
	AppliesTo          string
	AppliesToGrantable string
}

// Role group IDs, corputil.GROUP_* in the client. corp_ui_members and corp_ui_member_titles
// both open on ID 1, so that group must exist.
const (
	GroupGeneralRoles               int64 = 0
	GroupDivisionalAccountingRoles  int64 = 1
	GroupDivisionalHangarAtHQ       int64 = 2
	GroupDivisionalContainerAtHQ    int64 = 3
	GroupDivisionalHangarAtBase     int64 = 4
	GroupDivisionalContainerAtBase  int64 = 5
	GroupDivisionalHangarAtOther    int64 = 6
	GroupDivisionalContainerAtOther int64 = 7
)

// hiddenRoles are defined, repaired in eveConstants and safe to hold, but
// nothing enforces them, so they are kept out of the catalogue the client draws.
// Remove an entry to show the role again.
var hiddenRoles = map[uint64]bool{
	RoleAuditor:         true, // AuditMember is an empty stub; no role history is recorded
	RoleEquipmentConfig: true, // no server reader
}

// generalRoles are the non-divisional roles, in catalogue order.  The client
// re-sorts them by Short before drawing, so this order is only the tie-break.
var generalRoles = []Role{
	{RoleDirector, "roleDirector", "Director",
		"Full control of the corporation.  Only the CEO may grant it."},
	{RolePersonnelManager, "rolePersonnelManager", "Personnel Manager",
		"May accept, reject and renegotiate applications to the corporation."},
	{RoleAccountant, "roleAccountant", "Accountant",
		"May view and spend from the corporation wallet divisions they hold access to."},
	{RoleJuniorAccountant, "roleJuniorAccountant", "Junior Accountant",
		"May view, but not spend from, the corporation wallet divisions they hold access to."},
	{RoleSecurityOfficer, "roleSecurityOfficer", "Security Officer",
		"May inspect the station hangars of other corporation members."},
	{RoleFactoryManager, "roleFactoryManager", "Factory Manager",
		"May install and deliver corporation manufacturing and research jobs."},
	{RoleStationManager, "roleStationManager", "Station Manager",
		"May administer stations the corporation owns."},
	{RoleAuditor, "roleAuditor", "Auditor",
		"May audit the role history of corporation members."},
	{RoleEquipmentConfig, "roleEquipmentConfig", "Config Equipment",
		"May reconfigure corporation equipment in space."},
	{RoleStarbaseConfig, "roleStarbaseConfig", "Config Starbase Equipment",
		"May anchor, online and configure corporation starbase structures."},
	{RoleCanRentOffice, "roleCanRentOffice", "Rent Office",
		"May rent an office for the corporation at a station."},
	{RoleCanRentFactorySlot, "roleCanRentFactorySlot", "Rent Factory Facility",
		"May rent a manufacturing facility for the corporation."},
	{RoleCanRentResearchSlot, "roleCanRentResearchSlot", "Rent Research Facility",
		"May rent a research facility for the corporation."},
	{RoleTrader, "roleTrader", "Trader",
		"May place and manage market orders on behalf of the corporation."},
	{RoleChatManager, "roleChatManager", "Communications Officer",
		"May administer the corporation chat channel."},
}

// divisionalFamily describes one bank of seven per-division role bits.
type divisionalFamily struct {
	base       uint64 // the division-1 bit; division N is base << (N-1)
	namePrefix string // must contain CanTake or CanQuery
	shortLabel string
	what       string // wording for the generated description
}

var (
	accountFamilies = []divisionalFamily{
		{RoleAccountCanTake1, "roleAccountCanTake", "Account Take", "Spending"},
		{RoleAccountCanQuery1, "roleAccountCanQuery", "Account Query", "Viewing"},
	}
	hangarFamilies = []divisionalFamily{
		{RoleHangarCanTake1, "roleHangarCanTake", "Hangar Take", "Take-from"},
		{RoleHangarCanQuery1, "roleHangarCanQuery", "Hangar Query", "Look-into"},
	}
	containerFamilies = []divisionalFamily{
		{RoleContainerCanTake1, "roleContainerCanTake", "Container Take", "Secure-container"},
	}
)

func familyMask(fams []divisionalFamily) uint64 {
	var m uint64
	for _, f := range fams {
		for d := 0; d < 7; d++ {
			m |= f.base << uint(d)
		}
	}
	return m
}

func appendFamilies(out []Role, fams []divisionalFamily) []Role {
	for _, f := range fams {
		for d := 1; d <= 7; d++ {
			out = append(out, Role{
				ID:          f.base << uint(d-1),
				Name:        fmt.Sprintf("%s%d", f.namePrefix, d),
				Short:       fmt.Sprintf("%s %d", f.shortLabel, d),
				Description: fmt.Sprintf("%s access to division %d.", f.what, d),
			})
		}
	}
	return out
}

// generalMask is every non-divisional role bit; it is the general group's mask
// and by construction does not overlap any divisional family.
func generalMask() uint64 {
	var m uint64
	for _, r := range generalRoles {
		if !hiddenRoles[r.ID] {
			m |= r.ID
		}
	}
	return m
}

// Catalogue returns every corporation role: the general roles followed by the
// three divisional banks.  The slice is freshly built on each call, because
// the client sorts the rowset it is built from in place.
func Catalogue() []Role {
	out := make([]Role, 0, len(generalRoles)+35)
	for _, r := range generalRoles {
		if !hiddenRoles[r.ID] {
			out = append(out, r)
		}
	}
	out = appendFamilies(out, accountFamilies)
	out = appendFamilies(out, hangarFamilies)
	out = appendFamilies(out, containerFamilies)
	return out
}

// Groups returns the eight role groups.  Accounting is corporation-wide, so it
// hangs off roles/grantableRoles; hangar and container access is per-location,
// which is what the three rolesAt* columns are for.
func Groups() []RoleGroup {
	acct := familyMask(accountFamilies)
	hangar := familyMask(hangarFamilies)
	cont := familyMask(containerFamilies)
	return []RoleGroup{
		{GroupGeneralRoles, "General Roles", false, generalMask(), "roles", "grantableRoles"},
		{GroupDivisionalAccountingRoles, "Accounting Roles", true, acct, "roles", "grantableRoles"},
		{GroupDivisionalHangarAtHQ, "Hangar Roles at HQ", true, hangar, "rolesAtHQ", "grantableRolesAtHQ"},
		{GroupDivisionalContainerAtHQ, "Container Roles at HQ", true, cont, "rolesAtHQ", "grantableRolesAtHQ"},
		{GroupDivisionalHangarAtBase, "Hangar Roles at Base", true, hangar, "rolesAtBase", "grantableRolesAtBase"},
		{GroupDivisionalContainerAtBase, "Container Roles at Base", true, cont, "rolesAtBase", "grantableRolesAtBase"},
		{GroupDivisionalHangarAtOther, "Hangar Roles at Other", true, hangar, "rolesAtOther", "grantableRolesAtOther"},
		{GroupDivisionalContainerAtOther, "Container Roles at Other", true, cont, "rolesAtOther", "grantableRolesAtOther"},
	}
}

// LocationalMask is the set of roles held per location rather than corporation-wide. It mirrors
// __locationalRoles in the client's base_corporation.py.
func LocationalMask() uint64 {
	return familyMask(hangarFamilies) | familyMask(containerFamilies)
}
