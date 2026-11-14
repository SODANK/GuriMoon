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
-- Table structure for table `ramaltypes`
--

DROP TABLE IF EXISTS `ramaltypes`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `ramaltypes` (
  `assemblyLineTypeID` int(10) unsigned NOT NULL DEFAULT 0,
  `assemblyLineTypeName` varchar(100) NOT NULL DEFAULT '',
  `typeName` varchar(100) NOT NULL DEFAULT '',
  `description` varchar(100) NOT NULL DEFAULT '',
  `activityID` int(10) unsigned NOT NULL DEFAULT 0,
  `baseTimeMultiplier` double NOT NULL DEFAULT 0,
  `baseMaterialMultiplier` double NOT NULL DEFAULT 0,
  `volume` double NOT NULL DEFAULT 0,
  PRIMARY KEY (`assemblyLineTypeID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `ramaltypes`
--

LOCK TABLES `ramaltypes` WRITE;
/*!40000 ALTER TABLE `ramaltypes` DISABLE KEYS */;
INSERT INTO `ramaltypes` VALUES (2,'Lab Slot','Lab Slot','Copying',5,1,1,1),(3,'Reverse Engineering','Reverse Engineering','Reverse Engineering',7,1,1,1),(5,'STATION copying','STATION copying','STATION copying',5,1,1,1),(6,'STATION manufacturing','STATION manufacturing','STATION manufacturing',1,1,1,1),(7,'STATION material productivity','STATION material productivity','STATION material productivity',4,1,1,1),(8,'STATION productivity time','STATION productivity time','STATION productivity time',3,1,1,1),(10,'Capital Ship Assembly','Capital Ship Assembly','Capital Ship Assembly',1,1,1,1),(11,'ME Research','ME Research','ME Research',4,1,1,1),(12,'PE Research','PE Research','PE Research',3,1,1,1),(13,'Manufacturing','Manufacturing','Manufacturing',1,1,1,1),(17,'Small Ship Assembly Array','Small Ship Assembly Array','Small Ship Assembly Array\r\n',1,0.75,1,1),(18,'Advanced Small Ship Assembly Array','Advanced Small Ship Assembly Array','Advanced Small Ship Assembly Array\r\n',1,0.75,1.1,1),(19,'Medium Ship Assembly Array','Medium Ship Assembly Array','Medium Ship Assembly Array\r\n',1,0.75,1,1),(20,'Advanced Medium Ship Assembly Array','Advanced Medium Ship Assembly Array','Advanced Medium Ship Assembly Array\r\n',1,0.75,1.1,1),(21,'Large Ship Assembly Array','Large Ship Assembly Array','Large Ship Assembly Array\r\n',1,0.75,1,1),(22,'Advanced Large Ship Assembly Array','Advanced Large Ship Assembly Array','Advanced Large Ship Assembly Array\r\n',1,0.75,1.1,1),(23,'Rapid Equipment Assembly Array','Rapid Equipment Assembly Array','Rapid Equipment Assembly Array\r\n',1,0.65,1.2,1),(24,'Efficient Equipment Assembly Array','Efficient Equipment Assembly Array','Efficient Equipment Assembly Array\r\n',1,0.8,1,1),(25,'Ammunition Assembly Array','Ammunition Assembly Array','Ammunition Assembly Array',1,0.75,1,1),(26,'Drone Assembly Array','Drone Assembly Array','Drone Assembly Array',1,0.75,1,1),(27,'Component Assembly Array','Component Assembly Array','Component Assembly Array',1,0.75,1.1,1),(28,'Mobile Laboratory ME','Mobile Laboratory ME','Mobile Laboratory ME',4,0.75,1,1),(29,'Mobile Laboratory PE','Mobile Laboratory PE','Mobile Laboratory PE',3,0.75,1,1),(30,'Mobile Laboratory Copying','Mobile Laboratory Copying','Mobile Laboratory Copying',5,0.75,1,1),(31,'Amarr Outpost Manufacturing','Amarr Outpost Manufacturing','Amarr Outpost Manufacturing',1,0.7,1,1),(32,'Caldari Outpost ME Research','Caldari Outpost ME Research','Caldari Outpost ME Research',4,0.7,0.95,1),(33,'Caldari Outpost PE Research','Caldari Outpost PE Research','Caldari Outpost PE Research',3,0.7,0.95,1),(34,'Caldari Outpost Copying','Caldari Outpost Copying','Caldari Outpost Copying',5,0.7,0.95,1),(35,'STATION 0.5+ Manufacturing','STATION 0.5+ Manufacturing','STATION 0.5+ Manufacturing',1,1,1,1),(37,'Booster Manufacturing','Booster Manufacturing','Booster Manufacturing',1,1,1,1);
/*!40000 ALTER TABLE `ramaltypes` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
