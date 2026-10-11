//go:build sqlite_fts5

package archive

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

// The fixture's explicit authorized set and expected times are independent of
// either horizon query. The old aggregate then provides a same-snapshot oracle
// for the indexed implementation, including NULL and negative timestamp cases.
func readerHorizonAssert(t *testing.T, frame *readFrame, ids []MessageID, wantHorizon, wantExpiry time.Time) {
	t.Helper()
	rows, err := frame.tx.QueryContext(frame.ctx, `SELECT m.id FROM `+authorizedTables+` WHERE `+frame.where, frame.args...)
	if err != nil {
		t.Fatal(err)
	}
	actual := make([]MessageID, 0)
	for rows.Next() {
		var id MessageID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		actual = append(actual, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]MessageID{}, ids...)
	sort.Slice(actual, func(i, j int) bool { return actual[i] < actual[j] })
	sort.Slice(expected, func(i, j int) bool { return expected[i] < expected[j] })
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("horizon fixture authorization: got %v, want %v", actual, expected)
	}
	horizon, expiry, err := frame.cursorHorizon()
	if err != nil || !horizon.Equal(wantHorizon) || !expiry.Equal(wantExpiry) {
		t.Fatalf("authorized horizon: got %v/%v error=%v, want %v/%v", horizon, expiry, err, wantHorizon, wantExpiry)
	}
	var minimumExpiry, minimumSent sql.NullInt64
	if err = frame.tx.QueryRowContext(frame.ctx, `SELECT min(nullif(m.expires_at,0)),min(m.original_sent_at) FROM `+authorizedTables+` WHERE `+frame.where, frame.args...).Scan(&minimumExpiry, &minimumSent); err != nil {
		t.Fatal(err)
	}
	var oracleHorizon, oracleExpiry time.Time
	if minimumExpiry.Valid {
		oracleExpiry = fromMillis(minimumExpiry.Int64)
	}
	if frame.rolling > 0 && minimumSent.Valid {
		oracleHorizon = fromMillis(minimumSent.Int64).Add(frame.rolling)
	}
	if !horizon.Equal(oracleHorizon) || !expiry.Equal(oracleExpiry) {
		t.Fatalf("indexed horizon changed old MIN semantics in same snapshot: got %v/%v, oracle %v/%v", horizon, expiry, oracleHorizon, oracleExpiry)
	}
}

