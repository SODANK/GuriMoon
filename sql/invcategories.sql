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
-- Table structure for table `invcategories`
--

DROP TABLE IF EXISTS `invcategories`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `invcategories` (
  `categoryID` int(10) unsigned NOT NULL DEFAULT 0,
  `categoryName` varchar(100) NOT NULL DEFAULT '',
  `description` text DEFAULT NULL,
  `graphicID` int(11) DEFAULT NULL,
  PRIMARY KEY (`categoryID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `invcategories`
--

LOCK TABLES `invcategories` WRITE;
/*!40000 ALTER TABLE `invcategories` DISABLE KEYS */;
INSERT INTO `invcategories` VALUES (0,'#System','',NULL),(1,'Owner','',1),(2,'Celestial','',6),(3,'Station','',17),(4,'Material','',399),(5,'Accessories','',33),(6,'Ship','',38),(7,'Module','',67),(8,'Charge','',171),(9,'Blueprint','',194),(10,'Trading','',195),(11,'Entity','',NULL),(14,'Bonus','Character creation bonuses.  Like innate skills but genetic rather than learned.',0),(16,'Skill','Where all the skills go under.',0),(17,'Commodity','',0),(18,'Drone','Player owned and controlled drones.',0),(20,'Implant','Implant',0),(22,'Deployable','',0),(23,'Structure','Player owned structure related objects',0),(24,'Reaction','',0),(25,'Asteroid','',NULL);
/*!40000 ALTER TABLE `invcategories` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
