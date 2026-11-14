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
-- Table structure for table `missiontemplates`
--

DROP TABLE IF EXISTS `missiontemplates`;
/*!40101 SET @saved_cs_client     = @@character_set_client */;
/*!50503 SET character_set_client = utf8mb4 */;
CREATE TABLE `missiontemplates` (
  `templateID` int(11) NOT NULL AUTO_INCREMENT,
  `missionName` varchar(255) NOT NULL,
  `missionType` enum('kill','courier','trade','mining') NOT NULL DEFAULT 'kill',
  `agentLevel` int(11) NOT NULL DEFAULT 1,
  `storyline` tinyint(1) NOT NULL DEFAULT 0,
  `briefingText` text NOT NULL,
  `objectiveText` text DEFAULT NULL,
  `acceptText` text DEFAULT NULL,
  `completeText` text DEFAULT NULL,
  `failText` text DEFAULT NULL,
  `declineText` text DEFAULT NULL,
  `moreInfoText` text DEFAULT NULL,
  `acceptLabel` varchar(64) DEFAULT NULL,
  `declineLabel` varchar(64) DEFAULT NULL,
  `viewMissionLabel` varchar(64) DEFAULT NULL,
  `moreInfoLabel` varchar(64) DEFAULT NULL,
  `completeLabel` varchar(64) DEFAULT NULL,
  `quitMissionLabel` varchar(64) DEFAULT NULL,
  `dungeonVID` int(11) DEFAULT NULL,
  `dungeonRoomIdx` int(11) DEFAULT 1,
  `completionType` enum('killAll','killTarget','killStructure','killItem') DEFAULT 'killAll',
  `targetTypeID` int(11) DEFAULT NULL,
  `targetCount` int(11) DEFAULT 1,
  `killItemTypeID` int(11) DEFAULT NULL,
  `killItemQty` int(11) DEFAULT 1,
  `killItemDropoffMode` enum('agent','specific','nearby') DEFAULT 'agent',
  `killItemDropoffStationID` int(11) DEFAULT NULL,
  `enemyFactionID` int(11) DEFAULT NULL,
  `courierTypeID` int(11) DEFAULT NULL,
  `courierQty` int(11) DEFAULT 1,
  `pickupMode` enum('agent','specific','nearby') DEFAULT 'agent',
  `pickupStationID` int(11) DEFAULT NULL,
  `dropoffMode` enum('agent','specific','nearby') DEFAULT 'nearby',
  `dropoffStationID` int(11) DEFAULT NULL,
  `tradeTypeID` int(11) DEFAULT NULL,
  `tradeQty` int(11) DEFAULT 1,
  `miningTypeID` int(11) DEFAULT NULL,
  `miningQty` int(11) DEFAULT 1,
  `minSecurity` float DEFAULT NULL,
  `maxSecurity` float DEFAULT NULL,
  `baseRewardISK` bigint(20) DEFAULT 0,
  `baseRewardLP` int(11) DEFAULT 0,
  `baseBonusISK` bigint(20) DEFAULT 0,
  `baseRewardItemTypeID` int(11) DEFAULT NULL,
  `baseRewardItemQty` int(11) DEFAULT 1,
  `bonusRewardItemTypeID` int(11) DEFAULT NULL,
  `bonusRewardItemQty` int(11) DEFAULT 1,
  `bonusTimeSec` int(11) DEFAULT 0,
  `expiryDays` int(11) NOT NULL DEFAULT 7,
  `standingGain` double DEFAULT 0,
  `corpStandingGain` double DEFAULT 0,
  `factionStandingGain` double DEFAULT 0,
  `standingLoss` double DEFAULT 0,
  `factionStandingLoss` double DEFAULT 0,
  `enabled` tinyint(1) DEFAULT 1,
  `allowedFactions` varchar(255) DEFAULT NULL,
  `nextMissionID` int(11) DEFAULT NULL,
  PRIMARY KEY (`templateID`)
) ENGINE=InnoDB AUTO_INCREMENT=7 DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_general_ci;
/*!40101 SET character_set_client = @saved_cs_client */;

--
-- Dumping data for table `missiontemplates`
--

