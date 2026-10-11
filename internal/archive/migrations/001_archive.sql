CREATE TABLE archive_meta(id INTEGER PRIMARY KEY CHECK(id=1),archive_revision INTEGER NOT NULL,index_generation INTEGER NOT NULL,normalization_version TEXT NOT NULL,cursor_key BLOB NOT NULL CHECK(length(cursor_key)=32));
CREATE TABLE accounts(account TEXT PRIMARY KEY);
CREATE TABLE account_safety(account TEXT PRIMARY KEY REFERENCES accounts(account),unsafe INTEGER NOT NULL DEFAULT 0,reason TEXT NOT NULL DEFAULT '',epoch INTEGER NOT NULL DEFAULT 0);
CREATE TABLE identities(id TEXT PRIMARY KEY,account TEXT NOT NULL REFERENCES accounts(account),source_identity TEXT NOT NULL,name TEXT NOT NULL DEFAULT '',UNIQUE(account,source_identity));
CREATE TABLE identity_aliases(account TEXT NOT NULL REFERENCES accounts(account),alias TEXT NOT NULL,canonical TEXT NOT NULL,provenance TEXT NOT NULL,PRIMARY KEY(account,alias));
CREATE TABLE chats(id TEXT PRIMARY KEY,account TEXT NOT NULL REFERENCES accounts(account),source_chat TEXT NOT NULL,name TEXT NOT NULL DEFAULT '',observed_oldest INTEGER NOT NULL DEFAULT 0,observed_latest INTEGER NOT NULL DEFAULT 0,UNIQUE(account,source_chat));
CREATE TABLE chat_aliases(account TEXT NOT NULL REFERENCES accounts(account),alias TEXT NOT NULL,canonical TEXT NOT NULL,provenance TEXT NOT NULL,PRIMARY KEY(account,alias));
CREATE TABLE archive_policy(id INTEGER PRIMARY KEY CHECK(id=1),generation INTEGER NOT NULL,date_mode TEXT NOT NULL,import_start INTEGER NOT NULL,chat_mode TEXT NOT NULL,rolling_retention INTEGER NOT NULL DEFAULT 0);
CREATE TABLE policy_chats(chat_id TEXT PRIMARY KEY REFERENCES chats(id));
CREATE TABLE messages(id TEXT PRIMARY KEY,account TEXT NOT NULL REFERENCES accounts(account),chat_id TEXT NOT NULL REFERENCES chats(id),sender_id TEXT NOT NULL REFERENCES identities(id),source_chat TEXT NOT NULL,source_sender TEXT NOT NULL,source_message TEXT NOT NULL,from_me INTEGER NOT NULL,
 original_sent_at INTEGER NOT NULL DEFAULT 0,original_date_known INTEGER NOT NULL DEFAULT 0,protocol_revision INTEGER NOT NULL DEFAULT 0,local_revision INTEGER NOT NULL DEFAULT 1,state TEXT NOT NULL,source_kind TEXT NOT NULL,
 body TEXT NOT NULL DEFAULT '',caption TEXT NOT NULL DEFAULT '',normalized_body TEXT NOT NULL DEFAULT '',normalized_caption TEXT NOT NULL DEFAULT '',body_kind TEXT NOT NULL DEFAULT 'unsupported',content_hash TEXT NOT NULL DEFAULT '',edited INTEGER NOT NULL DEFAULT 0,expires_at INTEGER NOT NULL DEFAULT 0,media_availability TEXT NOT NULL DEFAULT 'not_retained',view_once INTEGER NOT NULL DEFAULT 0,revision_barrier TEXT NOT NULL DEFAULT '',
 reply_account TEXT NOT NULL DEFAULT '',reply_chat TEXT NOT NULL DEFAULT '',reply_sender TEXT NOT NULL DEFAULT '',reply_message TEXT NOT NULL DEFAULT '',reply_from_me INTEGER NOT NULL DEFAULT 0,
 UNIQUE(account,source_chat,source_sender,source_message,from_me));
CREATE INDEX messages_chat_date ON messages(chat_id,original_sent_at,id);
CREATE INDEX messages_account_state_expiry ON messages(account,state,expires_at);
CREATE INDEX messages_scope_date ON messages(account,state,chat_id,original_sent_at,id);
CREATE INDEX messages_mutation_target ON messages(account,source_chat,source_message);
CREATE VIRTUAL TABLE message_fts USING fts5(normalized_body,normalized_caption,tokenize='unicode61 remove_diacritics 2');
CREATE TABLE source_tombstones(account TEXT NOT NULL REFERENCES accounts(account),chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,protocol_revision INTEGER NOT NULL,PRIMARY KEY(account,chat,sender,message,from_me));
CREATE TABLE chat_tombstones(account TEXT NOT NULL REFERENCES accounts(account),chat TEXT NOT NULL,cutoff INTEGER NOT NULL,PRIMARY KEY(account,chat));
CREATE TABLE source_anchors(account TEXT NOT NULL REFERENCES accounts(account),chat TEXT NOT NULL,sender TEXT NOT NULL,message TEXT NOT NULL,from_me INTEGER NOT NULL,original_sent_at INTEGER NOT NULL DEFAULT 0,date_known INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(account,chat,sender,message,from_me));
CREATE INDEX source_anchors_date ON source_anchors(account,chat,original_sent_at);
CREATE TABLE sync_state(account TEXT NOT NULL REFERENCES accounts(account),chat TEXT NOT NULL,connection TEXT NOT NULL DEFAULT 'disconnected',checkpoint_kind TEXT NOT NULL DEFAULT '',checkpoint_id TEXT NOT NULL DEFAULT '',observed_oldest INTEGER NOT NULL DEFAULT 0,observed_latest INTEGER NOT NULL DEFAULT 0,received INTEGER NOT NULL DEFAULT 0,applied INTEGER NOT NULL DEFAULT 0,dropped INTEGER NOT NULL DEFAULT 0,coverage_unknown INTEGER NOT NULL DEFAULT 1,PRIMARY KEY(account,chat));
CREATE TABLE sync_gaps(account TEXT NOT NULL REFERENCES accounts(account),chat TEXT NOT NULL,code TEXT NOT NULL,PRIMARY KEY(account,chat,code));
CREATE TABLE dirty_windows(chat_id TEXT NOT NULL REFERENCES chats(id),generation INTEGER NOT NULL,from_time INTEGER NOT NULL,until_time INTEGER NOT NULL,archive_revision INTEGER NOT NULL,PRIMARY KEY(chat_id,generation));
