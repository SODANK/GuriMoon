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
-- Table structure for table `chrraces`
--

DROP TABLE IF EXISTS `chrraces`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `chrraces` (
  `raceID` int(10) unsigned NOT NULL DEFAULT 0,
  `raceName` varchar(128) NOT NULL DEFAULT '',
  `description` text DEFAULT NULL,
  `skillTypeID1` int(11) DEFAULT NULL,
  `typeID` int(11) DEFAULT NULL,
  `typeQuantity` int(11) DEFAULT NULL,
  `graphicID` int(11) DEFAULT NULL,
  PRIMARY KEY (`raceID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `chrraces`
--

LOCK TABLES `chrraces` WRITE;
/*!40000 ALTER TABLE `chrraces` DISABLE KEYS */;
INSERT INTO `chrraces` VALUES (1,'Caldari','Ruled by a handful of mega corporations, Caldari State is the epitome of civil duty and efficiency, where ruthless capitalism leaves no room for human rights or feelings. They are brutally efficient fighters, giving their opponents no chance of retaliation. Through grit and superior technology, the State fought the much larger Gallente Federation to a standstill.',3330,215,100,1439),(2,'Minmatar','The undeniable ultimate underdogs of EVE are the Minmatars. Once enslaved by the Amarr Empire for centuries, the Minmatar Nation is now reveling in its newfound freedom, fighting for its claim as a major power. Steeped in a deep-rooted tribal society, the Minmatars display vehement attachment to their kindred. They will never forget their lost brothers and sisters enslaved by the Amarr Empire.',3329,178,100,1440),(4,'Amarr','Amarr is a vast and ancient empire, with the Amarr Emperor as the head of this feudal society. It is rich in history and traditions, with a caste system that is cruel to those on the bottom, but kind to those at the top. Centuries ago during the Reclaiming, the Amarr Empire enslaved several nations, including the Minmatars, but has calmed down in recent times.',3331,NULL,NULL,1442),(8,'Gallente','The Gallente Federation is the only,1,democracy in EVE. The Gallenteans regard themselves as the champions of freedom and liberty. Their human touch makes them excellent traders and media moguls. Forged in fire in a war against the Caldari State, the Federation still depends on its mighty drone fleets to defend its beliefs against encroaching anarchy.',3328,215,100,1441),(16,'Jove','The most mysterious and elusive of all the peoples of EVE, the Jovians number only a fraction of any of their neighbors, but their technological superiority makes them powerful beyond all proportion.',3330,NULL,NULL,NULL),(32,'Pirate','',NULL,NULL,NULL,NULL);
/*!40000 ALTER TABLE `chrraces` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
