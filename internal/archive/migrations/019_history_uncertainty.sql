CREATE TABLE history_uncertainty_coverage (
 account TEXT PRIMARY KEY REFERENCES accounts(account),
 epoch INTEGER NOT NULL CHECK(typeof(epoch)='integer' AND epoch>0),
 total_observations INTEGER NOT NULL CHECK(typeof(total_observations)='integer' AND total_observations>0)
);
CREATE TABLE history_uncertainty_reasons (
 account TEXT NOT NULL REFERENCES history_uncertainty_coverage(account),
 reason TEXT NOT NULL CHECK(reason IN ('unsupported_metadata','unsupported_notice','unsupported_control')),
 count INTEGER NOT NULL CHECK(typeof(count)='integer' AND count>0),
 first_seen INTEGER NOT NULL CHECK(typeof(first_seen)='integer'),
 last_seen INTEGER NOT NULL CHECK(typeof(last_seen)='integer' AND last_seen>=first_seen),
 PRIMARY KEY(account,reason)
);
