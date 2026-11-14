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
-- Table structure for table `chrattributes`
--

DROP TABLE IF EXISTS `chrattributes`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `chrattributes` (
  `attributeID` int(10) unsigned NOT NULL DEFAULT 0,
  `attributeName` varchar(100) NOT NULL DEFAULT '',
  `description` mediumtext NOT NULL,
  `graphicID` int(10) unsigned NOT NULL DEFAULT 0,
  PRIMARY KEY (`attributeID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `chrattributes`
--

LOCK TABLES `chrattributes` WRITE;
/*!40000 ALTER TABLE `chrattributes` DISABLE KEYS */;
INSERT INTO `chrattributes` VALUES (1,'Intelligence','Intelligence describes the person\'s capacity for creative and logic-based thinking and reasoning. Intelligence helps in scientific pursuits and other activities requiring good deductive skills.',1380),(2,'Charisma','Charisma describes the person\'s attractiveness to other people and how easy going it is. High charisma helps in all dealings with computer-controlled characters.',1378),(3,'Perception','Perception reflect the situational awareness of the person and its ability to adapt and react quickly to a rapidly changing situation. Good Reflexes helps the person on the battlefield and in other situations where swiftness is of the essence.',1382),(4,'Memory','Memory describes the person\'s ability to store and retrieve data quickly and efficiently. Good memory makes complex and repetitive activities easier to perform, such as activities of manufacturing and production.',1381),(5,'Willpower','Willpower describes the person\'s drive and determination. Willpower is very useful for commanders and greatly enhances their leadership abilities.',1379);
/*!40000 ALTER TABLE `chrattributes` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
