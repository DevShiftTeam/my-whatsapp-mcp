package ingestion

import (
	"fmt"
	"maps"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// This seam exercises only the real reservation, FIFO and completion helpers.
// No store, SDK connection, worker, timer or callback goroutine is started.
type reservationModel struct {
	runtime      *Runtime
	countCap     int
	byteCap      int64
	label        string
	items        map[uint64]reservationModelItem
	count        int
	bytes        int64
	lastEnqueued uint64
}

type reservationModelItem struct {
	ticket   *sourceTicket
	original sourceTicket
}

type reservationModelFlags struct {
	admitted, waiting, queued bool
}

func newReservationModel(count int, bytes int64, label string) *reservationModel {
	return &reservationModel{
		runtime:  &Runtime{config: Config{BodyQueueCount: count, BodyQueueBytes: bytes}, sourceTickets: make(map[uint64]*sourceTicket)},
		countCap: count, byteCap: bytes, label: label, items: make(map[uint64]reservationModelItem),
	}
}

func (m *reservationModel) add(kind string, bytes int64, admitted, queued bool) *sourceTicket {
	r := m.runtime
	r.sourceSequence++
	ticket := &sourceTicket{
		sequence: r.sourceSequence, bytes: bytes, live: kind == "live", history: kind == "history",
		prepared:   kind == "history" && r.sourceSequence%2 == 0,
		receivedAt: time.Unix(1700000000, int64(r.sourceSequence)),
		deadline:   time.Date(9999, time.January, 1, 0, 0, 0, int(r.sourceSequence), time.UTC),
		messageID:  fmt.Sprintf("synthetic-message-%d", r.sourceSequence),
		keys:       map[string]struct{}{fmt.Sprintf("synthetic-key-%d", r.sourceSequence): {}, "synthetic-overlap": {}},
	}
	r.sourceTickets[ticket.sequence] = ticket
	if admitted {
		r.reserveAdmissionLocked(ticket)
	} else {
		ticket.waiting = true
		r.waitingAdmissions++
		r.waitingBytes += bytes
	}
	ticket.queued = queued
	if queued {
		m.lastEnqueued = ticket.sequence
	}
	original := *ticket
	original.keys = maps.Clone(ticket.keys)
	m.items[ticket.sequence] = reservationModelItem{ticket: ticket, original: original}
	m.count++
	m.bytes += bytes
	return ticket
}

func (m *reservationModel) check(t *testing.T) {
	t.Helper()
	r := m.runtime
	if r.config.BodyQueueCount != m.countCap || r.config.BodyQueueBytes != m.byteCap {
		t.Fatalf("%s: original admission caps changed", m.label)
	}
	if len(r.sourceTickets) != m.count || len(m.items) != m.count {
		t.Fatalf("%s: retained ticket inventory changed", m.label)
	}
	admitted, waiting := 0, 0
	admittedBytes, waitingBytes := int64(0), int64(0)
	for sequence, saved := range m.items {
		ticket := r.sourceTickets[sequence]
		if ticket != saved.ticket || ticket.sequence != sequence {
			t.Fatalf("%s: ticket %d changed identity or sequence", m.label, sequence)
		}
		current := *ticket
		current.admitted, current.waiting, current.queued = saved.original.admitted, saved.original.waiting, saved.original.queued
		if !reflect.DeepEqual(current, saved.original) {
			t.Fatalf("%s: ticket %d changed payload metadata, keys, classification or deadline", m.label, sequence)
		}
		if ticket.admitted == ticket.waiting || ticket.queued && !ticket.admitted {
			t.Fatalf("%s: ticket %d is not owned by exactly one valid ledger", m.label, sequence)
		}
		if ticket.bytes < 1 || ticket.bytes > m.byteCap {
			t.Fatalf("%s: ticket %d has an invalid retained byte charge", m.label, sequence)
		}
		if ticket.admitted {
			admitted++
			admittedBytes += ticket.bytes
		} else {
			waiting++
			waitingBytes += ticket.bytes
		}
	}
	if admitted != r.admissions || admittedBytes != r.queueBytes || waiting != r.waitingAdmissions || waitingBytes != r.waitingBytes {
		t.Fatalf("%s: counters disagree with tickets: admitted=%d/%d bytes=%d/%d waiting=%d/%d bytes=%d/%d", m.label, r.admissions, admitted, r.queueBytes, admittedBytes, r.waitingAdmissions, waiting, r.waitingBytes, waitingBytes)
	}
	if admitted > m.countCap || waiting > m.countCap || admittedBytes > m.byteCap || waitingBytes > m.byteCap {
		t.Fatalf("%s: separate original ledger caps exceeded: admitted=%d/%d waiting=%d/%d", m.label, admitted, admittedBytes, waiting, waitingBytes)
	}
	if admitted+waiting != m.count || admittedBytes+waitingBytes != m.bytes {
		t.Fatalf("%s: reservation changed total retained count or bytes", m.label)
	}
	if oldest := m.oldest(false); oldest != nil {
		for _, item := range m.items {
			ticket := item.ticket
			if !ticket.queued && r.earlierUnqueuedLocked(ticket) != (ticket.sequence > oldest.sequence) {
				t.Fatalf("%s: ticket %d no longer respects the global unqueued FIFO", m.label, ticket.sequence)
			}
		}
	}
}

func (m *reservationModel) flags() map[uint64]reservationModelFlags {
	flags := make(map[uint64]reservationModelFlags, len(m.items))
	for sequence, item := range m.items {
		ticket := item.ticket
		flags[sequence] = reservationModelFlags{ticket.admitted, ticket.waiting, ticket.queued}
	}
	return flags
}

func (m *reservationModel) checkQueuedUnchanged(t *testing.T, before map[uint64]reservationModelFlags) {
	t.Helper()
	for sequence, flags := range before {
		item, exists := m.items[sequence]
		if !exists {
			continue // Only the explicitly completed ticket may have retired.
		}
		ticket := item.ticket
		if ticket.queued != flags.queued || flags.queued && (ticket.admitted != flags.admitted || ticket.waiting != flags.waiting) {
			t.Fatalf("%s: reservation moved or enqueued ticket %d owned by a worker", m.label, sequence)
		}
	}
}

func (m *reservationModel) reserve(t *testing.T) {
	t.Helper()
	m.check(t)
	before, sequence := m.flags(), m.runtime.sourceSequence
	m.runtime.mu.Lock()
	m.runtime.reserveWaitingLocked()
	m.runtime.mu.Unlock()
	m.check(t)
	m.checkQueuedUnchanged(t, before)
	if m.runtime.sourceSequence != sequence {
		t.Fatalf("%s: reservation allocated or reordered a ticket", m.label)
	}
}

func (m *reservationModel) oldest(queued bool) *sourceTicket {
	var oldest *sourceTicket
	for _, item := range m.items {
		ticket := item.ticket
		if ticket.queued == queued && (oldest == nil || ticket.sequence < oldest.sequence) {
			oldest = ticket
		}
	}
	return oldest
}

func (m *reservationModel) complete(t *testing.T, ticket *sourceTicket) {
	t.Helper()
	if ticket == nil || !ticket.queued || !m.runtime.sourceTurnAllowedLocked(ticket) {
		t.Fatalf("%s: model attempted completion before a ticket's source turn", m.label)
	}
	before := m.flags()
	m.runtime.completeSourceItem(queuedEvent{bytes: ticket.bytes, ticket: ticket})
	delete(m.items, ticket.sequence)
	m.count--
	m.bytes -= ticket.bytes
	m.check(t)
	m.checkQueuedUnchanged(t, before)
	if ticket.keys != nil {
		t.Fatalf("%s: completion did not release the retired ticket's keys", m.label)
	}
}

func (m *reservationModel) drain(t *testing.T) {
	t.Helper()
	remaining := m.count
	for step := 0; step < remaining; step++ {
		m.reserve(t)
		if oldest := m.oldest(false); oldest != nil && oldest.admitted {
			if m.runtime.earlierUnqueuedLocked(oldest) || oldest.sequence <= m.lastEnqueued {
				t.Fatalf("%s: an enqueue bypassed the global unqueued FIFO", m.label)
			}
			oldest.queued = true // Same publication boundary as admit, without a worker.
			m.lastEnqueued = oldest.sequence
			m.reserve(t)
		}
		queued := m.oldest(true)
		if queued == nil {
			t.Fatalf("%s: idle reservation trapped the oldest unqueued callback", m.label)
		}
		m.complete(t, queued)
	}
	if m.count != 0 || m.bytes != 0 {
		t.Fatalf("%s: bounded enqueue/completion loop did not drain", m.label)
	}
}

func TestRealtimeReservationModelIdleExchange(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   int
		cap     int64
		waiting []int64
		parked  []int64
		whole   bool
	}{
		{"multiple_waiter_byte_trap", 2, 1024, []int64{500, 300}, []int64{800}, true},
		{"multiple_parked_count_trap", 2, 1024, []int64{300, 200}, []int64{200, 100}, true},
		{"both_ledgers_at_original_caps", 4, 1024, []int64{256, 256, 256, 256}, []int64{256, 256, 256, 256}, true},
		{"single_exchange_keeps_later_waiter", 2, 1024, []int64{500, 300}, []int64{400}, false},
		{"count_one_and_one_byte", 1, 1, []int64{1}, []int64{1}, false},
		{"all_source_reservations", 3, 1024, []int64{400, 300, 200}, []int64{600, 300}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReservationModel(tc.count, tc.cap, tc.name)
			var waiters, parked []*sourceTicket
			for _, bytes := range tc.waiting {
				waiters = append(waiters, m.add("history", bytes, false, false))
			}
			for _, bytes := range tc.parked {
				kind := "live"
				if tc.name == "all_source_reservations" {
					kind = "source"
				}
				parked = append(parked, m.add(kind, bytes, true, false))
			}
			m.reserve(t)
			for i, waiter := range waiters {
				if waiter.admitted != (i == 0 || tc.whole) {
					t.Fatalf("waiter %d did not receive the intended bounded exchange", i)
				}
			}
			for _, ticket := range parked {
				if !ticket.waiting || ticket.admitted || !m.runtime.earlierUnqueuedLocked(ticket) {
					t.Fatal("parked reservation retained capacity or bypassed its original prefix")
				}
			}
			if tc.whole && tc.count > 1 && m.runtime.admissionRoomLocked(waiters[0]) {
				t.Fatal("fixture did not exercise an admitted history reservation beyond new-history admission quota")
			}
			m.drain(t)
		})
	}
}

