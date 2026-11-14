-- jove_spaceship_command.sql — every Jove character at Spaceship Command V.
--
-- WHY
--
-- Three of the four Jove hull skills gate on Spaceship Command V (Jove
-- Frigate, Jove Industrial, Jove Battleship; Jove Cruiser on III). A Jove
-- created at the usual SSC I cannot begin to train for its own race's ships,
-- and at rank 1 the 256,000 SP between I and V is a week of training spent
-- before the rank-6 to rank-12 Jove skills can even start.
--
-- New Jove get SSC V from character creation now (db/chardb.go,
-- CreateCharacter: raceID 16). This brings the existing ones up to match.
-- On 2026-09-25 that was 203 race-16 characters: 153 already at V, 47 below,
-- 3 with no Spaceship Command item at all, 4 mid-training it.
--
-- WHAT IT TOUCHES
--
--   - the SSC skill item of every race-16 character: level (attr 280) to 5,
--     skill points (attr 276) to at least 256,000 (the rank-1 level-V total);
--   - a skill in training (flag 61) goes back to trained (flag 7), and its
--     chrskilltraining row is removed — otherwise the job completes later at
--     its own target level and writes IV over the V set here;
--   - queued SSC steps for these characters are removed: every one of them is
--     now already met;
--   - a race-16 character with no SSC item is given one, at V.
--
-- Nothing else about these characters changes. Idempotent: a character at V
-- is left exactly as it is.
--
-- REVERSIBLE. The prior level and SP of every touched item are kept in
-- jove_ssc_backup (written once per item); the undo is at the bottom.
-- A logged-in character sees the new level after relog.

CREATE TABLE IF NOT EXISTS jove_ssc_backup (
  itemID      INT(10) UNSIGNED NOT NULL PRIMARY KEY,
  characterID INT(10) UNSIGNED NOT NULL,
  flag        INT(11) NULL,
  level       INT(11) NULL,
  skillPoints BIGINT NULL,
  created     TINYINT(1) NOT NULL DEFAULT 0,
  savedAt     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
) ENGINE=InnoDB DEFAULT CHARSET=utf8;

-- ── 1. give an SSC item to the Jove who have none ───────────────────────────
INSERT INTO entity
  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
SELECT 'Skill', 3327, c.characterID, c.characterID, 7, 0, 1, 1, 0, 0, 0, NULL
FROM character_ c
WHERE c.raceID = 16
  AND NOT EXISTS (SELECT 1 FROM entity e
                  WHERE e.ownerID = c.characterID AND e.typeID = 3327 AND e.flag IN (7, 61));

INSERT IGNORE INTO jove_ssc_backup (itemID, characterID, flag, level, skillPoints, created)
SELECT e.itemID, e.ownerID, e.flag, NULL, NULL, 1
FROM entity e
JOIN character_ c ON c.characterID = e.ownerID AND c.raceID = 16
WHERE e.typeID = 3327 AND e.flag = 7
  AND NOT EXISTS (SELECT 1 FROM entity_attributes a WHERE a.itemID = e.itemID AND a.attributeID = 280);

-- ── 2. back up everything about to change ───────────────────────────────────
INSERT IGNORE INTO jove_ssc_backup (itemID, characterID, flag, level, skillPoints, created)
SELECT e.itemID, e.ownerID, e.flag, lv.valueInt, sp.valueInt, 0
FROM entity e
JOIN character_ c ON c.characterID = e.ownerID AND c.raceID = 16
LEFT JOIN entity_attributes lv ON lv.itemID = e.itemID AND lv.attributeID = 280
LEFT JOIN entity_attributes sp ON sp.itemID = e.itemID AND sp.attributeID = 276
WHERE e.typeID = 3327 AND e.flag IN (7, 61)
  AND (COALESCE(lv.valueInt, 0) < 5 OR e.flag = 61);

-- ── 3. stop in-flight SSC training and drop queued SSC steps ────────────────
DELETE t FROM chrskilltraining t
JOIN character_ c ON c.characterID = t.characterID AND c.raceID = 16
WHERE t.typeID = 3327;

DELETE q FROM chrskillqueue q
JOIN character_ c ON c.characterID = q.characterID AND c.raceID = 16
WHERE q.typeID = 3327;

UPDATE entity e
JOIN character_ c ON c.characterID = e.ownerID AND c.raceID = 16
SET e.flag = 7
WHERE e.typeID = 3327 AND e.flag = 61;

-- ── 4. level V ──────────────────────────────────────────────────────────────
-- Upsert both attributes; entity_attributes is keyed (itemID, attributeID).
INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
SELECT e.itemID, 280, 5, NULL
FROM entity e JOIN character_ c ON c.characterID = e.ownerID AND c.raceID = 16
WHERE e.typeID = 3327 AND e.flag = 7
ON DUPLICATE KEY UPDATE valueInt = 5, valueFloat = NULL;

INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
SELECT e.itemID, 276, 256000, NULL
FROM entity e JOIN character_ c ON c.characterID = e.ownerID AND c.raceID = 16
WHERE e.typeID = 3327 AND e.flag = 7
ON DUPLICATE KEY UPDATE valueInt = GREATEST(COALESCE(valueInt, 0), 256000), valueFloat = NULL;

-- ── undo (run by hand) ──────────────────────────────────────────────────────
-- UPDATE entity_attributes a JOIN jove_ssc_backup b ON b.itemID = a.itemID AND b.created = 0
--    SET a.valueInt = b.level WHERE a.attributeID = 280 AND b.level IS NOT NULL;
-- UPDATE entity_attributes a JOIN jove_ssc_backup b ON b.itemID = a.itemID AND b.created = 0
--    SET a.valueInt = b.skillPoints WHERE a.attributeID = 276 AND b.skillPoints IS NOT NULL;
-- DELETE a FROM entity_attributes a JOIN jove_ssc_backup b ON b.itemID = a.itemID AND b.created = 1;
-- DELETE e FROM entity e JOIN jove_ssc_backup b ON b.itemID = e.itemID AND b.created = 1;
-- (in-flight training and queued steps removed by step 3 are not restored)
-- DROP TABLE jove_ssc_backup;
