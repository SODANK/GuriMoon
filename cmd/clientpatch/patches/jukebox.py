# ============================================================================
#  GuriMoon client patch: jukebox playlist
#
#  Installed by cmd/clientpatch in the GuriMoon server repository and run
#  from script/my_autoexec.py.  Do not edit this copy -- rerun the patcher.
#  Python 2.3 (Stackless): no decorators, no generator expressions, no `with`,
#  no conditional expressions, no set().
#
#  The Jukebox plays whatever svc.audio loaded from
#  res:/sound/music/inflight.m3u (audioService.ProcessSessionChange; nothing in
#  the client ever sets the 'playList' override).  That list names 49 of the
#  Ambient tracks and nothing else, so 31 tracks the client ships were never
#  playable from it:
#
#    - the dungeon/combat cues in resDungeons.stuff (Ambient031combat,
#      Ambient046combat, Ambient010beat3, Ambient017beat2) -- named by no
#      client script, playlist or .blue; only a server-sent gate dunMusicUrl
#      could ever play them;
#    - the racial themes, EVE_Rock_01-05 and EVE_Theme_01 in resMusic.stuff
#      -- likewise named by nothing in any of the 11 archives or the scripts;
#    - Ambient001 / Ambient042, named only by login.m3u, which nothing loads;
#    - ShivaTheme003d, the login screen track (login2.m3u).
#
#  Fixed: Playlist.Load appends those tracks after reading inflight.m3u, and
#  they are SELECT-ONLY -- listed in the Jukebox, played when clicked, never
#  chosen by the rotation:
#    - InitializeEveAmp shuffles after Load; the shuffled list is then split
#      so the regular tracks come first (track 0, which every session change
#      starts on, is always one of them) and the added ones sit at the bottom.
#    - AmpCallback (a track ended), Advance and the Jukebox's Next / Prev skip
#      added tracks.  A clicked one plays to its end and the rotation resumes
#      at the next regular track.
#  A track already on the list is not added twice, and a file this client
#  does not have is skipped (checked once, on first use).
#
#  Everything is wrapped: if anything here fails the client starts exactly
#  as it would without the patch.
# ============================================================================

print ">>> GuriMoon jukebox playlist patch loading..."

