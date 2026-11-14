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
-- Table structure for table `tutorials`
--

DROP TABLE IF EXISTS `tutorials`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `tutorials` (
  `tutorialID` int(10) unsigned NOT NULL DEFAULT 0,
  `tutorialVName` varchar(100) NOT NULL DEFAULT '',
  `nextTutorialID` int(10) unsigned DEFAULT NULL,
  PRIMARY KEY (`tutorialID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci ROW_FORMAT=DYNAMIC;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `tutorials`
--

LOCK TABLES `tutorials` WRITE;
/*!40000 ALTER TABLE `tutorials` DISABLE KEYS */;
INSERT INTO `tutorials` VALUES (5,'Aura : Station Tutorial',6),(6,'Ship Hangar and boarding',7),(7,'Character Sheet',8),(8,'The Items Hangar',9),(9,'Reprocessing - Selling minerals',11),(11,'Wallet',12),(12,'Market',13),(13,'Fittings, station service',14),(14,'Map',16),(15,'Communication with other pilots',23),(16,'Undock',NULL),(17,'Aura : Flight tutorial',37),(18,'Navigation, manual',21),(19,'Navigation, Automatic',38),(21,'Overview',19),(23,'The Warp Drive',28),(24,'Reap the rewards',31),(27,'Aura : Being pod-killed',NULL),(28,'Attack the Pirate!',24),(31,'Activating Acceleration Gate',34),(32,'Assets',48),(33,'Corporations and applying',45),(34,'Engage more dangerous enemies',35),(35,'Mining',36),(36,'Attain the prize container',39),(37,'The Control Console',18),(38,'Lock/unlock & Concord',15),(39,'Returning home',40),(40,'Reprocess veldspar',32),(41,'Aura : Insurance',NULL),(42,'Aura : Cloning',NULL),(43,'Aura : Losing a ship',NULL),(44,'Agents',47),(45,'Help',44),(46,'Repairshop',33),(47,'Revealing the full UI',NULL),(48,'Map & travel planning',49),(49,'Use Assets to set destination',46),(50,'Aura : Creating a chat channel',NULL),(51,'Aura : Insufficient skill to fit a module',NULL);
/*!40000 ALTER TABLE `tutorials` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
