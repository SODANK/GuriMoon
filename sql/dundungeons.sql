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
-- Table structure for table `dundungeons`
--

DROP TABLE IF EXISTS `dundungeons`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `dundungeons` (
  `dungeonVID` int(11) NOT NULL AUTO_INCREMENT,
  `dungeonVName` varchar(255) NOT NULL,
  `factionID` int(11) NOT NULL,
  `version` int(11) NOT NULL DEFAULT 1,
  `status` int(11) NOT NULL DEFAULT 4,
  `dedLevel` tinyint(4) NOT NULL DEFAULT 0,
  PRIMARY KEY (`dungeonVID`)
) ENGINE=InnoDB AUTO_INCREMENT=2828 DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `dundungeons`
--

LOCK TABLES `dundungeons` WRITE;
/*!40000 ALTER TABLE `dundungeons` DISABLE KEYS */;
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (3,'Serpentis Drug Lab',500020,1,4,0);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (7,'Republic Fleet Base',500002,1,4,0);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (10,'Easy Pickings',500021,1,4,0);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (15,'Caldari Navy Scout Camp',500001,1,4,0);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (18,'Serpent\'s Venom',500020,1,4,0);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (19,'Madhare Showdown',500010,1,4,0);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (20,'Blood Enclave',500012,1,4,1);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (2824,'Cyberware Smuggling Hub',500021,1,4,2);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (2825,'Angel Hideout',500011,1,4,1);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (2826,'Guristas Raid Camp',500010,1,4,3);
INSERT INTO `dundungeons` (`dungeonVID`, `dungeonVName`, `factionID`, `version`, `status`, `dedLevel`) VALUES (2827,'Serpentis Outlet',500010,1,4,0);
/*!40000 ALTER TABLE `dundungeons` ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

