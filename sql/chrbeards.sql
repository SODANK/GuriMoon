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
-- Table structure for table `chrbeards`
--

DROP TABLE IF EXISTS `chrbeards`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `chrbeards` (
  `beardID` int(10) unsigned NOT NULL DEFAULT 0,
  `beardName` varchar(100) NOT NULL DEFAULT '',
  PRIMARY KEY (`beardID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `chrbeards`
--

LOCK TABLES `chrbeards` WRITE;
/*!40000 ALTER TABLE `chrbeards` DISABLE KEYS */;
INSERT INTO `chrbeards` VALUES (1,'1'),(2,'10'),(3,'11'),(4,'12'),(5,'13'),(6,'14'),(7,'15'),(8,'16'),(9,'17'),(10,'2'),(11,'22'),(12,'4'),(13,'7'),(14,'8'),(15,'9'),(16,'None'),(17,'18'),(18,'3'),(19,'5'),(20,'6'),(21,'Heavy black'),(22,'Musket'),(23,'Mustache'),(24,'19'),(25,'20'),(26,'21'),(27,'23'),(28,'24'),(29,'25'),(30,'Unnamed'),(31,'Unshaven'),(32,'Unshaven Moustache'),(33,'Dark Moustache'),(34,'Dark Long Moustache'),(35,'Shaven'),(36,'Moustache'),(37,'Grey Moustache');
/*!40000 ALTER TABLE `chrbeards` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
