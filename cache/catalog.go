// catalog.go holds the SQL query and result kind for every cachable object.
//
// Each entry maps a cachable objectID to the query that regenerates it and the PyRep
// container the result is wrapped in, mirroring the macro table in
// server/cache/CachableList.h. Column names use the camelCase of the EVE SDE.
package cache

// resultKind identifies the PyRep container type a SQL result is wrapped in.
type resultKind int

const (
	kindTupleSet    resultKind = iota // PyTuple([cols_list, rows_list])
	kindRowset                        // PyObject("util.Rowset", {header, RowClass, lines})
	kindRowsetTuple                   // PyTuple([None, util.Rowset])
	kindIndexRowset                   // PyObject("util.IndexRowset", {idName, header, RowClass, items})
)

type catalogEntry struct {
	query string
	kind  resultKind
	key   string // index column name, used only for kindIndexRowset
}

// catalog maps every known cachable objectID to its SQL query and result type.
var catalog = map[string]catalogEntry{

	// ── Login cachables (39) ─────────────────────────────────────────────────

	"config.BulkData.ramactivities": {
		query: "SELECT activityID,activityName,iconNo,description,published FROM ramActivities",
		kind:  kindTupleSet,
	},
	"config.BulkData.billtypes": {
		query: "SELECT billTypeID,billTypeName,description FROM billTypes",
		kind:  kindTupleSet,
	},
	"config.Bloodlines": {
		query: "SELECT bloodlineID,bloodlineName,raceID,description,maleDescription,femaleDescription,shipTypeID,corporationID,perception,willpower,charisma,memory,intelligence,bonusTypeID,skillTypeID1,skillTypeID2,graphicID FROM chrBloodlines",
		kind:  kindRowset,
	},
	"config.Units": {
		query: "SELECT unitID,unitName,displayName FROM eveUnits",
		kind:  kindRowset,
	},
	"config.BulkData.dgmtypeattribs": {
		query: "SELECT dgmTypeAttributes.typeID,dgmTypeAttributes.attributeID," +
			"IF(valueInt IS NULL,valueFloat,valueInt) AS value" +
			" FROM dgmTypeAttributes NATURAL JOIN dgmAttributeTypes" +
			" WHERE COALESCE(valueInt,valueFloat)!=defaultValue",
		kind: kindTupleSet,
	},
	"config.BulkData.messages": {
		query: "SELECT messageID,messageType,messageText,urlAudio,urlIcon FROM eveMessages",
		kind:  kindTupleSet,
	},
	"config.BulkData.ramtyperequirements": {
		query: "SELECT typeID,activity,requiredTypeID,quantity,damagePerJob,recycle FROM TL2MaterialsForTypeWithActivity",
		kind:  kindRowsetTuple,
	},
	"config.Statistics": {
		query: "SELECT statisticID,statisticName,statisticType,description,orderID,graphicID FROM eveStatistics",
		kind:  kindRowset,
	},
	"config.BulkData.ramaltypesdetailpergroup": {
		query: "SELECT assemblyLineTypeID,activityID,groupID,timeMultiplier,materialMultiplier FROM ramALTypeGroup",
		kind:  kindTupleSet,
	},
	// ramALTypes is served from the full ramAssemblyLineTypes table: the client's miss
	// path (GetAssemblyLineTypeEx) overwrites the whole cfg.ramaltypes recordset's header
	// (cfg.py __LoadData), shifting every row already loaded. Keep this column list
	// identical to assemblyLineTypeCols in db/configdb.go. The table has no typeName
	// column, so the name doubles for it.
	"config.BulkData.ramaltypes": {
		query: "SELECT assemblyLineTypeID," +
			"COALESCE(assemblyLineTypeName,'') AS assemblyLineTypeName," +
			"COALESCE(assemblyLineTypeName,'') AS typeName," +
			"COALESCE(description,'') AS description," +
			"COALESCE(activityID,1) AS activityID," +
			"COALESCE(baseTimeMultiplier,1) AS baseTimeMultiplier," +
			"COALESCE(baseMaterialMultiplier,1) AS baseMaterialMultiplier," +
			"COALESCE(volume,0) AS volume" +
			" FROM ramAssemblyLineTypes",
		kind:  kindTupleSet,
	},
	"config.BulkData.allianceshortnames": {
		query: "SELECT allianceID,shortName FROM alliance_shortnames",
		kind:  kindTupleSet,
	},
	"config.BulkData.ramcompletedstatuses": {
		query: "SELECT completedStatusID,completedStatusName,completedStatusText FROM ramCompletedStatuses",
		kind:  kindTupleSet,
	},
	"config.BulkData.categories": {
		query: "SELECT categoryID,categoryName,description,graphicID FROM eveCategories",
		kind:  kindTupleSet,
	},
	"config.BulkData.invtypereactions": {
		query: "SELECT reactionTypeID,input,typeID,quantity FROM invTypeReactions",
		kind:  kindTupleSet,
	},
	"config.BulkData.dgmtypeeffects": {
		query: "SELECT typeID,effectID,isDefault FROM dgmTypeEffects",
		kind:  kindTupleSet,
	},
	"config.BulkData.metagroups": {
		query: "SELECT metaGroupID,metaGroupName,description,graphicID FROM invMetaGroups",
		kind:  kindTupleSet,
	},
	"config.BulkData.ramtypematerials": {
		// What each type is made of, keyed by that type: the client's Refine and Reprocess menu
		// entries test for a row. The rows are the ones evedb.GetTypeMaterials reprocesses.
		query: "SELECT typeID,requiredTypeID AS materialTypeID,quantity FROM tl2materialsfortypewithactivity" +
			" WHERE activity=6 AND recycle=0 AND damagePerJob>=1" +
			" UNION ALL SELECT oreTypeID,mineralTypeID,amountPerBatch FROM invorereprocessing",
		kind: kindRowsetTuple,
	},
	"config.EncyclopediaTypes": {
		query: "SELECT encyclopediaTypeID,encyclopediaTypeName FROM eveEncyclTypes",
		kind:  kindRowset,
	},
	"config.ChannelTypes": {
		query: "SELECT channelTypeID,channelTypeName,defaultName FROM channelTypes",
		kind:  kindRowset,
	},
	"config.BulkData.owners": {
		// Player characters are absent, so the cachable does not grow with the roster and a
		// rename does not rebuild it for everybody (see InvalidateOwnerNames). cfg.eveowners
		// is a Recordset whose miss path primes through GetMultiOwnersEx, whose first UNION
		// arm is the entity lookup, so names resolve when asked. Consequences: Prime logs a
		// severity-2 traceback when one call misses 50+ keys, and GetIfExists returns None on
		// a miss without fetching.
		//
		// cacheOwners holds the static entries, eveStaticOwners adds factions and corps, and
		// the staStations fallback keeps station-owning corps present.
		query: "SELECT ownerID,ownerName,CAST(typeID AS SIGNED) AS typeID FROM cacheOwners" +
			" UNION SELECT ownerID,ownerName,CAST(typeID AS SIGNED) AS typeID FROM eveStaticOwners" +
			" UNION SELECT DISTINCT s.corporationID AS ownerID," +
			" SUBSTRING_INDEX(s.stationName,' - ',1) AS ownerName," +
			" 2 AS typeID FROM staStations s" +
			" WHERE s.corporationID NOT IN (SELECT ownerID FROM cacheOwners)",
		kind: kindTupleSet,
	},
	"config.StaticOwners": {
		query: "SELECT ownerID,ownerName,CAST(typeID AS SIGNED) AS typeID FROM eveStaticOwners",
		kind:  kindRowset,
	},
	"config.Races": {
		query: "SELECT raceID,raceName,description,skillTypeID1,typeID,typeQuantity,graphicID FROM chrRaces",
		kind:  kindRowset,
	},
	"config.BulkData.ramaltypesdetailpercategory": {
		query: "SELECT assemblyLineTypeID,activityID,categoryID,timeMultiplier,materialMultiplier FROM ramALTypeCategory",
		kind:  kindTupleSet,
	},
	"config.Attributes": {
		query: "SELECT attributeID,attributeName,description,graphicID FROM chrAttributes",
		kind:  kindRowset,
	},
	"config.BulkData.tickernames": {
		// NPC corporations only (hasPlayerPersonnelManager=0).
		query: "SELECT corporationID,tickerName,shape1,shape2,shape3,color1,color2,color3 FROM corporation WHERE hasPlayerPersonnelManager=0",
		kind:  kindTupleSet,
	},
	"config.Roles": {
		query: "SELECT roleID,roleName,description FROM eveRoles",
		kind:  kindRowset,
	},
	"config.BulkData.groups": {
		query: "SELECT groupID,categoryID,groupName,description,graphicID,useBasePrice,allowManufacture,allowRecycler,anchored,anchorable,fittableNonSingleton FROM invGroups",
		kind:  kindTupleSet,
	},
	"config.BulkData.shiptypes": {
		query: "SELECT shipTypeID,powerCoreTypeID,capacitorTypeID,shieldTypeID,propulsionTypeID,sensorTypeID,armorTypeID,computerTypeID,weaponTypeID,miningTypeID,skillTypeID FROM invShipTypes",
		kind:  kindTupleSet,
	},
	"config.BulkData.dgmattribs": {
		query: "SELECT attributeID,attributeName,attributeCategory,description,maxAttributeID,attributeIdx,graphicID,chargeRechargeTimeID,defaultValue,published,unitID,displayName,stackable,highIsGood FROM dgmAttributes",
		kind:  kindTupleSet,
	},
	"config.BulkData.locations": {
		query: "SELECT locationID,locationName,x,y,z FROM cacheLocations",
		kind:  kindTupleSet,
	},
	"config.BulkData.constants": {
		query: "SELECT constantID,constantValue FROM eveConstants",
		kind:  kindTupleSet,
	},
	"config.Flags": {
		query: "SELECT flagID,flagName,flagText,flagType,orderID FROM eveFlags",
		kind:  kindRowset,
	},
	"config.BulkData.bptypes": {
		query: "SELECT invTypes.typeName AS blueprintTypeName,invTypes.description,invTypes.graphicID,invTypes.basePrice," +
			"blueprintTypeID,parentBlueprintTypeID,productTypeID,productionTime,techLevel," +
			"researchProductivityTime,researchMaterialTime,researchCopyTime,researchTechTime," +
			"productivityModifier,materialModifier,wasteFactor,chanceOfReverseEngineering,maxProductionLimit" +
			" FROM invBlueprintTypes,invTypes WHERE invBlueprintTypes.blueprintTypeID=invTypes.typeID",
		kind: kindTupleSet,
	},
	"config.BulkData.graphics": {
		query: "SELECT graphicID,url3D,urlWeb,icon,urlSound,explosionID FROM eveGraphics",
		kind:  kindTupleSet,
	},
	"config.StaticLocations": {
		query: "SELECT locationID,locationName,x,y,z FROM eveStaticLocations",
		kind:  kindRowset,
	},
	"config.InvContrabandTypes": {
		query: "SELECT factionID,typeID,standingLoss,confiscateMinSec,fineByValue,attackMinSec FROM invContrabandTypes",
		kind:  kindRowset,
	},
	"config.BulkData.dgmeffects": {
		query: "SELECT effectID,effectName,effectCategory,preExpression,postExpression,description,guid,graphicID," +
			"isOffensive,isAssistance,durationAttributeID,trackingSpeedAttributeID,dischargeAttributeID," +
			"rangeAttributeID,falloffAttributeID,published,displayName,isWarpSafe,rangeChance," +
			"electronicChance,propulsionChance,distribution,sfxName,npcUsageChanceAttributeID,npcActivationChanceAttributeID" +
			" FROM dgmEffects",
		kind: kindTupleSet,
	},
	"config.BulkData.types": {
		query: "SELECT typeID,groupID,typeName,description,graphicID,radius,mass,volume,capacity," +
			"portionSize,raceID,basePrice,published,marketGroupID,chanceOfDuplicating FROM invTypes",
		kind: kindTupleSet,
	},
	"config.BulkData.invmetatypes": {
		query: "SELECT typeID,parentTypeID,metaGroupID FROM invMetaTypes",
		kind:  kindTupleSet,
	},

	// ── Character-creation cachables ─────────────────────────────────────────

	"charCreationInfo.bloodlines": {
		query: "SELECT bloodlineID,bloodlineName,raceID,description,maleDescription,femaleDescription,shipTypeID,corporationID,perception,willpower,charisma,memory,intelligence,bonusTypeID,skillTypeID1,skillTypeID2,graphicID FROM chrBloodlines",
		kind:  kindRowset,
	},
	"charCreationInfo.departments": {
		query: "SELECT schoolID,departmentID,departmentName,description,skillTypeID1,skillTypeID2,skillTypeID3,graphicID FROM chrDepartments",
		kind:  kindRowset,
	},
	"charCreationInfo.races": {
		query: "SELECT raceID,raceName,description,skillTypeID1,typeID,typeQuantity,graphicID FROM chrRaces",
		kind:  kindRowset,
	},
	"charCreationInfo.ancestries": {
		query: "SELECT ancestryID,ancestryName,bloodlineID,description,postPerception,postWillpower,postCharisma,postMemory,postIntelligence,perception,willpower,charisma,memory,intelligence,skillTypeID1,skillTypeID2,typeID,typeQuantity,typeID2,typeQuantity2,graphicID FROM chrAncestries",
		kind:  kindRowset,
	},
	"charCreationInfo.specialities": {
		query: "SELECT fieldID,specialityID,specialityName,description,skillTypeID1,skillTypeID2,typeID,typeQuantity,typeID2,typeQuantity2,graphicID FROM chrSpecialities",
		kind:  kindRowset,
	},
	"charCreationInfo.schools": {
		query: "SELECT raceID,schoolID,schoolName,description,graphicID,corporationID,agentID,newAgentID FROM chrSchools",
		kind:  kindRowset,
	},
	"charCreationInfo.attributes": {
		query: "SELECT attributeID,attributeName,description,graphicID FROM chrAttributes",
		kind:  kindRowset,
	},
	"charCreationInfo.fields": {
		query: "SELECT departmentID,fieldID,fieldName,description,skillTypeID1,skillTypeID2,typeID,typeQuantity,typeID2,typeQuantity2,graphicID FROM chrFields",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_accessories": {
		query: "SELECT bloodlineID,gender,accessoryID,npc FROM chrBLAccessories",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_lights": {
		query: "SELECT lightID,lightName FROM chrBLLights",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_skins": {
		query: "SELECT bloodlineID,gender,skinID,npc FROM chrBLSkins",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_beards": {
		query: "SELECT bloodlineID,gender,beardID,npc FROM chrBLBeards",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_eyes": {
		query: "SELECT bloodlineID,gender,eyesID,npc FROM chrBLEyes",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_lipsticks": {
		query: "SELECT bloodlineID,gender,lipstickID,npc FROM chrBLLipsticks",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_makeups": {
		query: "SELECT bloodlineID,gender,makeupID,npc FROM chrBLMakeups",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_hairs": {
		query: "SELECT bloodlineID,gender,hairID,npc FROM chrBLHairs",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_backgrounds": {
		query: "SELECT backgroundID,backgroundName FROM chrBLBackgrounds",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_decos": {
		query: "SELECT bloodlineID,gender,decoID,npc FROM chrBLDecos",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_eyebrows": {
		query: "SELECT bloodlineID,gender,eyebrowsID,npc FROM chrBLEyebrows",
		kind:  kindRowset,
	},
	"charCreationInfo.bl_costumes": {
		query: "SELECT bloodlineID,gender,costumeID,npc FROM chrBLCostumes",
		kind:  kindRowset,
	},

	// ── Appearance cachables ─────────────────────────────────────────────────

	"charCreationInfo.eyebrows": {
		query: "SELECT eyebrowsID,eyebrowsName FROM chrEyebrows",
		kind:  kindRowset,
	},
	"charCreationInfo.eyes": {
		query: "SELECT eyesID,eyesName FROM chrEyes",
		kind:  kindRowset,
	},
	"charCreationInfo.decos": {
		query: "SELECT decoID,decoName FROM chrDecos",
		kind:  kindRowset,
	},
	"charCreationInfo.hairs": {
		query: "SELECT hairID,hairName FROM chrHairs",
		kind:  kindRowset,
	},
	"charCreationInfo.backgrounds": {
		query: "SELECT backgroundID,backgroundName FROM chrBackgrounds",
		kind:  kindRowset,
	},
	"charCreationInfo.accessories": {
		query: "SELECT accessoryID,accessoryName FROM chrAccessories",
		kind:  kindRowset,
	},
	"charCreationInfo.lights": {
		query: "SELECT lightID,lightName FROM chrLights",
		kind:  kindRowset,
	},
	"charCreationInfo.costumes": {
		query: "SELECT costumeID,costumeName FROM chrCostumes",
		kind:  kindRowset,
	},
	"charCreationInfo.makeups": {
		query: "SELECT makeupID,makeupName FROM chrMakeups",
		kind:  kindRowset,
	},
	"charCreationInfo.beards": {
		query: "SELECT beardID,beardName FROM chrBeards",
		kind:  kindRowset,
	},
	"charCreationInfo.skins": {
		query: "SELECT skinID,skinName FROM chrSkins",
		kind:  kindRowset,
	},
	"charCreationInfo.lipsticks": {
		query: "SELECT lipstickID,lipstickName FROM chrLipsticks",
		kind:  kindRowset,
	},

	// ── Other cachables ──────────────────────────────────────────────────────

	"dogmaIM.attributesByName": {
		query: "SELECT attributeID,attributeName,attributeCategory,description,maxAttributeID,attributeIdx,graphicID,chargeRechargeTimeID,defaultValue,published,displayName,unitID,stackable,highIsGood FROM dgmAttributeTypes",
		kind:  kindIndexRowset,
		key:   "attributeName",
	},
}

