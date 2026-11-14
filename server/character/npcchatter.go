package character

import (
	"log"
	"strconv"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// npcChatterPirateGroups joins the client's pirate chatter loop: Deadspace Overseer plus every
// Asteroid* and Deadspace* rat group. See docs/audio-ambience.md#npc-radio-chatter.
var npcChatterPirateGroups = groupRanges(
	[2]int64{435, 435}, [2]int64{550, 584}, [2]int64{593, 597}, [2]int64{602, 606},
	[2]int64{611, 615}, [2]int64{620, 624}, [2]int64{629, 633},
)

// npcChatterPoliceGroups joins the police chatter loop: Faction Drone (navies) and Concord Drone.
var npcChatterPoliceGroups = []int64{288, 301}

func groupRanges(ranges ...[2]int64) []int64 {
	var out []int64
	for _, r := range ranges {
		for g := r[0]; g <= r[1]; g++ {
			out = append(out, g)
		}
	}
	return out
}

// npcChatterSource is exec'd by the client's svc.debug.OnRemoteExecute with the debug module as
// globals and a session dict as locals, so every name lives inside the function. Python 2.3.
// Update is wrapped because a throw in it otherwise ends all ambience for the session, and the
// running AutoTimer holds the old bound method, so its method is repointed.
const npcChatterSource = `
def _ecNPCChatter(pirate, police):
    import sys
    groups = {}
    for g in pirate:
        groups[g] = 'PirateDrone'
    for g in police:
        groups[g] = 'PoliceDrone'
    cls = Import('svc').envAudio
    cls._ecChatterGroups = groups
    if getattr(cls, '_ecChatter', 0):
        return
    origType = cls.GetType.im_func
    origUpdate = cls.Update.im_func
    def GetType(self, ballID):
        ret = origType(self, ballID)
        if ret is None:
            bp = eve.LocalSvc('michelle').GetBallpark()
            if bp is not None:
                slim = bp.GetInvItem(ballID)
                if slim is not None:
                    ret = self._ecChatterGroups.get(cfg.invtypes.Get(slim.typeID).groupID)
        return ret
    def Update(self):
        try:
            origUpdate(self)
        except:
            if not getattr(cls, '_ecChatterLogged', 0):
                cls._ecChatterLogged = 1
                self.LogError('envAudio Update', sys.exc_info()[1])
            sys.exc_clear()
    cls.GetType = GetType
    cls.Update = Update
    cls._ecChatter = 1
    inst = sm.services.get('envAudio')
    if inst is not None and inst.updateTimer:
        inst.updateTimer.method = inst.Update
`

// pushNPCChatter sends the client the chatter group mapping as OnRemoteExecute. server_settings
// audio.npcChatter = 0 turns it off for clients that log in afterwards.
func pushNPCChatter(c *evenet.Conn, addr string, accountID int64) {
	if v, ok := evedb.CachedSetting("audio.npcChatter"); ok && strings.TrimSpace(v) == "0" {
		return
	}
	if err := server.SendNotification(c, accountID, "OnRemoteExecute", "clientID", npcChatterArgs()); err != nil {
		log.Printf("[%s] npc chatter: %v", addr, err)
	}
}

// npcChatterArgs is OnRemoteExecute's argument tuple: the source plus a call with both group lists.
func npcChatterArgs() marshal.PyTuple {
	code := npcChatterSource + "_ecNPCChatter(" + pyIntList(npcChatterPirateGroups) + ", " +
		pyIntList(npcChatterPoliceGroups) + ")\n"
	return marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: code}}}
}

func pyIntList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
