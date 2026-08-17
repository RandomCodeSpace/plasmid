package outputlimit

import (
	"sync"
	"testing"
)

func TestBudgetReserveConsumeShrinksGrant(t *testing.T) {
	budget := NewBudget(10000)
	reservations := []Reservation{
		budget.Reserve("session", 10000),
		{},
		{},
		{},
	}
	budget.Consume("session", reservations[0].ID, reservations[0].Grant)
	reservations[1] = budget.Reserve("session", 10000)
	budget.Consume("session", reservations[1].ID, reservations[1].Grant)
	reservations[2] = budget.Reserve("session", 10000)
	budget.Consume("session", reservations[2].ID, reservations[2].Grant)
	reservations[3] = budget.Reserve("session", 10000)
	grants := []int{reservations[0].Grant, reservations[1].Grant, reservations[2].Grant, reservations[3].Grant}
	if want := []int{5000, 2500, 2000, 500}; !equalInts(grants, want) {
		t.Fatalf("grants = %v; want %v", grants, want)
	}
	budget.Consume("session", reservations[3].ID, reservations[3].Grant)
	if used, limit := budget.Report("session"); used != 10000 || limit != 10000 {
		t.Fatalf("Report() = %d, %d", used, limit)
	}
}

func TestBudgetSmallRequestAndUnusedReservation(t *testing.T) {
	budget := NewBudget(10000)
	reservation := budget.Reserve("session", 17)
	if reservation.Grant != 17 {
		t.Fatalf("Reserve() = %#v", reservation)
	}
	budget.Consume("session", reservation.ID, 7)
	if used, _ := budget.Report("session"); used != 7 {
		t.Fatalf("used = %d", used)
	}
	if grant := budget.Reserve("session", 10000).Grant; grant != 4996 {
		t.Fatalf("Reserve() after partial consumption = %d", grant)
	}
}

func TestBudgetResetAndSessionIsolation(t *testing.T) {
	budget := NewBudget(8000)
	a := budget.Reserve("a", 8000)
	b := budget.Reserve("b", 8000)
	if a.Grant != 4000 || b.Grant != 4000 {
		t.Fatalf("grants = %#v, %#v", a, b)
	}
	budget.Consume("a", a.ID, 3000)
	budget.Reset("a")
	if used, limit := budget.Report("a"); used != 0 || limit != 8000 {
		t.Fatalf("a report = %d, %d", used, limit)
	}
	if used, limit := budget.Report("b"); used != 0 || limit != 8000 {
		t.Fatalf("b report = %d, %d", used, limit)
	}
}

func TestBudgetConcurrentReserveNeverOvergrants(t *testing.T) {
	const limit = 100000
	budget := NewBudget(limit)
	grants := make(chan int, 32)
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			grants <- budget.Reserve("session", 10000).Grant
		}()
	}
	group.Wait()
	close(grants)
	total := 0
	for grant := range grants {
		if grant < 0 {
			t.Fatalf("negative grant %d", grant)
		}
		total += grant
	}
	if total > limit {
		t.Fatalf("concurrent grants total %d; limit %d", total, limit)
	}
}

func TestBudgetDefaultsAndClampsConsumption(t *testing.T) {
	budget := NewBudget(0)
	if _, limit := budget.Report("session"); limit != DefaultPerSession {
		t.Fatalf("default limit = %d", limit)
	}
	reservation := budget.Reserve("session", 3000)
	budget.Consume("session", reservation.ID, reservation.Grant+1000)
	if used, _ := budget.Report("session"); used != reservation.Grant {
		t.Fatalf("used = %d; grant %d", used, reservation.Grant)
	}
}

func TestBudgetSettlesOutOfOrderByReservationID(t *testing.T) {
	budget := NewBudget(10000)
	first := budget.Reserve("session", 8000)
	second := budget.Reserve("session", 8000)
	if first.Grant != 5000 || second.Grant != 2500 || first.ID == second.ID {
		t.Fatalf("reservations = %#v, %#v", first, second)
	}

	budget.Consume("session", second.ID, second.Grant)
	budget.Consume("session", first.ID, 1000)
	if used, _ := budget.Report("session"); used != 3500 {
		t.Fatalf("out-of-order used = %d, want 3500", used)
	}
	if grant := budget.Reserve("session", 10000).Grant; grant != 3250 {
		t.Fatalf("next grant = %d, want 3250", grant)
	}
}

func equalInts(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
