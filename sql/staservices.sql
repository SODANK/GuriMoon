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
-- Table structure for table `staservices`
--

DROP TABLE IF EXISTS `staservices`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `staservices` (
  `serviceID` int(10) unsigned NOT NULL DEFAULT 0,
  `serviceName` varchar(100) NOT NULL DEFAULT '',
  `description` mediumtext NOT NULL,
  PRIMARY KEY (`serviceID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `staservices`
--

LOCK TABLES `staservices` WRITE;
/*!40000 ALTER TABLE `staservices` DISABLE KEYS */;
INSERT INTO `staservices` VALUES (1,'Bounty Missions',''),(2,'Assassination Missions',''),(4,'Courier Missions',''),(8,'Interbus',''),(16,'Reprocessing Plant',''),(32,'Refinery',''),(64,'Market',''),(128,'Black Market',''),(256,'Stock Exchange',''),(512,'Cloning',''),(1024,'Surgery',''),(2048,'DNA Therapy',''),(4096,'Repair Facilities',''),(8192,'Factory',''),(16384,'Laboratory',''),(32768,'Gambling',''),(65536,'Fitting',''),(131072,'Paintshop',''),(262144,'News',''),(524288,'Storage',''),(1048576,'Insurance','Used to buy insurance for ships.'),(2097152,'Docking',''),(4194304,'Office Rental',''),(8388608,'Jump Clone Facility','');
/*!40000 ALTER TABLE `staservices` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
