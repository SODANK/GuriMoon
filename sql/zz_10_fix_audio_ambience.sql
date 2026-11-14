-- fix_audio_ambience.sql
-- Repairs the three audio columns the RMR dump ships broken. Space audio is
-- entirely client-side (parklife/envAudioMgr.py buckets balls by invTypes.groupID
-- and plays a hardcoded sound per bucket); these columns are the only server-side
-- levers over any of it.
--
-- Idempotent and value-guarded, so it is safe to re-run and safe to apply after
-- reimporting sql/evegraphics.sql, sql/evemessages.sql or sql/dgmtypeattributes.sql.
--
-- All three catalogs are CRC32-versioned bulkdata cachables, so clients refetch on
-- their next login after a server restart. No client cache clearing needed.
--
-- REQUIRES cmd/audiorestore to have been run against the client install: the
-- station ambience wrappers below (res:/Sound/Station/StationAmb*.blue) do not
-- exist in any .stuff archive. See "Audio & Ambience" in CLAUDE.md.

-- ---------------------------------------------------------------------------
-- 1. Station ambience — eveGraphics.urlSound
-- ---------------------------------------------------------------------------
-- envAudioMgr's Station bucket is the loudest ambience in the game (gain 0.5,
-- 100 km, and the only bucket in omitDistances so it does not attenuate at all).
-- It is also the only bucket whose sound is data-driven: soundPaths['Station'] is
-- None, so the client falls through to cfg.evegraphics.Get(graphicID).urlSound.
--
-- Every file the shipped column points at is a husk. All 19 res:/Sound/Station/
-- Station*.blue and stationFlame.blue are 571/580-byte stubs -- a named
-- audio.SoundNode with an empty TrackSequencer and NO media reference at all.
-- Verified across all 11 .stuff archives: exactly one copy of each, nothing
-- shadowing them. eve.rot.GetCopy succeeds, Play() succeeds, silence comes out,
-- and no exception is logged -- which is why this reads as "stations are quiet"
-- rather than as an error.
--
-- The audio itself was always present, under res/sound/station/hangar/. The
-- StationAmb*.blue wrappers written by cmd/audiorestore re-point at it. This
-- preserves the original per-station-type mapping exactly -- 15 distinct sounds
-- across 5042 stations -- rather than flattening every station onto one hum.
--
-- eveGraphics rows are shared: 13 of the 37 station graphicIDs are also used by
-- groups 319/494 (Large Collidable Structure / Deadspace Overseer's Structure).
-- That is harmless. envAudioMgr:179 is the ONLY reader of urlSound in the entire
-- client, and it only reads it for const.groupStation.
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb01.blue' WHERE urlSound='res:/Sound/Station/Station01.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb02.blue' WHERE urlSound='res:/Sound/Station/Station02.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb03.blue' WHERE urlSound='res:/Sound/Station/Station03.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb04.blue' WHERE urlSound='res:/Sound/Station/Station04.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb05.blue' WHERE urlSound='res:/Sound/Station/Station05.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb07.blue' WHERE urlSound='res:/Sound/Station/Station07.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb08.blue' WHERE urlSound='res:/Sound/Station/Station08.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb11.blue' WHERE urlSound='res:/Sound/Station/Station11.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb12.blue' WHERE urlSound='res:/Sound/Station/Station12.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb13.blue' WHERE urlSound='res:/Sound/Station/Station13.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb15.blue' WHERE urlSound='res:/Sound/Station/Station15.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb16.blue' WHERE urlSound='res:/Sound/Station/Station16.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb17.blue' WHERE urlSound='res:/Sound/Station/Station17.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb19.blue' WHERE urlSound='res:/Sound/Station/Station19.blue';
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmbFL.blue' WHERE urlSound='res:/Sound/Station/stationFlame.blue';

-- Station types 06/09/10/14/18/20 have husks too but no station uses them, so
-- cmd/audiorestore builds no wrapper for them and nothing is repointed here.

-- The three conquerable Minmatar Service Outposts (stationTypeID 21646,
-- graphicID 2758) ship with NO sound authored at all. Giving them one is an
-- addition rather than a restoration, but a soundless station is a hole.
UPDATE eveGraphics SET urlSound='res:/Sound/Station/StationAmb02.blue'
  WHERE graphicID=2758 AND (urlSound IS NULL OR urlSound='');

-- ---------------------------------------------------------------------------
-- 2. Aura voice lines and UI cues — eveMessages.urlAudio
-- ---------------------------------------------------------------------------
-- 243 rows carry audio; 8 named a file absent from every archive and 2 held
-- something that is not a resource path at all.
--
-- NOTE: the no-audio sentinel in this column is the EMPTY STRING -- the column is
-- NOT NULL, so clearing a row with NULL fails under STRICT_TRANS_TABLES.
-- NOTE: a "?vol=0.3" suffix is a legitimate query string the client strips; rows
-- carrying one are fine and are deliberately untouched here.

-- Extension drift: the file ships as .ogg, the column says .wav.
UPDATE eveMessages SET urlAudio='res:/sound/ui/CCTerminateForGoodFemale.ogg'
  WHERE messageID='CCTerminateForGoodFemale' AND urlAudio='res:/sound/ui/CCTerminateForGoodFemale.wav';
UPDATE eveMessages SET urlAudio='res:/sound/ui/CCTerminateForGoodMale.ogg'
  WHERE messageID='CCTerminateForGoodMale'   AND urlAudio='res:/sound/ui/CCTerminateForGoodMale.wav';
UPDATE eveMessages SET urlAudio='res:/sound/ui/SwooshUp09.ogg'
  WHERE messageID='SwooshUp'                 AND urlAudio='res:/sound/ui/SwooshUp09.wav';

-- Named file exists in no archive; nearest shipped equivalent. Substitution.
UPDATE eveMessages SET urlAudio='res:/sound/ui/SwooshUp09.ogg'   -- SwooshUp08.wav absent
  WHERE messageID='Swoosh250'        AND urlAudio='res:/sound/ui/SwooshUp08.wav';
UPDATE eveMessages SET urlAudio='res:/sound/ui/CCMenter.wav'     -- Menter.wav absent; character-creation cue
  WHERE messageID='CCAncestrySelect' AND urlAudio='res:/sound/ui/Menter.wav';
UPDATE eveMessages SET urlAudio='res:/sound/ui/uihme01.wav'      -- uishort02e3.wav absent; UI notify blip
  WHERE messageID='NeoNotify'        AND urlAudio='res:/sound/ui/uishort02e3.wav';

-- Corrupt column contents, not resource paths.
--   CanNotSurrenderAsWarIsOver held a message BODY.
--   MktTransFailedNoShares held a message ID, and that message
--   (MktTransFailedNoMoneyAudio) has an empty urlAudio itself -- so there is
--   nothing for it to point at and the row is cleared rather than redirected.
UPDATE eveMessages SET urlAudio=''
  WHERE messageID='CanNotSurrenderAsWarIsOver'
    AND urlAudio='Can Not Surrender::You can not surrender because the war is over.';
UPDATE eveMessages SET urlAudio=''
  WHERE messageID='MktTransFailedNoShares' AND urlAudio='MktTransFailedNoMoneyAudio';

-- ---------------------------------------------------------------------------
-- 3. Ship booster graphics — dgmTypeAttributes attributeID 246 (gfxBoosterID)
-- ---------------------------------------------------------------------------
-- gfxBoosterID is a dgma attribute, NOT an invTypes column. ship.py FitBoosters
-- derives the engine sound from the booster graphic's url3D basename:
--   res:/Sound/Thruster/<basename>_l.blue   (_lc cruiser, _lb battleship)
-- and only booster_{blue,green,orange,red,yellow} have sound files.
--
-- These four point at "*2" graphics that have none, and FitBoosters calls
-- eve.rot.GetCopy on that path UNGUARDED -- unlike turret.py, which tests
-- ResFile.Open first. The miss throws and aborts the function before the booster
-- models, the trails and the gainCurve are built, so these hulls lose their
-- engine sound, their booster glow AND their trails together.
--
-- graphicID 3014 is additionally a corrupt row: "booster_.blue2.blue".
UPDATE dgmTypeAttributes SET valueInt=394, valueFloat=NULL   -- booster_.blue2  -> booster_blue
  WHERE attributeID=246 AND typeID=20316 AND valueInt=3014;  -- Pimp Basilisk
UPDATE dgmTypeAttributes SET valueInt=395, valueFloat=NULL   -- booster_yellow2 -> booster_yellow
  WHERE attributeID=246 AND typeID=20318 AND valueInt=3017;  -- Pimp Guardian
UPDATE dgmTypeAttributes SET valueInt=396, valueFloat=NULL   -- booster_green2  -> booster_green
  WHERE attributeID=246 AND typeID=20320 AND valueInt=3015;  -- Pimp Oneiros
UPDATE dgmTypeAttributes SET valueInt=397, valueFloat=NULL   -- booster_red2    -> booster_red
  WHERE attributeID=246 AND typeID=20322 AND valueInt=3016;  -- Pimp Scimitar
