-- Escrow schema: standalone escrows table.
-- Replaces the old approach of storing escrows in couriermissions with kind=2.

CREATE TABLE IF NOT EXISTS escrows (
  escrowID            INT UNSIGNED NOT NULL AUTO_INCREMENT,
  state               TINYINT      NOT NULL DEFAULT 0,
  description         VARCHAR(255) NOT NULL DEFAULT '',
  issuerID            BIGINT       NOT NULL,
  assigneeID          BIGINT       DEFAULT NULL,
  collateral          DOUBLE       NOT NULL DEFAULT 0,
  dateIssued          BIGINT       NOT NULL DEFAULT 0,
  dateExpires         BIGINT       NOT NULL DEFAULT 0,
  dateAccepted        BIGINT       NOT NULL DEFAULT 0,
  dateCompleted       BIGINT       NOT NULL DEFAULT 0,
  pickupStationID     BIGINT       NOT NULL DEFAULT 0,
  pickupSolarSystemID BIGINT       NOT NULL DEFAULT 0,
  pickupRegionID      BIGINT       NOT NULL DEFAULT 0,
  crateContainerID    INT UNSIGNED DEFAULT NULL,
  issuerCorpID        INT UNSIGNED DEFAULT NULL,
  acceptorID          BIGINT       DEFAULT NULL,
  acceptorCorpID      INT UNSIGNED DEFAULT NULL,
  issuerIsBot         TINYINT(1)   NOT NULL DEFAULT 0,
  acceptorIsBot       TINYINT(1)   NOT NULL DEFAULT 0,
  listingFee          DOUBLE       NOT NULL DEFAULT 0,
  volume              DOUBLE       NOT NULL DEFAULT 0,
  PRIMARY KEY (escrowID),
  INDEX idx_escrow_state_station (state, pickupStationID),
  INDEX idx_escrow_state_region  (state, pickupRegionID),
  INDEX idx_escrow_state_system  (state, pickupSolarSystemID)
) ENGINE=InnoDB AUTO_INCREMENT=1000000000 DEFAULT CHARSET=utf8mb4;
-- AUTO_INCREMENT: escrow IDs live in their own band, disjoint from
-- couriermissions.missionID. The client's AcceptCourierMission / GetManifest
-- carry a bare ID for both, so an overlap routes one to the other.
-- db.EnsureEscrowSchema migrates rows created before this band existed.

-- Escrow sales journal under the SHIPPED refType 1 "Player Trading"
-- (db.EscrowRefType); nothing to insert. 47 is the shipped
-- "Corporation Bulk Payment".

-- Drop old escrow indexes from couriermissions (they served kind=2 queries
-- that no longer exist). Safe to run even if the indexes are already gone.
SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_escrow_kind_state_station');
SET @sql = IF(@idx > 0,
    'ALTER TABLE couriermissions DROP INDEX idx_escrow_kind_state_station',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_escrow_kind_state_region');
SET @sql = IF(@idx > 0,
    'ALTER TABLE couriermissions DROP INDEX idx_escrow_kind_state_region',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_escrow_kind_state_system');
SET @sql = IF(@idx > 0,
    'ALTER TABLE couriermissions DROP INDEX idx_escrow_kind_state_system',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_escrow_kind_state');
SET @sql = IF(@idx > 0,
    'ALTER TABLE couriermissions DROP INDEX idx_escrow_kind_state',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_escrow_region');
SET @sql = IF(@idx > 0,
    'ALTER TABLE couriermissions DROP INDEX idx_escrow_region',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;