LOCK TABLES `missiontemplates` WRITE;
/*!40000 ALTER TABLE `missiontemplates` DISABLE KEYS */;
INSERT INTO `missiontemplates` (`templateID`, `missionName`, `missionType`, `agentLevel`, `storyline`, `briefingText`, `objectiveText`, `acceptText`, `completeText`, `failText`, `declineText`, `moreInfoText`, `acceptLabel`, `declineLabel`, `viewMissionLabel`, `moreInfoLabel`, `completeLabel`, `quitMissionLabel`, `dungeonVID`, `dungeonRoomIdx`, `completionType`, `targetTypeID`, `targetCount`, `killItemTypeID`, `killItemQty`, `killItemDropoffMode`, `killItemDropoffStationID`, `enemyFactionID`, `courierTypeID`, `courierQty`, `pickupMode`, `pickupStationID`, `dropoffMode`, `dropoffStationID`, `tradeTypeID`, `tradeQty`, `miningTypeID`, `miningQty`, `minSecurity`, `maxSecurity`, `baseRewardISK`, `baseRewardLP`, `baseBonusISK`, `baseRewardItemTypeID`, `baseRewardItemQty`, `bonusRewardItemTypeID`, `bonusRewardItemQty`, `bonusTimeSec`, `expiryDays`, `standingGain`, `corpStandingGain`, `factionStandingGain`, `standingLoss`, `factionStandingLoss`, `enabled`, `allowedFactions`, `nextMissionID`) VALUES (1,'Test','courier',1,0,'Briefing Test','Objective Text Test','Acceptance test','Completion Test',NULL,'Decline Test',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,1,'killAll',NULL,1,NULL,1,'agent',NULL,NULL,23539,1,'agent',NULL,'nearby',NULL,NULL,1,NULL,1,NULL,NULL,1000000,500,20000,9850,1555,9850,2000,40000,7,0.1,0.08,0.01,-0.05,-0.02,1,'500010',NULL);
INSERT INTO `missiontemplates` (`templateID`, `missionName`, `missionType`, `agentLevel`, `storyline`, `briefingText`, `objectiveText`, `acceptText`, `completeText`, `failText`, `declineText`, `moreInfoText`, `acceptLabel`, `declineLabel`, `viewMissionLabel`, `moreInfoLabel`, `completeLabel`, `quitMissionLabel`, `dungeonVID`, `dungeonRoomIdx`, `completionType`, `targetTypeID`, `targetCount`, `killItemTypeID`, `killItemQty`, `killItemDropoffMode`, `killItemDropoffStationID`, `enemyFactionID`, `courierTypeID`, `courierQty`, `pickupMode`, `pickupStationID`, `dropoffMode`, `dropoffStationID`, `tradeTypeID`, `tradeQty`, `miningTypeID`, `miningQty`, `minSecurity`, `maxSecurity`, `baseRewardISK`, `baseRewardLP`, `baseBonusISK`, `baseRewardItemTypeID`, `baseRewardItemQty`, `bonusRewardItemTypeID`, `bonusRewardItemQty`, `bonusTimeSec`, `expiryDays`, `standingGain`, `corpStandingGain`, `factionStandingGain`, `standingLoss`, `factionStandingLoss`, `enabled`, `allowedFactions`, `nextMissionID`) VALUES (2,'Third Shift','courier',1,0,'Yeah I got something for you.\r\n\r\nSecurity team for {stationName} is due for their next shift. They live here because it\'s cheaper and take an Interbus shuttle in-between the job... Pain in the ass, but hey - That\'s their life. Problem is, they missed their shuttle ride and are in need of a lift... Think you can help them out? ','Transport the increasingly late security team to {stationName}','\"Nice! You may have just saved these people\'s livelihood!\"','\"Heard they got a verbal lashing from their bosses. But all in all? That\'s nothing. Good work Capsuleer, payment has been sent.',NULL,'\"What? You\'re too good for that? What you\'d think I was gonna offer you...\"',NULL,NULL,NULL,NULL,NULL,NULL,NULL,NULL,1,'killAll',NULL,1,NULL,1,'agent',NULL,NULL,3810,12,'agent',NULL,'nearby',NULL,NULL,1,NULL,1,NULL,NULL,10000,100,2500,NULL,1,NULL,1,3600,7,0.1,0.02,0.01,-0.01,-0.02,1,'500003,500007,500011,500001,500006,500004,500010,500008,500002,500014,500020,500012,500016,500009,500015',NULL);
INSERT INTO `missiontemplates` (`templateID`, `missionName`, `missionType`, `agentLevel`, `storyline`, `briefingText`, `objectiveText`, `acceptText`, `completeText`, `failText`, `declineText`, `moreInfoText`, `acceptLabel`, `declineLabel`, `viewMissionLabel`, `moreInfoLabel`, `completeLabel`, `quitMissionLabel`, `dungeonVID`, `dungeonRoomIdx`, `completionType`, `targetTypeID`, `targetCount`, `killItemTypeID`, `killItemQty`, `killItemDropoffMode`, `killItemDropoffStationID`, `enemyFactionID`, `courierTypeID`, `courierQty`, `pickupMode`, `pickupStationID`, `dropoffMode`, `dropoffStationID`, `tradeTypeID`, `tradeQty`, `miningTypeID`, `miningQty`, `minSecurity`, `maxSecurity`, `baseRewardISK`, `baseRewardLP`, `baseBonusISK`, `baseRewardItemTypeID`, `baseRewardItemQty`, `bonusRewardItemTypeID`, `bonusRewardItemQty`, `bonusTimeSec`, `expiryDays`, `standingGain`, `corpStandingGain`, `factionStandingGain`, `standingLoss`, `factionStandingLoss`, `enabled`, `allowedFactions`, `nextMissionID`) VALUES (3,'Easy Pickings','kill',1,0,'Yeah I got something for you\r\n\r\n{charName}, we got an issue. We\'ve started seeing a spike in mercenary activity in the area of late and they\'re starting to cause trouble. Miners being ransomed, convoys being ransacked. We don\'t know whose paying them - and frankly - That\'s not why we\'re talking.\r\n\r\nI have a lead on one of their \"Scrub camps\" in a secured deadspace pocket not too far from here in the {systemName} system. The location serves as a training ground for greener pilots to figure out how to fly their ship before they get shipped off on a job elsewhere in {regionName}. You\'re gonna go in and smother these pilots out before that can happen. Dishonorable? Maybe. But these are mercenaries we\'re dealing with... They knew what they signed up for.\r\n\r\n\r\n','Clear out the Merc training ground of recruits and instructors.','Easy money, Am I right?','Not bad flying {charName}, you could go far! {corporationName} Thanks you for your service.',NULL,'Well who do you expect me to find to do this?',NULL,'Bully some newbie pilots for cash? Consider it done.','I\'m better than this. I\'m out.','I need to know more...',NULL,'They\'re dead. Won\'t be causing trouble for your corporation.','I don\'t got it in me, I\'m canceling this job.',10,1,'killAll',NULL,1,NULL,1,'agent',NULL,500021,NULL,1,'agent',NULL,'nearby',NULL,NULL,1,NULL,1,NULL,NULL,50000,500,10000,NULL,1,NULL,1,3600,7,0.5,0.05,0.01,-0.05,-0.02,1,'500003,500007,500011,500001,500006,500004,500010,500008,500002,500018,500014,500019,500020,500012,500013,500016,500017,500009,500015',NULL);
INSERT INTO `missiontemplates` (`templateID`, `missionName`, `missionType`, `agentLevel`, `storyline`, `briefingText`, `objectiveText`, `acceptText`, `completeText`, `failText`, `declineText`, `moreInfoText`, `acceptLabel`, `declineLabel`, `viewMissionLabel`, `moreInfoLabel`, `completeLabel`, `quitMissionLabel`, `dungeonVID`, `dungeonRoomIdx`, `completionType`, `targetTypeID`, `targetCount`, `killItemTypeID`, `killItemQty`, `killItemDropoffMode`, `killItemDropoffStationID`, `enemyFactionID`, `courierTypeID`, `courierQty`, `pickupMode`, `pickupStationID`, `dropoffMode`, `dropoffStationID`, `tradeTypeID`, `tradeQty`, `miningTypeID`, `miningQty`, `minSecurity`, `maxSecurity`, `baseRewardISK`, `baseRewardLP`, `baseBonusISK`, `baseRewardItemTypeID`, `baseRewardItemQty`, `bonusRewardItemTypeID`, `bonusRewardItemQty`, `bonusTimeSec`, `expiryDays`, `standingGain`, `corpStandingGain`, `factionStandingGain`, `standingLoss`, `factionStandingLoss`, `enabled`, `allowedFactions`, `nextMissionID`) VALUES (4,'Serpent\'s Venom','kill',1,0,'If you got the stomach for it {charName}, try this on for size...\r\n\r\n\r\nAgents of the Serpentis Corporation have set up shop over in the system of {systemName} and are peddling their filth to local workers and attracking other unsavory scumbags to the area. I need you to go there, and blast these drug pushers into metal scraps! Do this, and you got a cool {rewardISK} waiting for you. ','Head to {systemname} and eliminate the Serpentis drug pushers','Good! Talk to me when you see it done.','Fantastic work {charName}! You showed those Serpentis bastards what we think about them poisoning our people!',NULL,'You disappoint me {charName}',NULL,NULL,NULL,NULL,NULL,NULL,NULL,18,1,'killTarget',23998,1,NULL,1,'agent',NULL,500020,NULL,1,'agent',NULL,'nearby',NULL,NULL,1,NULL,1,NULL,NULL,15000,500,5000,NULL,1,NULL,1,3600,7,0.2,0.02,0,-0.01,0,1,'500003,500007,500001,500006,500004,500008,500002,500014,500013,500016,500017',NULL);
INSERT INTO `missiontemplates` (`templateID`, `missionName`, `missionType`, `agentLevel`, `storyline`, `briefingText`, `objectiveText`, `acceptText`, `completeText`, `failText`, `declineText`, `moreInfoText`, `acceptLabel`, `declineLabel`, `viewMissionLabel`, `moreInfoLabel`, `completeLabel`, `quitMissionLabel`, `dungeonVID`, `dungeonRoomIdx`, `completionType`, `targetTypeID`, `targetCount`, `killItemTypeID`, `killItemQty`, `killItemDropoffMode`, `killItemDropoffStationID`, `enemyFactionID`, `courierTypeID`, `courierQty`, `pickupMode`, `pickupStationID`, `dropoffMode`, `dropoffStationID`, `tradeTypeID`, `tradeQty`, `miningTypeID`, `miningQty`, `minSecurity`, `maxSecurity`, `baseRewardISK`, `baseRewardLP`, `baseBonusISK`, `baseRewardItemTypeID`, `baseRewardItemQty`, `bonusRewardItemTypeID`, `bonusRewardItemQty`, `bonusTimeSec`, `expiryDays`, `standingGain`, `corpStandingGain`, `factionStandingGain`, `standingLoss`, `factionStandingLoss`, `enabled`, `allowedFactions`, `nextMissionID`) VALUES (5,'Madhare Showdown','kill',1,0,'Yeah something just came up.\r\n\r\n{charName}, We got an issue. some crazy Gurista is on the loose in {systemName} picking a fight with just about anything that he rolls up on. They\'ve proven to be quite the ace, taking on bounty hunters and even navy pilots who\'ve gone after them. Turns out he\'s one of those \"Dread\" Gurista we keep hearing about. Nasty stuff.\r\n\r\nSo it\'s time we called in Capsuleer... Go to {systemName}, {charName} and punch in these coordinate where they were last located. If they\'re there? Blast em. They\'ve amassed quite the bounty already, so consider that and anything on their wreck as payment. And uh... A favor from me. ','Hunt down the Gurista ace causing','Don\'t underestimate him. HE maybe one pilot, but he\'s still one of the Guristas\' Elite.','Nice flying! You proved you can handle yourself in a one-on-one with an Elite raider!',NULL,'No way these pirate bastards are that scary!',NULL,NULL,NULL,NULL,NULL,NULL,NULL,19,1,'killAll',NULL,1,NULL,1,'agent',NULL,500010,NULL,1,'agent',NULL,'nearby',NULL,NULL,1,NULL,1,NULL,NULL,0,1500,0,NULL,1,NULL,1,0,7,0.5,0.05,0,-0.05,0,1,'500003,500007,500011,500001,500006,500004,500005,500008,500002,500018,500014,500020,500013,500016,500009,500015',NULL);
INSERT INTO `missiontemplates` (`templateID`, `missionName`, `missionType`, `agentLevel`, `storyline`, `briefingText`, `objectiveText`, `acceptText`, `completeText`, `failText`, `declineText`, `moreInfoText`, `acceptLabel`, `declineLabel`, `viewMissionLabel`, `moreInfoLabel`, `completeLabel`, `quitMissionLabel`, `dungeonVID`, `dungeonRoomIdx`, `completionType`, `targetTypeID`, `targetCount`, `killItemTypeID`, `killItemQty`, `killItemDropoffMode`, `killItemDropoffStationID`, `enemyFactionID`, `courierTypeID`, `courierQty`, `pickupMode`, `pickupStationID`, `dropoffMode`, `dropoffStationID`, `tradeTypeID`, `tradeQty`, `miningTypeID`, `miningQty`, `minSecurity`, `maxSecurity`, `baseRewardISK`, `baseRewardLP`, `baseBonusISK`, `baseRewardItemTypeID`, `baseRewardItemQty`, `bonusRewardItemTypeID`, `bonusRewardItemQty`, `bonusTimeSec`, `expiryDays`, `standingGain`, `corpStandingGain`, `factionStandingGain`, `standingLoss`, `factionStandingLoss`, `enabled`, `allowedFactions`, `nextMissionID`) VALUES (6,'Bloodsoaked Heretics','kill',1,0,'{charName}, Are you familiar with Blood Raiders by chance? Gruesome bunch of fanatics. Ruthlessly cornering ships in deadspace pockets and bleeding the crew alive... Why do I bring them up? Recently we\'ve had some \'pilgrims\' in the area who claim to be common Sani Sabik followers, but in reality are a front for Blood Raider recruiters preying on some of our less fortunate citizens in our territory. I\'ve tracked down one of their sanctuaries in {systemName} and got a bookmark for you to warp directly to it. I need you to head in and gun down any of the cultists in the area. Don\'t worry about any of the actual structures, We\'ll have local authorities clear those out. Just take out those ships! ','Head to the bookmark and eliminate all Blood Raiders','Great! Hit me up when you\'re done {charName}!','Fantastic work! I\'ll let the cops know. Wiring payment.',NULL,'Look I know they\'re scary and all, but you\'re a Capsuleer!',NULL,'I\'ll make them bleed.','Blood Raiders? I want nothing to do with those guys...',NULL,'I\'m gonna need to know some more...','Site is clear, all Blood Raiders eliminated.','This is too much! Gotta bail!',20,1,'killAll',NULL,6,NULL,1,'agent',NULL,500012,NULL,1,'agent',NULL,'nearby',NULL,NULL,1,NULL,1,NULL,NULL,25000,100,10000,NULL,1,NULL,1,1600,7,0.05,0.05,0,0,0,1,'500003,500007,500011,500001,500006,500004,500008,500002,500018,500014,500020,500013,500016,500009,500015',NULL);
/*!40000 ALTER TABLE `missiontemplates` ENABLE KEYS */;
UNLOCK TABLES;
/*!40103 SET TIME_ZONE=@OLD_TIME_ZONE */;

/*!40101 SET SQL_MODE=@OLD_SQL_MODE */;
/*!40014 SET FOREIGN_KEY_CHECKS=@OLD_FOREIGN_KEY_CHECKS */;
/*!40014 SET UNIQUE_CHECKS=@OLD_UNIQUE_CHECKS */;
/*!40101 SET CHARACTER_SET_CLIENT=@OLD_CHARACTER_SET_CLIENT */;
/*!40101 SET CHARACTER_SET_RESULTS=@OLD_CHARACTER_SET_RESULTS */;
/*!40101 SET COLLATION_CONNECTION=@OLD_COLLATION_CONNECTION */;
/*!40111 SET SQL_NOTES=@OLD_SQL_NOTES */;