try:
    Import('svc')

    _jbG = svc.audio.InitializeEveAmp.im_func.func_globals
    _jbPlaylist = _jbG['Playlist']
    _jbBlue = _jbG['blue']
    _jbOrigLoad = _jbPlaylist.Load.im_func

    # (file, title, seconds).  Titles for the variants name the inflight.m3u
    # track they are built on; seconds are read off each file's last Ogg page.
    _jbExtra = [
        ('Ambient031combat.ogg', 'At the End of Your Journey (combat)', 349),
        ('Ambient046combat.ogg', "It's Full of Stars (combat)", 300),
        ('Ambient010beat3.ogg', 'Forsaken Ruins (beat)', 367),
        ('Ambient017beat2.ogg', 'Safe Trade Routes (beat)', 344),
        ('ShivaTheme003d.ogg', 'Shiva Theme', 295),
        ('Ambient001.ogg', 'Stellar Shadows', 360),
        ('Ambient042.ogg', 'Ambient 042', 387),
        ('EVE_Theme_01.ogg', 'EVE Theme', 253),
        ('EVE_Rock_01.ogg', 'EVE Rock 1', 370),
        ('EVE_Rock_02.ogg', 'EVE Rock 2', 324),
        ('EVE_Rock_03.ogg', 'EVE Rock 3', 298),
        ('EVE_Rock_04.ogg', 'EVE Rock 4', 260),
        ('EVE_Rock_05.ogg', 'EVE Rock 5', 335),
        ('Amarr001.ogg', 'Amarr Theme 1', 324),
        ('Amarr002.ogg', 'Amarr Theme 2', 318),
        ('Amarr003.ogg', 'Amarr Theme 3', 303),
        ('Caldari001.ogg', 'Caldari Theme 1', 231),
        ('Caldari002.ogg', 'Caldari Theme 2', 243),
        ('Caldari003.ogg', 'Caldari Theme 3', 378),
        ('Caldari004.ogg', 'Caldari Theme 4', 352),
        ('Gallente001.ogg', 'Gallente Theme 1', 288),
        ('Gallente002.ogg', 'Gallente Theme 2', 306),
        ('Gallente003.ogg', 'Gallente Theme 3', 309),
        ('Gallente004.ogg', 'Gallente Theme 4', 184),
        ('Gallente005.ogg', 'Gallente Theme 5', 280),
        ('Gallente006.ogg', 'Gallente Theme 6', 329),
        ('Gallente007.ogg', 'Gallente Theme 7', 277),
        ('Minmatar001.ogg', 'Minmatar Theme 1', 288),
        ('Minmatar002.ogg', 'Minmatar Theme 2', 205),
        ('Minmatar003.ogg', 'Minmatar Theme 3', 297),
        ('Minmatar004.ogg', 'Minmatar Theme 4', 293),
    ]
    _jbPresent = []  # filled on first Load: the _jbExtra rows this client has

    def _JBAvailable(_blue=_jbBlue, _extra=_jbExtra, _present=_jbPresent):
        if not _present:
            for row in _extra:
                f = _blue.ResFile()
                try:
                    if f.Open('res:/Sound/Music/' + row[0]):
                        _present.append(row)
                        f.Close()
                except:
                    pass
            if not _present:
                _present.append(None)  # nothing found: do not look again
        if _present[0] is None:
            return []
        return _present

    def _JBLoad(self, playlist, _orig=_jbOrigLoad, _avail=_JBAvailable):
        ret = _orig(self, playlist)
        try:
            if str(playlist).lower().endswith('inflight.m3u'):
                seen = {}
                for item in self.playlist:
                    seen[str(item[0]).lower()] = 1
                for (fn, title, length) in _avail():
                    if not seen.has_key(fn.lower()):
                        self.AddTrack(fn, title, float(length))
        except:
            import traceback
            traceback.print_exc()
        return ret

    _jbExtraNames = {}  # lowercased file -> position in _jbExtra
    for _jbI in range(len(_jbExtra)):
        _jbExtraNames[_jbExtra[_jbI][0].lower()] = _jbI + 1

    def _JBIsExtra(fn, _names=_jbExtraNames):
        return _names.has_key(str(fn).lower())

    def _JBNextRegular(lst, start, step, _isExtra=_JBIsExtra):
        # First index after `start` (stepping by +1 or -1, wrapping) whose
        # track is not an added one; None if every track is.
        n = len(lst)
        i = start
        for k in range(n):
            i = (i + step) % n
            if not _isExtra(lst[i][0]):
                return i
        return None

    _jbOrigInit = svc.audio.InitializeEveAmp.im_func

    def _JBInitializeEveAmp(self, plist, flags='', gain=-1, _orig=_jbOrigInit, _isExtra=_JBIsExtra, _names=_jbExtraNames):
        ret = _orig(self, plist, flags, gain)
        try:
            regular = []
            extra = []
            for item in self.playlist.playlist:
                if _isExtra(item[0]):
                    extra.append(item)
                else:
                    regular.append(item)
            if extra and regular:
                # Added tracks in their authored order, so the bottom of the
                # list reads as a fixed menu rather than a second shuffle.
                keyed = []
                for item in extra:
                    keyed.append((_names[str(item[0]).lower()], item))
                keyed.sort()
                extra = []
                for (k, item) in keyed:
                    extra.append(item)
                self.playlist.playlist[:] = regular + extra
                self.current = 0
        except:
            import traceback
            traceback.print_exc()
        return ret

    _jbOrigAmp = svc.audio.AmpCallback.im_func

    def _JBAmpCallback(self, cookie, _orig=_jbOrigAmp, _next=_JBNextRegular):
        try:
            lst = self.playlist.GetList()
            if lst:
                n = _next(lst, self.current, 1)
                if n is not None:
                    self.current = (n - 1) % len(lst)  # orig plays current+1
        except:
            import traceback
            traceback.print_exc()
        return _orig(self, cookie)

    _jbOrigAdvance = svc.audio.Advance.im_func

    def _JBAdvance(self, direction='n', _orig=_jbOrigAdvance, _next=_JBNextRegular):
        try:
            lst = self.playlist.GetList()
            if lst:
                if direction == 'n':
                    n = _next(lst, self.current, 1)
                    if n is not None:
                        self.current = (n - 1) % len(lst)  # orig plays current+1
                elif direction == 'p':
                    n = _next(lst, self.current, -1)
                    if n is not None:
                        self.current = (n + 1) % len(lst)  # orig plays current-1
        except:
            import traceback
            traceback.print_exc()
        return _orig(self, direction)

    _jbPlaylist.Load = _JBLoad
    svc.audio.InitializeEveAmp = _JBInitializeEveAmp
    svc.audio.AmpCallback = _JBAmpCallback
    svc.audio.Advance = _JBAdvance

    # The Jukebox's Next / Prev step through its own list entries and call
    # PlayTrack directly when a row is selected, bypassing Advance.
    Import('form')

    def _JBStep(self, step, _isExtra=_JBIsExtra):
        entries = self.sr.playlist.sr.entries
        current = self.sr.playlist.GetSelected()
        if not current or not entries:
            return 0
        audioSvc = eve.LocalSvc('audio')
        lst = audioSvc.GetPlaylist().GetList()
        idx = current[0].idx
        for k in range(len(entries)):
            idx = (idx + step) % len(entries)
            ti = entries[idx].trackIndex
            if ti < len(lst) and not _isExtra(lst[ti][0]):
                audioSvc.PlayTrack(ti)
                self.sr.playlist.ShowEntry(idx)
                return 1
        return 0

    _jbOrigNext = form.Jukebox.OnClickNext.im_func
    _jbOrigPrev = form.Jukebox.OnClickPrev.im_func

    def _JBOnClickNext(self, sender, _orig=_jbOrigNext, _step=_JBStep):
        try:
            if _step(self, 1):
                return
        except:
            import traceback
            traceback.print_exc()
        return _orig(self, sender)

    def _JBOnClickPrev(self, sender, _orig=_jbOrigPrev, _step=_JBStep):
        try:
            if _step(self, -1):
                return
        except:
            import traceback
            traceback.print_exc()
        return _orig(self, sender)

    form.Jukebox.OnClickNext = _JBOnClickNext
    form.Jukebox.OnClickPrev = _JBOnClickPrev
    print ">>>   jukebox: combat cues, racial themes and EVE Rock listed (select-only, not in rotation)"

    print ">>> GuriMoon jukebox playlist patch applied"

except:
    import traceback
    traceback.print_exc()
    print ">>> ERROR: GuriMoon jukebox playlist patch failed to apply (client unchanged)"
