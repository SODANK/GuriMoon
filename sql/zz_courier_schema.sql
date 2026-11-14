-- Courier contract schema migration.
-- Adds columns the player/bot courier system needs on top of the shipped
-- couriermissions table, plus the refType row that makes journal entries
-- visible.

-- ── columns ────────────────────────────────────────────────────────────────

-- Collateral the acceptor locks when accepting.
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'collateral');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN collateral DOUBLE NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Reward ISK the issuer locked at creation.
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'rewardEscrow');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN rewardEscrow DOUBLE NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Non-refundable listing fee.
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'listingFee');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN listingFee DOUBLE NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Escrow container holding the real items (invisible, ownerID=0).
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'crateContainerID');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN crateContainerID INT UNSIGNED DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Issuer's corporation at creation time.
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'issuerCorpID');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN issuerCorpID INT UNSIGNED DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Acceptor's corporation at accept time.
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'acceptorCorpID');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN acceptorCorpID INT UNSIGNED DEFAULT NULL',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Whether the issuer is a bot (for ESP filtering).
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'issuerIsBot');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN issuerIsBot TINYINT(1) NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Whether the acceptor is a bot (for ESP filtering).
SET @col = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND COLUMN_NAME = 'acceptorIsBot');
SET @sql = IF(@col = 0,
    'ALTER TABLE couriermissions ADD COLUMN acceptorIsBot TINYINT(1) NOT NULL DEFAULT 0',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- ── indexes ────────────────────────────────────────────────────────────────

-- Browse: available contracts at a station.
SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_courier_pickup_state');
SET @sql = IF(@idx = 0,
    'ALTER TABLE couriermissions ADD INDEX idx_courier_pickup_state (pickupStationID, state)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- My couriers (accepted).
SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_courier_acceptor_state');
SET @sql = IF(@idx = 0,
    'ALTER TABLE couriermissions ADD INDEX idx_courier_acceptor_state (acceptorID, state)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- My couriers (issued).
SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_courier_issuer_state');
SET @sql = IF(@idx = 0,
    'ALTER TABLE couriermissions ADD INDEX idx_courier_issuer_state (issuerID, state)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- Sweep: expired/failed contracts.
SET @idx = (SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
            WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
            AND INDEX_NAME = 'idx_courier_state');
SET @sql = IF(@idx = 0,
    'ALTER TABLE couriermissions ADD INDEX idx_courier_state (state)',
    'SELECT 1');
PREPARE stmt FROM @sql; EXECUTE stmt; DEALLOCATE PREPARE stmt;

-- ── refType ────────────────────────────────────────────────────────────────
-- Courier journal rows use the SHIPPED refType 23 "Courier Mission Escrow"
-- (db.CourierRefType); nothing to insert. 46 is the shipped "Broker fee".
