-- Charges auto-fitted into modules that take no ammo.
--
-- bound.Add's auto-fit path treated "module declares no chargeGroup1..5" as
-- "accepts anything", so dragging an ammo stack onto the fitting window
-- loaded one unit into every fitted module — afterburners, hardeners, plates.
-- The client renders those slots with the ammo icon and a quantity of 1.
--
-- This moves any such charge back into the ship's cargo hold (flag 5).
-- Run the SELECT first to see what will move.

SELECT ch.itemID, ch.typeID AS chargeType, ch.quantity, ch.locationID AS shipID,
       ch.flag, m.typeID AS moduleType
FROM entity ch
JOIN invTypes  ct ON ct.typeID  = ch.typeID
JOIN invGroups cg ON cg.groupID = ct.groupID AND cg.categoryID = 8
JOIN entity    m  ON m.locationID = ch.locationID AND m.flag = ch.flag
JOIN invTypes  mt ON mt.typeID  = m.typeID
JOIN invGroups mg ON mg.groupID = mt.groupID AND mg.categoryID <> 8
WHERE ch.flag BETWEEN 11 AND 34
  AND NOT EXISTS (
        SELECT 1 FROM dgmTypeAttributes dta
        WHERE dta.typeID = m.typeID
          AND dta.attributeID IN (604, 605, 606, 609, 610)
          AND COALESCE(NULLIF(dta.valueFloat, 0), dta.valueInt) > 0);

UPDATE entity ch
JOIN invTypes  ct ON ct.typeID  = ch.typeID
JOIN invGroups cg ON cg.groupID = ct.groupID AND cg.categoryID = 8
JOIN entity    m  ON m.locationID = ch.locationID AND m.flag = ch.flag
JOIN invTypes  mt ON mt.typeID  = m.typeID
JOIN invGroups mg ON mg.groupID = mt.groupID AND mg.categoryID <> 8
SET ch.flag = 5, ch.singleton = 0
WHERE ch.flag BETWEEN 11 AND 34
  AND NOT EXISTS (
        SELECT 1 FROM dgmTypeAttributes dta
        WHERE dta.typeID = m.typeID
          AND dta.attributeID IN (604, 605, 606, 609, 610)
          AND COALESCE(NULLIF(dta.valueFloat, 0), dta.valueInt) > 0);