func TestReaderHorizonDeniedMinimaNeverCrowdAuthorizedSources(t *testing.T) {
	for _, denial := range []string{"scope_chat", "request_chat", "policy_chat", "policy_date", "fixed_lower", "fixed_upper", "rolling", "deleted", "expired", "unknown_date"} {
		t.Run(denial, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			oldest := readerEvent("oldest-permitted", "allowed", "Original permitted minimum", now.Add(-30*time.Minute))
			oldest.ExpiresAt = now.Add(10 * time.Minute)
			immortal := readerEvent("immortal-permitted", "allowed", "Original non-expiring source", now.Add(-10*time.Minute))
			distractor := readerEvent("denied-distractor", "allowed", "DENIED_HORIZON_CANARY", now.Add(-45*time.Minute))
			distractor.ExpiresAt = now.Add(time.Minute)
			if denial == "scope_chat" || denial == "request_chat" || denial == "policy_chat" {
				distractor.Key.Chat = "other"
			}
			if denial == "fixed_upper" {
				distractor.OriginalSentAt = now.Add(-5 * time.Minute)
			} else if denial == "rolling" {
				distractor.OriginalSentAt = now.Add(-2 * time.Hour)
			}
			oldestID, immortalID, distractorID := readerApply(t, s, p, oldest), readerApply(t, s, p, immortal), readerApply(t, s, p, distractor)
			allowedChat, otherChat := readerChat(t, s, oldestID), readerChat(t, s, distractorID)
			if denial == "policy_chat" || denial == "policy_date" {
				policy := ArchivePolicy{DateMode: DateAllAvailable, ChatMode: ChatsSelective, IncludedChats: []ChatID{allowedChat}}
				if denial == "policy_date" {
					policy = ArchivePolicy{DateMode: DateSince, ImportStart: now.Add(-40 * time.Minute), ChatMode: ChatsAll}
				}
				var err error
				p, err = s.SetPolicy(context.Background(), policy, p.Generation)
				if err != nil {
					t.Fatal(err)
				}
				// Policy normally clears denied content. Retaining its metadata
				// under an active state proves that every horizon query still
				// applies the policy instead of relying on writer cleanup.
				if _, err = s.writeDB.Exec(`UPDATE messages SET state='active' WHERE id=?`, distractorID); err != nil {
					t.Fatal(err)
				}
			}
			if denial == "deleted" {
				distractor.Kind, distractor.SourceRevision, distractor.Body = EventDelete, 2, ""
				readerApply(t, s, p, distractor)
			} else if denial == "unknown_date" {
				if _, err := s.writeDB.Exec(`UPDATE messages SET original_date_known=0 WHERE id=?`, distractorID); err != nil {
					t.Fatal(err)
				}
			} else if denial == "expired" {
				now = now.Add(2 * time.Minute)
			}
			for _, rolling := range []time.Duration{0, time.Hour} {
				t.Run(fmt.Sprintf("rolling_%s", rolling), func(t *testing.T) {
					scope := readerScope(p, allowedChat, otherChat)
					scope.Rolling = rolling
					var requestChats []ChatID
					if denial == "scope_chat" {
						scope.Chats = []ChatID{allowedChat}
					} else if denial == "request_chat" {
						requestChats = []ChatID{allowedChat}
					} else if denial == "fixed_lower" {
						scope.From = oldest.OriginalSentAt.Add(-10 * time.Minute)
					} else if denial == "fixed_upper" {
						scope.Until = immortal.OriginalSentAt.Add(2 * time.Minute)
					}
					frame, err := s.beginRead(context.Background(), scope, requestChats, time.Time{}, time.Time{})
					if err != nil {
						t.Fatal(err)
					}
					defer frame.close()
					ids := []MessageID{oldestID, immortalID}
					wantExpiry := oldest.ExpiresAt
					var wantHorizon time.Time
					if rolling > 0 {
						wantHorizon = oldest.OriginalSentAt.Add(rolling)
					} else if denial == "rolling" {
						// With rolling disabled the older source is legitimately
						// authorized, so it supplies the earliest live expiry.
						ids = append(ids, distractorID)
						wantExpiry = distractor.ExpiresAt
					}
					readerHorizonAssert(t, frame, ids, wantHorizon, wantExpiry)
				})
			}
		})
	}
}

func TestReaderHorizonEmptyAndNonExpiringAuthorizedSets(t *testing.T) {
	for _, scenario := range []string{"no_expiring", "only_denied_expiry", "no_authorized"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			s, p := readerFixture(t, &now)
			oldest := readerEvent("oldest", "allowed", "Original non-expiring oldest", now.Add(-30*time.Minute))
			oldestID := readerApply(t, s, p, oldest)
			otherID := readerApply(t, s, p, readerEvent("other", "allowed", "Original non-expiring other", now.Add(-10*time.Minute)))
			if scenario == "only_denied_expiry" {
				expired := readerEvent("expired", "allowed", "Expired source metadata", now.Add(-45*time.Minute))
				expired.ExpiresAt = now.Add(time.Minute)
				readerApply(t, s, p, expired)
				now = now.Add(2 * time.Minute)
			}
			for _, rolling := range []time.Duration{0, time.Hour} {
				t.Run(fmt.Sprintf("rolling_%s", rolling), func(t *testing.T) {
					scope := readerScope(p, readerChat(t, s, oldestID))
					scope.Rolling = rolling
					ids := []MessageID{oldestID, otherID}
					var wantHorizon time.Time
					if scenario == "no_authorized" {
						scope.From, ids = now.Add(time.Minute), nil
					} else if rolling > 0 {
						wantHorizon = oldest.OriginalSentAt.Add(rolling)
					}
					frame, err := s.beginRead(context.Background(), scope, nil, time.Time{}, time.Time{})
					if err != nil {
						t.Fatal(err)
					}
					defer frame.close()
					readerHorizonAssert(t, frame, ids, wantHorizon, time.Time{})
				})
			}
		})
	}
}

