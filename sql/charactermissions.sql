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
-- Table structure for table `charactermissions`
--

DROP TABLE IF EXISTS `charactermissions`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `charactermissions` (
  `missionID` bigint(20) NOT NULL AUTO_INCREMENT,
  `charID` int(11) NOT NULL,
  `agentID` int(11) NOT NULL,
  `templateID` int(11) NOT NULL,
  `state` enum('offered','accepted','completed','failed','declined') NOT NULL DEFAULT 'offered',
  `destSystemID` int(11) DEFAULT NULL,
  `destStationID` int(11) DEFAULT NULL,
  `pickupStationID` int(11) DEFAULT NULL,
  `dropoffStationID` int(11) DEFAULT NULL,
  `bookmarkX` double DEFAULT NULL,
  `bookmarkY` double DEFAULT NULL,
  `bookmarkZ` double DEFAULT NULL,
  `rewardISK` bigint(20) DEFAULT 0,
  `rewardLP` int(11) DEFAULT 0,
  `bonusISK` bigint(20) DEFAULT 0,
  `killProgress` int(11) DEFAULT 0,
  `killRequired` int(11) DEFAULT 0,
  `acceptedAt` datetime DEFAULT NULL,
  `expiresAt` datetime DEFAULT NULL,
  `completedAt` datetime DEFAULT NULL,
  `bonusDeadline` datetime DEFAULT NULL,
  PRIMARY KEY (`missionID`),
  KEY `idx_char_agent` (`charID`,`agentID`),
  KEY `idx_char_state` (`charID`,`state`)
) ENGINE=InnoDB AUTO_INCREMENT=5 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
