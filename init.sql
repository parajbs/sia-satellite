DROP TABLE IF EXISTS accounts;

CREATE TABLE accounts (
	id             INT NOT NULL AUTO_INCREMENT,
	email          VARCHAR(255) NOT NULL UNIQUE,
	password_hash  VARCHAR(64) NOT NULL,
	verified       BOOL NOT NULL,
	PRIMARY KEY (id)
);