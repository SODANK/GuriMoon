/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;
/*!40101 SET @OLD_CHARACTER_SET_RESULTS=@@CHARACTER_SET_RESULTS */;
/*!40101 SET @OLD_COLLATION_CONNECTION=@@COLLATION_CONNECTION */;
/*!50503 SET NAMES utf8mb4 */;
/*!40103 SET @OLD_TIME_ZONE=@@TIME_ZONE */;
/*!40103 SET TIME_ZONE='+00:00' */;
/*!40014 SET @OLD_UNIQUE_CHECKS=@@UNIQUE_CHECKS, UNIQUE_CHECKS=0 */;
/*!40014 SET @OLD_FOREIGN_KEY_CHECKS=@@FOREIGN_KEY_CHECKS, FOREIGN_KEY_CHECKS=0 */;
/*!40101 SET @OLD_SQL_MODE=@@SQL_MODE, SQL_MODE='NO_AUTO_VALUE_ON_ZERO' */;
/*!40111 SET @OLD_SQL_NOTES=@@SQL_NOTES, SQL_NOTES=0 */;

--
-- Table structure for table `npcbehaviorprofiles`
--
-- A profile is a role header. The behavior itself lives in `npctactics` rows
-- keyed to it. Range values are deliberately absent: orbit distance, aggro
-- range and leash range are derived at runtime from each NPC type's own weapon
-- attributes (entityFlyRange, entityAttackRange, entityChaseMaxDistance), so a
-- frigate and a battleship sharing a profile still fight at their own ranges.
--

DROP TABLE IF EXISTS `npcbehaviorprofiles`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `npcbehaviorprofiles` (
  `profileName` varchar(64) NOT NULL,
  `thinkIntervalMs` int(11) NOT NULL DEFAULT 2000,
  `movementIntervalMs` int(11) NOT NULL DEFAULT 1000,
  `movementMode` varchar(16) NOT NULL DEFAULT 'orbit',
  `autoAggro` tinyint(4) NOT NULL DEFAULT 1,
  `targetPreference` varchar(64) NOT NULL DEFAULT 'nearestPlayer',
  `maxTargets` int(11) NOT NULL DEFAULT 1,
  `description` varchar(255) DEFAULT NULL,
  PRIMARY KEY (`profileName`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `npcbehaviorprofiles`
--

LOCK TABLES `npcbehaviorprofiles` WRITE;
/*!40000 ALTER TABLE `npcbehaviorprofiles` DISABLE KEYS */;
INSERT INTO `npcbehaviorprofiles` VALUES
 ('Combat',250,1000,'orbit',1,'nearestPlayer',1,'Standard combat NPC - orbits and attacks nearest player, uses every electronic module its hull carries'),
 ('Relaxed',2000,1000,'orbit',0,'highestThreat',1,'Guard - ignores you until provoked, then fights back with everything'),
 ('sentry',2000,1000,'hold',1,'nearestPlayer',1,'Stationary turret - does not move, fires at targets in range'),
 ('approach',500,1000,'follow',0,'none',0,'Approaches nearest player but does not attack'),
 ('passive',500,1000,'hold',0,'none',0,'Does nothing - decoration or scripted NPC');
/*!40000 ALTER TABLE `npcbehaviorprofiles` ENABLE KEYS */;
UNLOCK TABLES;

--
-- Table structure for table `npctactics`
--
-- One row = one rule. The brain walks a profile's rules in `seq` order and
-- takes the first match on each channel (movement / combat / modules), so an
-- early targeting rule and a later movement rule both apply.
--
-- triggerType: always | hpBelow | shieldBelow | armorBelow | tookDamage |
--              targetCloserThan | targetFartherThan | jammed | engagedFor
-- actionType:  useEwar | noEwar | scram | jam | sensorDamp | trackingDisrupt |
--              targetPaint | capDrain            (modules)
--              engageNearest | engageHighestThreat | holdFire   (combat)
--              orbit | approach | hold | returnHome             (movement)
--
-- Electronic capability is NOT authored here. It comes off dgmTypeAttributes
-- per NPC type (entityWarpScrambleChance, entityTargetJam*, entitySensorDampen*,
-- entityTrackingDisrupt*, entityTargetPaint*, entityCapacitorDrain*). A tactic
-- only grants permission and timing; the hull decides what is on board.
--

DROP TABLE IF EXISTS `npctactics`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `npctactics` (
  `profileName` varchar(64) NOT NULL,
  `seq` int(11) NOT NULL,
  `triggerType` varchar(32) NOT NULL DEFAULT 'always',
  `triggerValue` double NOT NULL DEFAULT 0,
  `actionType` varchar(32) NOT NULL,
  `actionValue` double NOT NULL DEFAULT 0,
  PRIMARY KEY (`profileName`,`seq`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `npctactics`
--

LOCK TABLES `npctactics` WRITE;
/*!40000 ALTER TABLE `npctactics` DISABLE KEYS */;
INSERT INTO `npctactics` VALUES
 ('Combat',10,'always',0,'useEwar',0),
 ('sentry',10,'always',0,'useEwar',0),
 ('Relaxed',10,'tookDamage',0,'useEwar',0);
/*!40000 ALTER TABLE `npctactics` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
