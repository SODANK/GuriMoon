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
-- Table structure for table `everoles`
--

DROP TABLE IF EXISTS `everoles`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `everoles` (
  `roleID` int(10) unsigned NOT NULL DEFAULT 0,
  `roleName` varchar(100) NOT NULL DEFAULT '',
  `description` mediumtext NOT NULL,
  PRIMARY KEY (`roleName`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3 COLLATE=utf8mb3_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `everoles`
--

LOCK TABLES `everoles` WRITE;
/*!40000 ALTER TABLE `everoles` DISABLE KEYS */;
INSERT INTO `everoles` VALUES (536870912,'ACCOUNTMANAGEMENT','May manage user accounts'),(32,'ADMIN','Administrator'),(67108864,'BROADCAST','Broadcast messages in ESP'),(2048,'CENTURION','Role for senior volunteers (Polaris)'),(2097152,'CHTADMINISTRATOR','Administrate chat channels'),(1048576,'CHTINVISIBLE','Invisible in chat channels'),(1073741824,'DUNGEONMASTER','Design and master dungeons'),(16384,'EBS','Eve Backend System'),(1024,'GDH','Game Design High'),(512,'GDL','Game Design Low'),(4,'GDNPC','Game Design NPC'),(16,'GMH','Game Master High'),(8,'GML','Game Master Low'),(8388608,'HEALOTHERS','/heal on others'),(4194304,'HEALSELF','/heal on yourself'),(33554432,'HOSTING','Hardware hosting and administration'),(128,'HTTP','HTTP/ESP'),(262144,'LEGIONEER','Role for junior volunteers'),(1,'LOGIN',''),(268435456,'N00BIE','Newbie dude'),(16777216,'NEWSREPORTER','Forum and in-game news reporting role.'),(256,'PETITIONEE','Users that may handle petitions'),(2,'PLAYER',''),(65536,'PROGRAMMER','Role for executing Python code in the context of the server'),(8192,'QA','Quality Assurance'),(131072,'REMOTESERVICE',''),(32768,'ROLEADMIN','Administration of roles'),(64,'SERVICE',''),(4294967295,'SPAWN','Allowing /entity and /spawn'),(524288,'TRANSLATION','Permission to do translations in the translation system'),(134217728,'TRANSLATIONADMIN','Permission to do administer translations in the translation system'),(4294967295,'TRANSLATIONEDITOR','Approves or Rejects translations'),(4096,'WORLDMOD','World Modeling');
/*!40000 ALTER TABLE `everoles` ENABLE KEYS */;
UNLOCK TABLES;

/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;
