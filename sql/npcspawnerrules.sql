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
-- Table structure for table `npcspawnerrules`
--
-- How many ships a belt wave holds at a given security. WHAT spawns comes from
-- npcRegionSpawns; these rules only size the wave. Each belt rolls its own
-- count between minShips and maxShips, then fills every slot by weighted pick
-- from the region's eligible listings.
--

DROP TABLE IF EXISTS `npcspawnerrules`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `npcspawnerrules` (
  `ruleID` int(11) NOT NULL AUTO_INCREMENT,
  `ruleName` varchar(128) NOT NULL,
  `secMin` double NOT NULL DEFAULT -1,
  `secMax` double NOT NULL DEFAULT 1,
  `minShips` int(11) NOT NULL DEFAULT 0,
  `maxShips` int(11) NOT NULL DEFAULT 3,
  `description` varchar(255) DEFAULT NULL,
  PRIMARY KEY (`ruleID`)
) ENGINE=InnoDB AUTO_INCREMENT=6 DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `npcspawnerrules`
--

LOCK TABLES `npcspawnerrules` WRITE;
/*!40000 ALTER TABLE `npcspawnerrules` DISABLE KEYS */;
INSERT INTO `npcspawnerrules` VALUES (1,'High-sec Safe',0.9,1.1,0,2,'Safe high-sec belts (0.9-1.0)'),(2,'High-sec',0.7,0.9,1,3,'Standard high-sec belts (0.7-0.9)'),(3,'Mid-sec',0.5,0.7,1,4,'Mid-security belts (0.5-0.7)'),(4,'Low-sec',0.1,0.5,1,5,'Low-sec belts (0.1-0.5)'),(5,'Null-sec',-1,0.1,2,7,'Null-sec belts (-1.0-0.1)');
/*!40000 ALTER TABLE `npcspawnerrules` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
