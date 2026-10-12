-- Empty metadata tables are harmless on ordinary archives. Only an explicit
-- rebuilt-profile startup selection can create delivered-history authority.
CREATE TABLE rebuild_history_selection (
 id INTEGER PRIMARY KEY CHECK(id=1), account TEXT NOT NULL REFERENCES accounts(account),
 generation TEXT NOT NULL, mode TEXT NOT NULL CHECK(mode IN('selected-original','delivered-history')),
 epoch INTEGER NOT NULL CHECK(typeof(epoch)='integer' AND epoch>0),
 accepted INTEGER NOT NULL CHECK(accepted=1),
 body_limit INTEGER NOT NULL CHECK(typeof(body_limit)='integer' AND body_limit BETWEEN 10000 AND 1000000),
 body_spent INTEGER NOT NULL CHECK(typeof(body_spent)='integer' AND body_spent BETWEEN 0 AND body_limit),
 last_page_chat TEXT NOT NULL DEFAULT ''
);
CREATE TABLE rebuild_history_selection_audit (
 epoch INTEGER PRIMARY KEY, command_id TEXT NOT NULL UNIQUE, account TEXT NOT NULL,
 generation TEXT NOT NULL, previous_mode TEXT NOT NULL, mode TEXT NOT NULL,
 accepted INTEGER NOT NULL CHECK(accepted=1), policy_generation INTEGER NOT NULL, selected_at INTEGER NOT NULL
);
CREATE TABLE rebuild_history_bodies (
 account TEXT NOT NULL, chat TEXT NOT NULL, sender TEXT NOT NULL, message TEXT NOT NULL,
 from_me INTEGER NOT NULL CHECK(from_me IN(0,1)), kind TEXT NOT NULL CHECK(kind IN('original','edit_content')),
 protocol_revision INTEGER NOT NULL, generation TEXT NOT NULL, strategy_epoch INTEGER NOT NULL,
 policy_generation INTEGER NOT NULL, admitted_at INTEGER NOT NULL,
 PRIMARY KEY(account,chat,sender,message,from_me,kind,protocol_revision)
);
CREATE INDEX rebuild_history_body_lookup ON rebuild_history_bodies(account,message,from_me,kind,protocol_revision,chat,sender);
CREATE TABLE rebuild_history_pages (
 id TEXT PRIMARY KEY, generation TEXT NOT NULL, strategy_epoch INTEGER NOT NULL, policy_generation INTEGER NOT NULL,
 account TEXT NOT NULL, chat TEXT NOT NULL, sender TEXT NOT NULL, message TEXT NOT NULL,
 from_me INTEGER NOT NULL CHECK(from_me IN(0,1)), original_sent_at INTEGER NOT NULL, created_at INTEGER NOT NULL,
 UNIQUE(generation,strategy_epoch,policy_generation,account,chat,sender,message,from_me)
);
CREATE INDEX rebuild_history_page_chat ON rebuild_history_pages(generation,strategy_epoch,policy_generation,account,chat,created_at);
CREATE TABLE rebuild_history_frontiers (
 generation TEXT NOT NULL, strategy_epoch INTEGER NOT NULL, policy_generation INTEGER NOT NULL,
 account TEXT NOT NULL, chat TEXT NOT NULL, sender TEXT NOT NULL, message TEXT NOT NULL,
 from_me INTEGER NOT NULL CHECK(from_me IN(0,1)), original_sent_at INTEGER NOT NULL,
 PRIMARY KEY(generation,strategy_epoch,policy_generation,account,chat)
);