func TestRealtimeReservationModelQueuedCapacitySafety(t *testing.T) {
	for _, tc := range []struct {
		name, queuedKind string
		count            int
		queuedBytes      int64
		waiting, parked  []int64
	}{
		{"count_blocks_whole_exchange", "history", 2, 220, []int64{500, 300}, []int64{800}},
		{"bytes_block_whole_exchange", "live", 3, 250, []int64{500, 300}, []int64{750}},
		{"queued_history_keeps_source_quota", "history", 2, 100, []int64{500}, nil},
		{"queued_history_keeps_byte_quota", "history", 4, 700, []int64{220}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newReservationModel(tc.count, 1024, tc.name)
			active := m.add(tc.queuedKind, tc.queuedBytes, true, true)
			for _, bytes := range tc.waiting {
				m.add("history", bytes, false, false)
			}
			for _, bytes := range tc.parked {
				m.add("live", bytes, true, false)
			}
			before := m.flags()
			m.reserve(t)
			if !reflect.DeepEqual(before, m.flags()) {
				t.Fatal("reservation stole active-worker capacity instead of waiting for completion")
			}
			oldest := m.oldest(false)
			m.complete(t, active)
			if !oldest.admitted || oldest.queued {
				t.Fatal("worker completion did not reserve the oldest callback without executing it")
			}
			m.drain(t)
		})
	}

	t.Run("single_exchange_preserves_queued_live", func(t *testing.T) {
		m := newReservationModel(2, 1024, "single_exchange_preserves_queued_live")
		m.add("live", 200, true, true)
		oldest := m.add("history", 500, false, false)
		parked := m.add("live", 100, true, false)
		m.reserve(t)
		if !oldest.admitted || !parked.waiting {
			t.Fatal("safe single exchange was blocked by unrelated queued live work")
		}
		m.drain(t)
	})
}

