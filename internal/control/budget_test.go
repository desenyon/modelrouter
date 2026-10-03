package control

import (
	"testing"
	"time"
)

func TestLambdaRisesWhenOverspending(t *testing.T) {
	b := NewBudget(1) // $1/hour
	now := time.Unix(0, 0)
	b.SetClock(func() time.Time { return now })
	for i := 0; i < 10; i++ {
		b.Record(0.05) // $0.05 per 10s ≈ $18/hour
		now = now.Add(10 * time.Second)
		b.Tick()
	}
	if b.Lambda() <= 2 {
		t.Fatalf("λ should climb under sustained overspend, got %.2f", b.Lambda())
	}
	for i := 0; i < 40; i++ {
		now = now.Add(10 * time.Second)
		b.Tick() // no spend
	}
	if b.Lambda() > 1.01 {
		t.Fatalf("λ should relax back to 1, got %.2f", b.Lambda())
	}
}

func TestDisabledBudgetIsNeutral(t *testing.T) {
	b := NewBudget(0)
	b.Record(100)
	b.Tick()
	if b.Lambda() != 1 {
		t.Fatal("disabled budget must keep λ=1")
	}
}

func TestRetryBudget(t *testing.T) {
	r := NewRetryBudget(0.1)
	n := 0
	for r.Take() {
		n++
	}
	if n != 10 {
		t.Fatalf("burst should be 10, got %d", n)
	}
	for i := 0; i < 10; i++ {
		r.Deposit()
	}
	if !r.Take() || r.Take() {
		t.Fatal("10 primaries at ratio 0.1 should fund exactly one retry")
	}
}
