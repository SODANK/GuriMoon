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
-- Table structure for table `ramactivities`
--

DROP TABLE IF EXISTS `ramactivities`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `ramactivities` (
  `activityID` int(10) unsigned NOT NULL DEFAULT 0,
  `activityName` varchar(100) NOT NULL DEFAULT '',
  `iconNo` varchar(200) DEFAULT NULL,
  `description` mediumtext NOT NULL,
  `published` int(10) unsigned NOT NULL DEFAULT 0,
  PRIMARY KEY (`activityID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `ramactivities`
--

LOCK TABLES `ramactivities` WRITE;
/*!40000 ALTER TABLE `ramactivities` DISABLE KEYS */;
INSERT INTO `ramactivities` VALUES (0,'None',NULL,'No activity',1),(1,'Manufacturing','18_02','Manufacturing',1),(2,'Researching Technology','33_02','Technological research',0),(3,'Researching Time Productivity','33_02','Researching productivity time',1),(4,'Researching Material Productivity','33_02','Researching material productivity',1),(5,'Copying','33_02','Copying',1),(6,'Duplicating',NULL,'The process of creating an item, by studying an already existing item.',0),(7,'Reverse Engineering',NULL,'The process of creating a blueprint from an item.',0),(8,'Invention','33_02','The process of creating a more advanced item based on an existing item',0);
/*!40000 ALTER TABLE `ramactivities` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