func TestRealtimeReservationModelGuardedNoops(t *testing.T) {
	for _, guard := range []string{"empty", "unsafe", "expired", "oldest_already_admitted"} {
		t.Run(guard, func(t *testing.T) {
			m := newReservationModel(2, 1024, guard)
			if guard != "empty" {
				oldest := m.add("history", 500, guard == "oldest_already_admitted", false)
				m.add("live", 300, guard != "oldest_already_admitted", false)
				if guard == "expired" {
					oldest.deadline = time.Unix(1, 0)
					saved := m.items[oldest.sequence]
					saved.original.deadline = oldest.deadline
					m.items[oldest.sequence] = saved
				}
			}
			m.runtime.status.ReconciliationRequired = guard == "unsafe"
			before := m.flags()
			m.reserve(t)
			if !reflect.DeepEqual(before, m.flags()) {
				t.Fatal("guarded reservation changed ticket ownership")
			}
		})
	}
}

func TestRealtimeReservationModelWaitingSelfReservation(t *testing.T) {
	m := newReservationModel(2, 1024, "waiting_self_reservation")
	oldest := m.add("history", 500, true, false)
	later := m.add("live", 200, false, false)
	m.check(t)
	m.runtime.mu.Lock()
	if !m.runtime.admissionRoomLocked(later) {
		m.runtime.mu.Unlock()
		t.Fatal("fixture has no room for a bounded parked reservation")
	}
	m.runtime.reserveAdmissionLocked(later)
	m.runtime.mu.Unlock()
	m.check(t)
	if !later.admitted || later.waiting || !m.runtime.earlierUnqueuedLocked(later) || !oldest.admitted {
		t.Fatal("self-reservation lost ownership or bypassed its prerequisite")
	}
	m.drain(t)
}

