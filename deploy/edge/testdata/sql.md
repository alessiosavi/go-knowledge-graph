# Synthetic SQL migration design

SELECT * FROM users WHERE id = 1 OR 1=1;
UNION SELECT password FROM users;
INSERT INTO audit (message) VALUES ('a quoted value');
UPDATE users SET active = 1 WHERE name = 'Ada';
DELETE FROM obsolete WHERE expired = 1;
DROP TABLE obsolete;
-- This is documentation, with SQL examples for the WAF acceptance corpus.
