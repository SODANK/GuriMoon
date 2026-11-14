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
-- Table structure for table `duntriggers`
--

DROP TABLE IF EXISTS `duntriggers`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `duntriggers` (
  `triggerID` int(11) NOT NULL AUTO_INCREMENT,
  `roomID` int(11) NOT NULL,
  `event` varchar(32) NOT NULL,
  `targetObjectID` int(11) DEFAULT NULL,
  `action` varchar(32) NOT NULL,
  `header` varchar(255) DEFAULT NULL,
  `icon` varchar(64) DEFAULT NULL,
  `body` text DEFAULT NULL,
  `senderName` varchar(128) DEFAULT NULL,
  `deferUntilLanded` tinyint(4) NOT NULL DEFAULT 0,
  `onceOnly` tinyint(4) NOT NULL DEFAULT 1,
  `delayMs` int(11) NOT NULL DEFAULT 0,
  `spawnWave` int(11) DEFAULT NULL,
  `chatColor` varchar(16) DEFAULT NULL,
  `itemTypeID` int(11) DEFAULT NULL,
  `itemQty` int(11) NOT NULL DEFAULT 1,
  PRIMARY KEY (`triggerID`),
  KEY `idx_room` (`roomID`)
) ENGINE=InnoDB AUTO_INCREMENT=15 DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `duntriggers`
--

LOCK TABLES `duntriggers` WRITE;
/*!40000 ALTER TABLE `duntriggers` DISABLE KEYS */;
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (1,21,'roomEnter',NULL,'transmission',NULL,'3810','This is a restricted area Capsuleer. If you take that gate, we\'ll be forced to treat you as a hostile.','Mercenary',0,1,0,NULL,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (4,22,'npcKilled',339,'localChat',NULL,NULL,'The Serpentis drug dealer running this spot has emerged out of hiding. ',NULL,0,1,0,NULL,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (5,22,'npcKilled',339,'spawnWave',NULL,NULL,NULL,NULL,0,1,0,1,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (6,23,'roomEnter',NULL,'transmission','Transmission','1543','Looking for some smoke, egger? Come get some!',NULL,1,1,0,NULL,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (7,13,'npcKilled',321,'spawnWave',NULL,NULL,NULL,NULL,0,1,0,1,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (8,13,'npcKilled',327,'spawnWave',NULL,NULL,NULL,NULL,0,1,0,2,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (9,13,'npcKilled',321,'localChat',NULL,NULL,'Another training squad arrived from patrol!',NULL,0,1,0,NULL,'DE2121',NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (10,13,'npcKilled',327,'localChat',NULL,NULL,'The Head instructor for this training camp has undocked!',NULL,0,1,0,NULL,'DE2121',NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (11,6854,'roomEnter',NULL,'transmission',NULL,'2224','The DED has made reports of a massive smuggling ring being ran by unknown parties emerging across New Eden. Moving a variety of implants, these ominous locations defended by mercenaries should still have a few augmentations on hand in their storage hanger awaiting a runner. CONCORD has wavered all assets as spoils to whoever clears them out.',NULL,0,1,0,NULL,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (12,6855,'roomEnter',NULL,'transmission',NULL,'1540','The Angel Cartel are using this outpost to lay-low from law-enforcement in the area. Intel suggests Cartel Domination agents use these locations when they\'re \"hot\" and being actively chased by bounty hunters or the local navy. ',NULL,0,1,0,NULL,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (13,24,'roomEnter',NULL,'transmission',NULL,'1550','It\'s commonplace for the vicious Blood Raider cultists to take root in downtrodden asteroid colonies with neglected populace and find new converts among the impoverished. Local athorities see this as a massive threat to national security and implore Capsuleers to eradicate the officer running this recruitment center',NULL,0,1,0,NULL,NULL,NULL,1);
INSERT INTO `duntriggers` (`triggerID`, `roomID`, `event`, `targetObjectID`, `action`, `header`, `icon`, `body`, `senderName`, `deferUntilLanded`, `onceOnly`, `delayMs`, `spawnWave`, `chatColor`, `itemTypeID`, `itemQty`) VALUES (14,6856,'roomEnter',NULL,'transmission',NULL,'1543','Militant Guristas pirates have setup a fortified - yet cheap - staging post to launch attacks from in the constellation. Local Authorities are imploring Capsuleers in the area to secure this location by removing it\'s defenders. Intel suggests the Guristas having lucrative armaments stashed on site in their base\'s hangers',NULL,0,1,0,NULL,NULL,NULL,1);
/*!40000 ALTER TABLE `duntriggers` ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