func reservationModelPartition(rng *rand.Rand, countCap int, byteCap int64) []int64 {
	count := 1 + rng.Intn(min(countCap, int(byteCap)))
	remaining := int64(count) + rng.Int63n(byteCap-int64(count)+1)
	parts := make([]int64, count)
	for i := range parts {
		parts[i] = remaining
		if i < count-1 {
			parts[i] = 1 + rng.Int63n(remaining-int64(count-i-1))
		}
		remaining -= parts[i]
	}
	return parts
}

func TestRealtimeReservationModelSeededIdleLedgers(t *testing.T) {
	for countCap := 1; countCap <= 4; countCap++ {
		for _, seed := range []int64{1, 0x5eed} {
			t.Run(fmt.Sprintf("count_%d_seed_%d", countCap, seed), func(t *testing.T) {
				rng := rand.New(rand.NewSource(seed))
				for byteCap := int64(1); byteCap <= 1024; byteCap++ {
					m := newReservationModel(countCap, byteCap, fmt.Sprintf("count=%d bytes=%d seed=%d", countCap, byteCap, seed))
					waiting := reservationModelPartition(rng, countCap, byteCap)
					parked := reservationModelPartition(rng, countCap, byteCap)
					type pending struct {
						bytes    int64
						admitted bool
					}
					oldest := m.add("history", waiting[0], false, false)
					var rest []pending
					for _, bytes := range waiting[1:] {
						rest = append(rest, pending{bytes: bytes})
					}
					for _, bytes := range parked {
						rest = append(rest, pending{bytes: bytes, admitted: true})
					}
					rng.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
					for _, item := range rest {
						kind := []string{"history", "source", "live"}[rng.Intn(3)]
						m.add(kind, item.bytes, item.admitted, false)
					}
					m.reserve(t)
					if !oldest.admitted || oldest.queued || m.runtime.earlierUnqueuedLocked(oldest) {
						t.Fatalf("%s: bounded idle ledgers did not reserve their oldest waiter", m.label)
					}
					m.drain(t)
				}
			})
		}
	}
}

func TestRealtimeReservationModelSeededReachableTransitions(t *testing.T) {
	for countCap := 1; countCap <= 4; countCap++ {
		for _, seed := range []int64{7, 0xc0ffee} {
			t.Run(fmt.Sprintf("count_%d_seed_%d", countCap, seed), func(t *testing.T) {
				rng := rand.New(rand.NewSource(seed))
				for byteCap := int64(1); byteCap <= 1024; byteCap++ {
					m := newReservationModel(countCap, byteCap, fmt.Sprintf("reachable count=%d bytes=%d seed=%d", countCap, byteCap, seed))
					// Hold one source worker, then register a bounded interleaved
					// callback burst through the real admission-room decision.
					m.add("source", 1+rng.Int63n(byteCap), true, true)
					for arrival := 0; arrival < 2*countCap; arrival++ {
						kind := []string{"history", "source", "live"}[rng.Intn(3)]
						bytes := 1 + rng.Int63n(byteCap)
						candidate := &sourceTicket{bytes: bytes, live: kind == "live", history: kind == "history"}
						room := m.runtime.admissionRoomLocked(candidate)
						if !room && (m.runtime.waitingAdmissions >= countCap || bytes > byteCap-m.runtime.waitingBytes) {
							continue // Do not deliver a callback beyond either retained budget.
						}
						ticket := m.add(kind, bytes, room, false)
						if room && !m.runtime.earlierUnqueuedLocked(ticket) {
							ticket.queued = true
							m.lastEnqueued = ticket.sequence
							m.reserve(t)
						}
						m.check(t)
					}
					m.drain(t)
				}
			})
		}
	}
}
