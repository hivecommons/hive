-- Minimal Wispr Flow History schema (community-documented, Sequelize DATETIME strings).
CREATE TABLE History (
  transcriptEntityId VARCHAR(36) PRIMARY KEY,
  asrText TEXT, formattedText TEXT, editedText TEXT,
  timestamp DATETIME, audio BLOB, screenshot BLOB,
  status VARCHAR(255), app VARCHAR(255), url VARCHAR(255),
  duration FLOAT, numWords INTEGER, language TEXT, isArchived TINYINT(1) DEFAULT 0
);
INSERT INTO History VALUES ('d-1','um so add a retry with backoff around the upload call','Add a retry with backoff around the upload call.',NULL,'2026-11-10 09:15:00.000 +00:00',x'00',NULL,'formatted','com.microsoft.VSCode','',6.5,9,'en',0);
INSERT INTO History VALUES ('d-2','running late start without me','Running late, start without me.','Running late — start without me.','2026-11-11 17:55:30.000 +00:00',NULL,NULL,'formatted','com.tinyspeck.slackmacgap','',2.1,5,'en',0);
INSERT INTO History VALUES ('d-short','ok','Ok.',NULL,'2026-11-11 18:00:00.000 +00:00',NULL,NULL,'formatted','com.apple.MobileSMS','',0.8,1,'en',0);
INSERT INTO History VALUES ('d-raw','still processing','',NULL,'2026-11-11 18:05:00.000 +00:00',NULL,NULL,'raw_transcript','com.google.Chrome','',3,2,'en',0);
INSERT INTO History VALUES ('d-archived','archived thought about the hive dashboard','Archived thought about the hive dashboard.',NULL,'2026-11-11 18:10:00.000 +00:00',NULL,NULL,'formatted','md.obsidian','',4,6,'en',1);
INSERT INTO History VALUES ('d-old','ancient dictation from long ago','Ancient dictation from long ago.',NULL,'2020-01-01 10:00:00.000 +00:00',NULL,NULL,'formatted','com.google.Chrome','https://example.com',4,5,'en',0);