// HintSetEntry pairs a cachable objectID with the short dict key used in the
// char-creation and appearance-info response dicts (the KCACHABLE macro's second
// argument).
type HintSetEntry struct {
	ObjectID string
	ShortKey string
}

// CharCreateCachables is the ordered set of objects sent in GetCharCreationInfo,
// matching CharCreateCachableObjects[] in server/cache/ObjCacheService.cpp.
var CharCreateCachables = []HintSetEntry{
	{"charCreationInfo.bl_eyebrows", "eyebrows"},
	{"charCreationInfo.bl_costumes", "costumes"},
	{"charCreationInfo.bl_decos", "decos"},
	{"charCreationInfo.fields", "fields"},
	{"charCreationInfo.bl_hairs", "hairs"},
	{"charCreationInfo.bl_backgrounds", "backgrounds"},
	{"charCreationInfo.bloodlines", "bloodlines"},
	{"charCreationInfo.departments", "departments"},
	{"charCreationInfo.bl_accessories", "accessories"},
	{"charCreationInfo.bl_lights", "lights"},
	{"charCreationInfo.races", "races"},
	{"charCreationInfo.ancestries", "ancestries"},
	{"charCreationInfo.bl_skins", "skins"},
	{"charCreationInfo.specialities", "specialities"},
	{"charCreationInfo.schools", "schools"},
	{"charCreationInfo.attributes", "attributes"},
	{"charCreationInfo.bl_beards", "beards"},
	{"charCreationInfo.bl_eyes", "eyes"},
	{"charCreationInfo.bl_lipsticks", "lipsticks"},
	{"charCreationInfo.bl_makeups", "makeups"},
}

