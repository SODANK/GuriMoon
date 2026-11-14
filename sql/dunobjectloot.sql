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
-- Table structure for table `dunobjectloot`
--

DROP TABLE IF EXISTS `dunobjectloot`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `dunobjectloot` (
  `lootID` int(11) NOT NULL AUTO_INCREMENT,
  `objectID` int(11) NOT NULL,
  `itemTypeID` int(11) NOT NULL,
  `minQty` int(11) NOT NULL DEFAULT 1,
  `maxQty` int(11) NOT NULL DEFAULT 1,
  `dropChance` double NOT NULL DEFAULT 1,
  PRIMARY KEY (`lootID`),
  UNIQUE KEY `uq_dunloot_obj_item` (`objectID`,`itemTypeID`),
  KEY `idx_dunloot_obj` (`objectID`)
) ENGINE=InnoDB AUTO_INCREMENT=48 DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `dunobjectloot`
--

LOCK TABLES `dunobjectloot` WRITE;
/*!40000 ALTER TABLE `dunobjectloot` DISABLE KEYS */;
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (1,206191,9957,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (2,206191,19686,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (3,206191,13244,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (4,206191,19687,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (5,206191,13219,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (6,206191,19688,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (7,206191,13225,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (8,206191,19690,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (9,206191,13222,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (10,206191,19689,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (11,206191,22534,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (12,206191,22535,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (14,206191,22570,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (15,206191,22571,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (16,206191,13258,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (17,206191,17871,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (18,206191,10204,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (19,206191,13245,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (20,206191,13221,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (21,206191,24632,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (22,206191,24641,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (23,206191,13247,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (24,206191,13218,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (25,206191,13287,1,1,0.2);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (26,206191,13284,1,1,0.2);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (27,206191,13283,1,1,0.2);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (28,206191,13286,1,1,0.2);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (29,206191,13285,1,1,0.2);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (30,206191,14296,1,1,0.2);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (31,206191,14298,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (32,206191,14297,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (33,206191,14299,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (34,206191,14295,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (35,206191,13260,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (36,206191,13265,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (37,206191,13251,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (38,206191,13255,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (39,206191,13259,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (40,206191,13239,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (41,206191,13254,1,1,0.1);
INSERT INTO `dunobjectloot` (`lootID`, `objectID`, `itemTypeID`, `minQty`, `maxQty`, `dropChance`) VALUES (42,206191,13232,1,1,0.1);
/*!40000 ALTER TABLE `dunobjectloot` ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

