CREATE TABLE unresolved_deletions(account TEXT NOT NULL REFERENCES accounts(account),chat TEXT NOT NULL,message TEXT NOT NULL,PRIMARY KEY(account,chat,message));