// AppearanceCachables is the ordered set of objects sent in GetAppearanceInfo,
// matching AppearanceCachableObjects[] in server/cache/ObjCacheService.cpp.
var AppearanceCachables = []HintSetEntry{
	{"charCreationInfo.eyebrows", "eyebrows"},
	{"charCreationInfo.eyes", "eyes"},
	{"charCreationInfo.decos", "decos"},
	{"charCreationInfo.hairs", "hairs"},
	{"charCreationInfo.backgrounds", "backgrounds"},
	{"charCreationInfo.accessories", "accessories"},
	{"charCreationInfo.costumes", "costumes"},
	{"charCreationInfo.lights", "lights"},
	{"charCreationInfo.makeups", "makeups"},
	{"charCreationInfo.beards", "beards"},
	{"charCreationInfo.skins", "skins"},
	{"charCreationInfo.lipsticks", "lipsticks"},
}

// loginCachables is the ordered list of objectIDs sent as util.CachedObject hints in
// AuthenticationRsp.cachables (LoginCachableObjects[] in
// server/cache/ObjCacheService.cpp).
var loginCachables = []string{
	"config.BulkData.ramactivities",
	"config.BulkData.billtypes",
	"config.Bloodlines",
	"config.Units",
	"config.BulkData.dgmtypeattribs",
	"config.BulkData.messages",
	"config.BulkData.ramtyperequirements",
	"config.Statistics",
	"config.BulkData.ramaltypesdetailpergroup",
	"config.BulkData.ramaltypes",
	"config.BulkData.allianceshortnames",
	"config.BulkData.ramcompletedstatuses",
	"config.BulkData.categories",
	"config.BulkData.invtypereactions",
	"config.BulkData.dgmtypeeffects",
	"config.BulkData.metagroups",
	"config.BulkData.ramtypematerials",
	"config.EncyclopediaTypes",
	"config.ChannelTypes",
	"config.BulkData.owners",
	"config.StaticOwners",
	"config.Races",
	"config.BulkData.ramaltypesdetailpercategory",
	"config.Attributes",
	"config.BulkData.tickernames",
	"config.Roles",
	"config.BulkData.groups",
	"config.BulkData.shiptypes",
	"config.BulkData.dgmattribs",
	"config.BulkData.locations",
	"config.BulkData.constants",
	"config.Flags",
	"config.BulkData.bptypes",
	"config.BulkData.graphics",
	"config.StaticLocations",
	"config.InvContrabandTypes",
	"config.BulkData.dgmeffects",
	"config.BulkData.types",
	"config.BulkData.invmetatypes",
}
