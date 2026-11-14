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
-- Table structure for table `dunrooms`
--

DROP TABLE IF EXISTS `dunrooms`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `dunrooms` (
  `roomID` int(11) NOT NULL AUTO_INCREMENT,
  `dungeonVID` int(11) NOT NULL,
  `roomName` varchar(255) NOT NULL,
  `roomOrder` int(11) DEFAULT NULL,
  `naturalPhenomena` tinyint(1) NOT NULL DEFAULT 1,
  PRIMARY KEY (`roomID`)
) ENGINE=InnoDB AUTO_INCREMENT=6858 DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `dunrooms`
--

LOCK TABLES `dunrooms` WRITE;
/*!40000 ALTER TABLE `dunrooms` DISABLE KEYS */;
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (4,3,'Perimeter',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (5,3,'Drug Lab',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (6,3,'Escape Route',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (10,7,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (13,10,'Merc Base',2,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (18,15,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (21,10,'Entrance',1,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (22,18,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (23,19,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (24,20,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (6854,2824,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (6855,2825,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (6856,2826,'Room 1',NULL,1);
INSERT INTO `dunrooms` (`roomID`, `dungeonVID`, `roomName`, `roomOrder`, `naturalPhenomena`) VALUES (6857,2827,'Room 1',NULL,1);
/*!40000 ALTER TABLE `dunrooms` ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

