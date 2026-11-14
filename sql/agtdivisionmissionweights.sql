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
-- Table structure for table `agtdivisionmissionweights`
--

DROP TABLE IF EXISTS `agtdivisionmissionweights`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `agtdivisionmissionweights` (
  `divisionID` int(11) NOT NULL,
  `killWeight` int(11) NOT NULL DEFAULT 50,
  `courierWeight` int(11) NOT NULL DEFAULT 50,
  `tradeWeight` int(11) NOT NULL DEFAULT 0,
  `miningWeight` int(11) NOT NULL DEFAULT 0,
  PRIMARY KEY (`divisionID`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `agtdivisionmissionweights`
--

LOCK TABLES `agtdivisionmissionweights` WRITE;
/*!40000 ALTER TABLE `agtdivisionmissionweights` DISABLE KEYS */;
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (1,5,90,5,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (2,50,50,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (3,50,50,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (4,5,90,5,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (5,40,30,10,20);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (6,97,3,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (7,5,90,5,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (8,5,45,50,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (9,85,15,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (10,95,5,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (11,50,50,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (12,10,10,10,70);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (13,5,45,50,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (14,5,10,5,80);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (15,50,50,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (16,10,10,10,70);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (17,50,50,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (18,50,50,0,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (19,90,5,5,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (20,5,90,5,0);
INSERT INTO `agtdivisionmissionweights` (`divisionID`, `killWeight`, `courierWeight`, `tradeWeight`, `miningWeight`) VALUES (21,95,5,0,0);
/*!40000 ALTER TABLE `agtdivisionmissionweights` ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

