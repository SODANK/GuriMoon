# ============================================================================
#  GuriMoon client patch: Research Facility (Duplicating / Reverse Engineering)
#
#  Installed by cmd/clientpatch in the GuriMoon server repository and run
#  from script/my_autoexec.py.  Do not edit this copy -- rerun the patcher.
#  Python 2.3 (Stackless): no decorators, no generator expressions, no `with`,
#  no conditional expressions, no set().
#
#  The client ships the whole legacy Research Facility window
#  (ui/station/researchfacility, ui/station/facilities/facility.py) -- the only
#  UI it has for Duplicating and Reverse Engineering -- but its station lobby
#  button list (svc.station.GetStationServiceInfo) has no entry for it.  This
#  adds that one entry; the lobby already shows it only at stations whose
#  service mask has the Laboratory service.
#
#  Two defensive fixes ride along, both in code the button makes reachable:
#    - After renting a slot the window enables INSTALL but never selects the
#      new slot, so Install would use whatever slot was selected before.
#    - A corporation slot opened by a pilot without the Factory Manager role
#      or an office leaves the install dialog with no hangar, and the item
#      list crashes on None.List().  It now lists nothing instead.
#
#  Everything is wrapped: if anything here fails the client starts exactly
#  as it would without the patch.
# ============================================================================

print ">>> GuriMoon research facility patch loading..."

try:
    Import('svc')
    Import('form')

    # ------------------------------------------------------------------
    #  1) The lobby button.
    # ------------------------------------------------------------------
    _rfOrigInfo = svc.station.GetStationServiceInfo.im_func
    _rfStationGlobals = _rfOrigInfo.func_globals

    def _RFGetStationServiceInfo(self, _orig=_rfOrigInfo, _g=_rfStationGlobals):
        services = _orig(self)
        try:
            lab = 16384
            if _g.has_key('const'):
                lab = getattr(_g['const'], 'stationServiceLaboratory', 16384)
            for each in services:
                if each[0] == 'researchfacility':
                    return services
            services = list(services)
            at = len(services)
            i = 0
            for each in services:
                i = i + 1
                if each[0] == 'factories':
                    at = i
            services.insert(at, ('researchfacility', '33_02', (lab,), 1))
        except:
            import traceback
            traceback.print_exc()
        return services

    svc.station.GetStationServiceInfo = _RFGetStationServiceInfo
    print ">>>   station lobby: Research Facility button added (Laboratory stations)"

    # ------------------------------------------------------------------
    #  2) Select the slot that was just rented.
    # ------------------------------------------------------------------
    _rfOrigRent = svc.researchfacility.RentSlot.im_func

    def _RFRentSlot(self, cost, forCorpUse, _orig=_rfOrigRent):
        slotID = _orig(self, cost, forCorpUse)
        if slotID:
            self.sel_slotID = slotID
        return slotID

    svc.researchfacility.RentSlot = _RFRentSlot
    print ">>>   research facility: rented slot is selected for install"

    # ------------------------------------------------------------------
    #  3) No hangar -> an empty list instead of a crash.
    # ------------------------------------------------------------------
    class _RFNoHangar:
        def List(self, *args):
            return []

    _rfOrigHangar = form.InstallProductionJobDlg.GetHangarToUse.im_func

    def _RFGetHangarToUse(self, _orig=_rfOrigHangar, _empty=_RFNoHangar):
        hangar = _orig(self)
        if hangar is None:
            return _empty()
        return hangar

    form.InstallProductionJobDlg.GetHangarToUse = _RFGetHangarToUse
    print ">>>   research facility: install dialog tolerates a missing corp hangar"

    print ">>> GuriMoon research facility patch applied"

except:
    import traceback
    traceback.print_exc()
    print ">>> ERROR: GuriMoon research facility patch failed to apply (client unchanged)"
