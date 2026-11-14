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
-- Table structure for table `invmetagroups`
--

DROP TABLE IF EXISTS `invmetagroups`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `invmetagroups` (
  `metaGroupID` int(10) unsigned NOT NULL DEFAULT 0,
  `metaGroupName` varchar(100) NOT NULL DEFAULT '',
  `description` text DEFAULT NULL,
  `graphicID` int(11) DEFAULT NULL,
  PRIMARY KEY (`metaGroupID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `invmetagroups`
--

LOCK TABLES `invmetagroups` WRITE;
/*!40000 ALTER TABLE `invmetagroups` DISABLE KEYS */;
INSERT INTO `invmetagroups` VALUES (1,'Tech I','',NULL),(2,'Tech II','',NULL),(3,'Storyline','',NULL),(4,'Faction','',NULL),(5,'Officer','',NULL),(6,'Deadspace','Modules found in deadspace.',NULL),(7,'Frigates','',NULL),(8,'Elite Frigates','',NULL),(9,'Commander Frigates','',NULL),(10,'Destroyer','',NULL),(11,'Cruiser','',NULL),(12,'Elite Cruiser','',NULL),(13,'Commander Cruiser','',NULL);
/*!40000 ALTER TABLE `invmetagroups` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
