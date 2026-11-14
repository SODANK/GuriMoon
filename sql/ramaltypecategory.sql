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
-- Table structure for table `ramaltypecategory`
--

DROP TABLE IF EXISTS `ramaltypecategory`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `ramaltypecategory` (
  `assemblyLineTypeID` int(10) unsigned NOT NULL DEFAULT 0,
  `activityID` int(10) unsigned NOT NULL DEFAULT 0,
  `categoryID` int(10) unsigned NOT NULL DEFAULT 0,
  `timeMultiplier` double NOT NULL DEFAULT 0,
  `materialMultiplier` double NOT NULL DEFAULT 0,
  PRIMARY KEY (`assemblyLineTypeID`,`categoryID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `ramaltypecategory`
--

LOCK TABLES `ramaltypecategory` WRITE;
/*!40000 ALTER TABLE `ramaltypecategory` DISABLE KEYS */;
INSERT INTO `ramaltypecategory` VALUES (2,5,6,5,5),(2,5,7,1,1),(2,5,8,1,1),(2,5,18,1,1),(3,7,7,1,1),(5,5,9,1,1),(6,1,2,1,1),(6,1,4,1,1),(6,1,5,1,1),(6,1,7,1,1),(6,1,8,1,1),(6,1,17,1,1),(6,1,18,1,1),(6,1,20,1,1),(6,1,22,1,1),(7,4,9,1,1),(8,3,9,1,1),(11,4,6,1,1),(11,4,7,1,1),(11,4,8,1,1),(11,4,18,1,1),(12,3,6,1,1),(12,3,7,1,1),(12,3,8,1,1),(12,3,18,1,1),(13,1,6,1,1),(13,1,7,1,1),(13,1,8,1,1),(13,1,18,1,1),(23,1,2,1,1),(23,1,4,1,1),(23,1,5,1,1),(23,1,7,1,1),(23,1,17,1,1),(23,1,20,1,1),(23,1,22,1,1),(24,1,2,1,1),(24,1,4,1,1),(24,1,5,1,1),(24,1,7,1,1),(24,1,20,1,1),(24,1,22,1,1),(25,1,8,1,1),(28,4,9,1,1),(29,3,9,1,1),(30,5,9,1,1),(31,1,2,1,1),(31,1,4,1,1),(31,1,5,1,1),(31,1,7,1,1),(31,1,8,1,1),(31,1,17,1,1),(31,1,18,1,1),(31,1,20,1,1),(31,1,22,1,1),(32,4,9,1,1),(33,3,9,1,1),(34,5,9,1,1),(35,1,2,1,1),(35,1,4,1,1),(35,1,5,1,1),(35,1,7,1,1),(35,1,8,1,1),(35,1,17,1,1),(35,1,18,1,1),(35,1,20,1,1),(35,1,22,1,1);
/*!40000 ALTER TABLE `ramaltypecategory` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
