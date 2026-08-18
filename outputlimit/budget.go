package outputlimit

import (
	"errors"
	"sync"
)

const MinimumBudgetGrant = 2000
const DefaultPerSession = 400000

var (
	ErrInvalidLimit    = errors.New("outputlimit: invalid limit")
	ErrCounterOverflow = errors.New("outputlimit: counter overflow")
)

// Budget coordinates cumulative rendered output independently per session.
type Budget struct {
	mu sync.Mutex

	perSession int
	used       map[string]int
	reserved   map[string]int
	pending    map[string]map[uint64]int
	nextID     uint64
}

// Reservation identifies one outstanding budget grant. IDs are unique within
// a Budget so concurrent calls can settle in completion order.
type Reservation struct {
	ID    uint64
	Grant int
}

// NewBudget constructs a per-session budget. Non-positive values use the
// package default.
func NewBudget(perSession int) *Budget {
	if perSession <= 0 {
		perSession = DefaultPerSession
	}
	return &Budget{
		perSession: perSession,
		used:       make(map[string]int),
		reserved:   make(map[string]int),
		pending:    make(map[string]map[uint64]int),
	}
}

// Reserve returns and records the cap available to one tool call.
func (b *Budget) Reserve(sessionID string, want int) Reservation {
	if want <= 0 {
		return Reservation{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.perSession - b.used[sessionID] - b.reserved[sessionID]
	if remaining <= 0 {
		return Reservation{}
	}
	allowed := remaining / 2
	if allowed < MinimumBudgetGrant {
		allowed = MinimumBudgetGrant
	}
	if allowed > remaining {
		allowed = remaining
	}
	if allowed > want {
		allowed = want
	}
	b.nextID++
	if b.nextID == 0 {
		b.nextID++
	}
	reservation := Reservation{ID: b.nextID, Grant: allowed}
	b.reserved[sessionID] += allowed
	if b.pending[sessionID] == nil {
		b.pending[sessionID] = make(map[uint64]int)
	}
	b.pending[sessionID][reservation.ID] = allowed
	return reservation
}

// Consume settles one reservation with the bytes actually emitted.
func (b *Budget) Consume(sessionID string, reservationID uint64, emitted int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	grant := 0
	if reservations := b.pending[sessionID]; reservations != nil {
		grant = reservations[reservationID]
		if grant > 0 {
			delete(reservations, reservationID)
		}
		if len(reservations) == 0 {
			delete(b.pending, sessionID)
		}
		b.reserved[sessionID] -= grant
	}
	if emitted < 0 {
		emitted = 0
	}
	if grant > 0 && emitted > grant {
		emitted = grant
	}
	remaining := b.perSession - b.used[sessionID] - b.reserved[sessionID]
	if emitted > remaining {
		emitted = remaining
	}
	if emitted > 0 {
		b.used[sessionID] += emitted
	}
}

// Reset restores a session's full budget and drops outstanding reservations.
func (b *Budget) Reset(sessionID string) {
	b.mu.Lock()
	delete(b.used, sessionID)
	delete(b.reserved, sessionID)
	delete(b.pending, sessionID)
	b.mu.Unlock()
}

// Report returns consumed bytes and the configured per-session limit.
func (b *Budget) Report(sessionID string) (used, limit int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used[sessionID], b.perSession
}
