# ============================================================================
#  GuriMoon client patch: asteroid lock-on bracket
#
#  Installed by cmd/clientpatch in the GuriMoon server repository and run
#  from script/my_autoexec.py.  Do not edit this copy -- rerun the patcher.
#  Python 2.3 (Stackless): no decorators, no generator expressions, no `with`,
#  no conditional expressions, no set().
#
#  Asteroids get no bracket in space by default (bracketMgr.DoBallsAdded
#  ignores categoryAsteroid).  svc.bracket.OnStateChange makes a temporary one
#  when an asteroid becomes selected / targeting / targeted, and is meant to
#  drop it again when none of those hold.  The shipped test is
#
#      elif bracket and not IsTarget(itemID): ClearBracket
#
#  with no check of the new value, so the temp bracket is torn down on ANY
#  change to a not-yet-locked asteroid -- including targeting turning ON.
#  Ctrl+click selects first (bracket made) and then starts the lock (bracket
#  closed), so the lock spinner and countdown never appear; the countdown
#  tasklet then dies on the closed bracket (bracket.py GetScanSpeed,
#  'NoneType' object has no attribute 'slimItem').  The right-click Lock
#  Target path happened to work only because nothing had selected the rock.
#
#  Fixed: a temp asteroid bracket is made on any of the three states turning
#  on, and cleared only once NONE of them hold -- checked a moment later, so
#  the targeting -> targeted handover in targetMgr.OnTargetAdded (targeting 0
#  is set before targeted 1) does not drop and rebuild it.
#
#  Everything is wrapped: if anything here fails the client starts exactly
#  as it would without the patch.
# ============================================================================

print ">>> GuriMoon asteroid lock bracket patch loading..."

try:
    Import('svc')

    _alOrig = svc.bracket.OnStateChange.im_func
    _alG = _alOrig.func_globals
    _alUix = _alG['uix']
    _alState = _alG['state']
    _alUthread = _alG['uthread']
    _alBlue = _alG['blue']
    _alAsteroid = 25
    try:
        _alAsteroid = const.categoryAsteroid
    except:
        pass

    def _ALKeep(itemID, S=_alState):
        (sel, ting, ted) = eve.LocalSvc('state').GetStates(itemID, [S.selected, S.targeting, S.targeted])
        if sel or ting or ted:
            return 1
        tgt = eve.LocalSvc('target')
        if tgt.IsTarget(itemID) or itemID in tgt.GetTargeting():
            return 1
        return 0

    def _ALSweep(mgr, itemID, _keep=_ALKeep, _blue=_alBlue):
        try:
            _blue.pyos.synchro.Sleep(500)
            if _keep(itemID):
                return
            bracket = mgr.GetBracket(itemID)
            if bracket:
                mgr.ClearBracket(itemID, bracket)
        except:
            import traceback
            traceback.print_exc()

    def _ALOnStateChange(self, itemID, flag, true, _orig=_alOrig, S=_alState, _uix=_alUix, _uthread=_alUthread, _sweep=_ALSweep, _ast=_alAsteroid):
        if flag in (S.selected, S.targeted, S.targeting):
            slimItem = _uix.GetBallparkRecord(itemID)
            if slimItem and slimItem.categoryID == _ast:
                bracket = self.GetBracket(itemID)
                if true:
                    if not bracket:
                        ball = eve.LocalSvc('michelle').GetBall(itemID)
                        if ball:
                            # Load_update reads every state off svc.state, so
                            # the new bracket already shows this change;
                            # passing it on again would start a second
                            # countdown on top of the first.
                            self.DoBallsAdded([(ball, slimItem)], ignoreAsteroids=0)
                        return
                    bracket.OnStateChange(itemID, flag, true)
                    return
                if bracket:
                    bracket.OnStateChange(itemID, flag, true)
                    _uthread.new(_sweep, self, itemID)
                return
        return _orig(self, itemID, flag, true)

    svc.bracket.OnStateChange = _ALOnStateChange
    print ">>>   brackets: asteroid lock-on spinner survives Ctrl+click"

    print ">>> GuriMoon asteroid lock bracket patch applied"

except:
    import traceback
    traceback.print_exc()
    print ">>> ERROR: GuriMoon asteroid lock bracket patch failed to apply (client unchanged)"