func TestReaderHorizonFractionalScopeAndRequestIntersectionsMatchOracle(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	base := now.Add(-30 * time.Minute)
	ids := make([]MessageID, 4)
	for n := 0; n < 4; n++ {
		event := readerEvent(fmt.Sprintf("millisecond-%d", n), "allowed", "Original fractional-bound source", base.Add(time.Duration(n)*time.Millisecond))
		event.ExpiresAt = now.Add(time.Duration([]int{1, 4, 3, 2}[n]) * time.Minute)
		ids[n] = readerApply(t, s, p, event)
	}
	for _, scenario := range []string{"scope", "request", "request_narrows_scope", "scope_narrows_request"} {
		for _, rolling := range []time.Duration{0, time.Hour} {
			t.Run(fmt.Sprintf("%s/rolling_%s", scenario, rolling), func(t *testing.T) {
				scope := readerScope(p, readerChat(t, s, ids[0]))
				scope.Rolling = rolling
				lower, upper := base.Add(500*time.Microsecond), base.Add(2500*time.Microsecond)
				wideLower, wideUpper := base.Add(250*time.Microsecond), base.Add(2750*time.Microsecond)
				var requestFrom, requestUntil time.Time
				switch scenario {
				case "scope":
					scope.From, scope.Until = lower, upper
				case "request":
					requestFrom, requestUntil = lower, upper
				case "request_narrows_scope":
					scope.From, scope.Until, requestFrom, requestUntil = wideLower, wideUpper, lower, upper
				case "scope_narrows_request":
					scope.From, scope.Until, requestFrom, requestUntil = lower, upper, wideLower, wideUpper
				}
				frame, err := s.beginRead(context.Background(), scope, nil, requestFrom, requestUntil)
				if err != nil {
					t.Fatal(err)
				}
				defer frame.close()
				if !frame.fixedFrom.Equal(lower) || !frame.until.Equal(upper) {
					t.Fatalf("horizon frame widened fixed intersection: from=%v until=%v", frame.fixedFrom, frame.until)
				}
				var wantHorizon time.Time
				if rolling > 0 {
					wantHorizon = base.Add(time.Millisecond).Add(rolling)
				}
				readerHorizonAssert(t, frame, ids[1:3], wantHorizon, now.Add(3*time.Minute))
			})
		}
	}
}

func TestReaderHorizonPreEpochFutureExpiryRemainsNonzero(t *testing.T) {
	now := time.Date(1900, 1, 1, 0, 30, 0, 0, time.UTC)
	s, p := readerFixture(t, &now)
	oldest := readerEvent("pre-epoch-expiring", "allowed", "Original known 1899 source", time.Date(1899, 12, 31, 23, 30, 0, 0, time.UTC))
	oldest.ExpiresAt = time.Date(1901, 1, 1, 0, 0, 0, 0, time.UTC)
	oldestID := readerApply(t, s, p, oldest)
	immortalID := readerApply(t, s, p, readerEvent("pre-epoch-immortal", "allowed", "Original known 1900 source", time.Date(1900, 1, 1, 0, 15, 0, 0, time.UTC)))
	metadata, err := s.SourceMetadata(context.Background(), oldest.Key)
	if err != nil || !metadata.OriginalDateKnown || !metadata.Version.ExpiresAt.Equal(oldest.ExpiresAt) || oldest.ExpiresAt.UnixMilli() >= 0 || !oldest.ExpiresAt.After(now) {
		t.Fatalf("pre-epoch future expiry fixture not stored truthfully: %#v %v", metadata, err)
	}
	for _, rolling := range []time.Duration{0, 2 * time.Hour} {
		t.Run(fmt.Sprintf("rolling_%s", rolling), func(t *testing.T) {
			scope := readerScope(p, readerChat(t, s, oldestID))
			scope.Rolling = rolling
			frame, err := s.beginRead(context.Background(), scope, nil, time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			defer frame.close()
			var wantHorizon time.Time
			if rolling > 0 {
				wantHorizon = oldest.OriginalSentAt.Add(rolling)
			}
			readerHorizonAssert(t, frame, []MessageID{oldestID, immortalID}, wantHorizon, oldest.ExpiresAt)
		})
	}
}
