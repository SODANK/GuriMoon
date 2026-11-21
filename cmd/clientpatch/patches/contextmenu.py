# ============================================================================
#  GuriMoon client patch: info-window / link context menus
#
#  Installed by cmd/clientpatch in the GuriMoon server repository and run
#  from script/my_autoexec.py.  Do not edit this copy -- rerun the patcher.
#  Python 2.3 (Stackless): no decorators, no generator expressions, no `with`,
#  no conditional expressions, no set().
#
#  svc.menu.GetMenuFormItemIDTypeID builds the menu behind the little arrow
#  in an info window title bar (infosvc.GetActionMenu passes
#  filterFunc=['Show Info'], since the window already is "Show Info") and
#  behind owner / type links in lists.  It only has real menus for characters,
#  celestials / ships / structures, and for inventory items (invItem).  For
#  anything else its last branch reads
#
#      elif not (filterFunc and 'Show Info' in filterFunc):
#          return [Show Info]
#      return                      <- None
#
#  so a corporation or a plain type (no invItem: opened from the market, a
#  browser link, a blueprint, ...) opens an arrow with nothing in it.
#
#  Fixed: when the original returns no entries (or only Show Info), add
#
#    plain types   View market details, Add to / Remove from market quickbar
#    corporations  Apply to Corp (not your own corp), Set Standing
#                  (from you, and from your corp when it is a player corp)
#
#  using the client's own ShowMarketDetails / quickbar settings /
#  SetPlayerStanding / SetCorporationStanding / corp InsertApplication.
#  Anything the client already builds is returned untouched.
#
#  Everything is wrapped: if anything here fails the client starts exactly
#  as it would without the patch.
# ============================================================================

print ">>> GuriMoon context menu patch loading..."

try:
    Import('svc')

    _cmOrig = svc.menu.GetMenuFormItemIDTypeID.im_func
    _cmG = _cmOrig.func_globals
    _cmUix = _cmG['uix']
    _cmUthread = _cmG['uthread']
    _cmShowInfo = 'Show Info'
    try:
        _cmShowInfo = _cmG['ENTRY_SHOWINFO']
    except:
        pass

    def _cmIsNPC(ownerID):
        try:
            return _cmG['util'].IsNPC(ownerID)
        except:
            return 0

    def _cmApplyCheck(retval):
        if retval.has_key('appltext'):
            text = retval['appltext']
            if len(text) > 1000:
                return 'The specified application text is too long (%s characters). The text can be a maximum of 1000 characters long.' % len(text)
        return ''

    def _cmApplyToCorp(corpID):
        # Same flow as the station lobby's "Apply for membership" button.
        corp = eve.LocalSvc('corp')
        if eve.session.corpid == corpID:
            raise UserError('CanNotJoinCorpAlreadyAMember', {'corporation': (cfg.eveowners.Get(eve.session.corpid).name)})
        if corp.UserIsCEO():
            raise UserError('CeoCanNotJoinACorp', {'CEOsCorporation': (cfg.eveowners.Get(eve.session.corpid).name), 'otherCorporation': (cfg.eveowners.Get(corpID).name)})
        applications = corp.GetMyApplications(corpID)
        if applications is not None and len(applications) != 0:
            eve.LocalSvc('corpui').ViewApplication(corpID)
            return
        corpName = cfg.eveowners.Get(corpID).name
        tax = int(corp.GetCorporation(corpID).taxRate * 100)
        format = [{'type': 'header', 'text': ('Apply for membership to <b>%s</b>' % corpName), 'frame': 1},
                  {'type': 'push'},
                  {'type': 'textedit', 'key': 'appltext', 'label': 'Application text', 'frame': 1, 'maxLength': 1000},
                  {'type': 'push'},
                  {'type': 'btline'},
                  {'type': 'push'},
                  {'type': 'text', 'text': ('The current tax rate for %s is <b>%s%%</b>.' % (corpName, tax))}]
        format.append({'type': 'errorcheck', 'errorcheck': _cmApplyCheck})
        retval = _cmUix.HybridWnd(format, 'Join Corporation', 1, None, _cmUix.OKCANCEL, None, 400, 200, icon='07_06')
        if retval is not None:
            _cmUthread.new(corp.InsertApplication, corpID, retval['appltext'])

    def _cmApplyEntry(corpID):
        # Run on its own tasklet: the dialog blocks.
        _cmUthread.new(_cmApplyToCorp, corpID)

    def _cmCorpEntries(menu, corpID):
        entries = []
        if eve.session.corpid != corpID:
            entries.append(('Apply to Corp', _cmApplyEntry, (corpID,)))
        name = cfg.eveowners.Get(corpID).name
        if eve.session.corpid and eve.session.corpid != corpID and not _cmIsNPC(eve.session.corpid):
            standing = [('from you to %s\x85' % name, menu.SetPlayerStanding, (corpID,)),
                        ('from %s to %s\x85' % (cfg.eveowners.Get(eve.session.corpid).name, name), menu.SetCorporationStanding, (corpID,))]
            entries.append(('Set Standing', standing))
        else:
            entries.append(('Set Standing\x85', menu.SetPlayerStanding, (corpID,)))
        return entries

    def _cmTypeEntries(menu, typeID):
        entries = []
        try:
            inBar = typeID in settings.user.ui.Get('marketquickbar', [])
        except:
            inBar = 0
        entries.append(('View market details', menu.ShowMarketDetailsForType, (typeID,)))
        if inBar:
            entries.append(('Remove from market quickbar', menu.RemoveFromQuickBar, (typeID,)))
        else:
            entries.append(('Add to market quickbar', menu.AddToQuickBar, (typeID,)))
        return entries

    def _cmShowMarketDetailsForType(self, typeID):
        _cmUthread.new(sm.StartService('marketutils').ShowMarketDetails, typeID, None)

    svc.menu.ShowMarketDetailsForType = _cmShowMarketDetailsForType

    def _cmOnlyShowInfo(res):
        if not res:
            return 1
        for each in res:
            if each is None:
                continue
            if each[0] != _cmShowInfo:
                return 0
        return 1

    def _cmGetMenu(self, itemID, typeID, bookmark=None, filterFunc=None, invItem=None, _orig=_cmOrig):
        res = _orig(self, itemID, typeID, bookmark, filterFunc, invItem)
        try:
            if invItem is None and typeID is not None and _cmOnlyShowInfo(res):
                extra = None
                if typeID == const.typeCorporation and itemID:
                    extra = _cmCorpEntries(self, itemID)
                elif typeID != const.typeCorporation and typeID != const.typeAlliance and cfg.invtypes.Get(typeID).Group().categoryID != getattr(const, 'categoryOwner', -1):
                    extra = _cmTypeEntries(self, typeID)
                if extra:
                    if res:
                        res = list(res) + [None] + extra
                    else:
                        res = extra
        except:
            import traceback
            traceback.print_exc()
        return res

    svc.menu.GetMenuFormItemIDTypeID = _cmGetMenu
    print ">>>   menus: market / corp entries for info-window arrows"

    print ">>> GuriMoon context menu patch applied"

except:
    import traceback
    traceback.print_exc()
    print ">>> ERROR: GuriMoon context menu patch failed to apply (client unchanged)"
