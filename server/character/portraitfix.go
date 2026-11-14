package character

import (
	"context"
	"database/sql"
	"log"
	"strconv"
	"strings"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// portraitFixCostumes are the Jin-Mei male costumes built on costumeShape2.tri, the ones the
// client drops from a portrait drawn from cached face assets. See
// docs/character-profile.md#jin-mei-costume-portraits.
var portraitFixCostumes = []int64{257, 260, 261, 263, 264, 267, 270}

// portraitFixVersion names one purge of the client portrait cache. Bump it to make every client
// delete the listed pilots' cached portraits again.
const portraitFixVersion = "1"

// portraitFixSource is exec'd by the client's svc.debug.OnRemoteExecute with the debug module as
// globals and a session dict as locals, so every name lives inside the function. Python 2.3.
const portraitFixSource = `
def _ecPortraitFix(ids, version):
    import blue, os, sys
    cls = sm.GetService('photo').__class__
    if not getattr(cls, '_ecFaceFix', 0):
        orig = cls.GetPortrait.im_func
        def flush():
            rot = eve.rot
            old = rot.cacheTimeout
            try:
                rot.cacheTimeout = 0
                blue.pyos.synchro.Yield()
            finally:
                rot.cacheTimeout = old
        def GetPortrait(self, *args, **kw):
            try:
                flush()
            except:
                sys.exc_clear()
            return orig(self, *args, **kw)
        cls.GetPortrait = GetPortrait
        cls._ecFaceFix = 1
    d = os.path.join(blue.os.cachepath, 'Pictures', 'Portraits')
    marker = os.path.join(d, 'gurimoon_portraitfix.txt')
    done = ''
    try:
        f = open(marker)
        done = f.read().strip()
        f.close()
    except:
        sys.exc_clear()
    if done == version:
        return
    want = {}
    for i in ids:
        want[str(i)] = 1
    for name in os.listdir(d):
        if name[-4:] == '.png' and want.has_key(name.split('_')[0]):
            try:
                os.remove(os.path.join(d, name))
            except:
                sys.exc_clear()
    f = open(marker, 'w')
    f.write(version)
    f.close()
`

// pushPortraitFix sends the client the portrait render fix as OnRemoteExecute. It runs before
// the character list answer so the selection screen already draws with it. server_settings
// portraits.clientFix = 0 turns it off.
func pushPortraitFix(ctx context.Context, c *evenet.Conn, addr string, accountID int64, sqldb *sql.DB) {
	if v, ok := evedb.CachedSetting("portraits.clientFix"); ok && strings.TrimSpace(v) == "0" {
		return
	}
	ids, err := portraitFixPilots(ctx, sqldb)
	if err != nil {
		log.Printf("[%s] portrait fix: %v", addr, err)
		return
	}
	if err := server.SendNotification(c, accountID, "OnRemoteExecute", "clientID", portraitFixArgs(ids)); err != nil {
		log.Printf("[%s] portrait fix: %v", addr, err)
	}
}

// portraitFixArgs is OnRemoteExecute's argument tuple: the source plus a call with the pilot list.
func portraitFixArgs(ids []int64) marshal.PyTuple {
	idStrs := make([]string, len(ids))
	for i, id := range ids {
		idStrs[i] = strconv.FormatInt(id, 10)
	}
	code := portraitFixSource + "_ecPortraitFix([" + strings.Join(idStrs, ",") + "], '" + portraitFixVersion + "')\n"
	return marshal.PyTuple{Items: []marshal.PyRep{marshal.PyString{Value: code}}}
}

// portraitFixPilots lists the pilots whose cached portraits the client deletes once.
func portraitFixPilots(ctx context.Context, sqldb *sql.DB) ([]int64, error) {
	ph := make([]string, len(portraitFixCostumes))
	args := make([]any, len(portraitFixCostumes))
	for i, id := range portraitFixCostumes {
		ph[i] = "?"
		args[i] = id
	}
	rows, err := sqldb.QueryContext(ctx, `SELECT characterID FROM character_
		WHERE bloodlineID = 12 AND gender = 1 AND costumeID IN (`+strings.Join(ph, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
