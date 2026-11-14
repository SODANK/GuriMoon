# ============================================================================
#  GuriMoon client patch: bounty portraits for the billboards
#
#  Installed by cmd/clientpatch in the GuriMoon server repository and run
#  from script/my_autoexec.py.  Do not edit this copy -- rerun the patcher.
#  Python 2.3 (Stackless): no decorators, no generator expressions, no `with`,
#  no conditional expressions, no set().
#
#  The in-space billboards show a WANTED poster for a random Most Wanted pilot
#  and fetch the face over HTTP (billboardMgr: img.eve.is/serv.asp?s=256&c=).
#  The server cannot draw a face -- only a client can -- so it serves a
#  silhouette until somebody hands it a real one.
#
#  This patch makes the client that somebody.  While the pilot is docked it
#  asks the server (charmgr.GetPortraitJobs) for a couple of Most Wanted pilots
#  the server has no picture of, draws each with the client's own
#  svc.photo.GetPortrait -- the same render that fills chat and info-window
#  portraits, written to cache/Pictures/Portraits/<charID>_<size>.png -- and
#  hands the PNG back (charmgr.SubmitPortrait).  A face already in the local
#  cache is sent as it is, without a new render.  A pilot that cannot be drawn
#  is reported as None so the server stops asking for it.
#
#  Only docked, never mid session change: a portrait render is a short hitch
#  and must not land in a fight.  The server paces the polling (it answers
#  with the next poll interval) and can switch the whole thing off
#  (server_settings portraits.clientRender = 0).
#
#  Everything is wrapped: if anything here fails the client runs exactly as
#  it would without the patch.
# ============================================================================

print ">>> GuriMoon bounty portrait patch loading..."

try:
    Import('svc')

    _bpG = svc.photo.GetPortrait.im_func.func_globals
    _bpBlue = _bpG['blue']
    _bpUthread = _bpG['uthread']
    _bpOs = _bpG['os']

    def _BPReady():
        s = getattr(eve, 'session', None)
        if s is None:
            return 0
        if not getattr(s, 'charid', None) or not getattr(s, 'stationid', None):
            return 0
        if getattr(s, 'mutating', 0):
            return 0
        return 1

    def _BPPortraitPath(charID, size, _blue=_bpBlue):
        path = _blue.os.cachepath + 'Pictures/Portraits/%s_%s.png' % (charID, size)
        if isinstance(path, unicode):
            path = path.encode('cp1252', 'replace')
        return path

    def _BPDraw(charID, size, _blue=_bpBlue, _os=_bpOs, _path=_BPPortraitPath):
        # Returns the PNG bytes, or None when this pilot cannot be drawn.
        path = _path(charID, size)
        if not _os.path.exists(path):
            tries = 0
            while 1:
                tries = tries + 1
                try:
                    eve.LocalSvc('photo').GetPortrait(charID, None, size)
                    break
                except:
                    # A lost device (alt-tab, minimise) is the common one;
                    # photosvc.ProducePortraits waits it out the same way.
                    if tries >= 3:
                        import traceback
                        traceback.print_exc()
                        return None
                    _blue.pyos.synchro.Sleep(2000)
            if not _os.path.exists(path):
                return None
        f = open(path, 'rb')
        try:
            data = f.read()
        finally:
            f.close()
        if not data:
            return None
        return data

    def _BPLoop(_blue=_bpBlue, _ready=_BPReady, _draw=_BPDraw):
        wait = 90
        while 1:
            _blue.pyos.synchro.Sleep(wait * 1000)
            wait = 60
            try:
                if not _ready():
                    continue
                (jobs, size, poll) = eve.RemoteSvc('charmgr').GetPortraitJobs()
                wait = poll or 300
                for charID in jobs:
                    if not _ready():
                        # Undocked mid-batch: the server's lease runs out and
                        # someone else draws the rest.
                        break
                    data = _draw(charID, size)
                    eve.RemoteSvc('charmgr').SubmitPortrait(charID, size, data)
                    # Spread the hitches out.
                    _blue.pyos.synchro.Sleep(3000)
            except:
                # A server without these calls answers with an exception;
                # back off for an hour rather than asking every minute.
                import traceback
                traceback.print_exc()
                wait = 3600

    _bpUthread.new(_BPLoop)
    print ">>> GuriMoon bounty portrait patch applied (draws Most Wanted faces while docked)"

except:
    import traceback
    traceback.print_exc()
    print ">>> ERROR: GuriMoon bounty portrait patch failed to apply (client unchanged)"
