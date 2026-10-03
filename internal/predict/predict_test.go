package predict

import (
	"math"
	"testing"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	emb "github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/features"
)

func newTestPredictor(t testing.TB) *Predictor {
	t.Helper()
	dir := emb.DefaultDir()
	if err := emb.Present(dir); err != nil {
		t.Skipf("embedder not available: %v", err)
	}
	m, err := emb.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(m, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func userReq(text string) *canon.Request {
	return &canon.Request{Messages: []canon.Message{{Role: canon.RoleUser, Parts: []canon.Part{{Type: canon.PartText, Text: text}}}}}
}

func TestDifficultyOrdering(t *testing.T) {
	p := newTestPredictor(t)
	cases := []struct {
		text   string
		lo, hi float64
	}{
		{"hi", 0, 0.1},
		{"what's 2+2", 0, 0.2},
		{"Write a Python function that returns the nth Fibonacci number", 0, 0.35},
		{"Implement a wait-free concurrent ring buffer in Rust with correct atomics and explain the memory model guarantees", 0.6, 1},
		{"Prove that every planar graph is 5-colorable", 0.4, 1},
		{"Write me a haiku about rain", 0, 0.3},
	}
	for _, c := range cases {
		req := userReq(c.text)
		pr, _, _ := p.Predict(req, features.Extract(req))
		if pr.Difficulty < c.lo || pr.Difficulty > c.hi {
			t.Errorf("%q: difficulty %.3f not in [%.2f, %.2f] (knn=%.3f ridge=%.3f sim=%.3f)", c.text, pr.Difficulty, c.lo, c.hi, pr.KNN, pr.Ridge, pr.MaxSim)
		}
	}
}

func TestAxesSumToOne(t *testing.T) {
	p := newTestPredictor(t)
	pr := p.PredictText("Solve the integral of x^2 sin x dx")
	s := 0.0
	for _, v := range pr.Axes {
		s += v
	}
	if math.Abs(s-1) > 1e-9 {
		t.Fatalf("axes sum %f", s)
	}
	if pr.Axes[catalog.Math] < 0.3 {
		t.Fatalf("math axis too low: %v", pr.Axes.Map())
	}
}

func TestStructureRaisesDifficulty(t *testing.T) {
	p := newTestPredictor(t)
	base := userReq("Summarize the document")
	pr0, _, _ := p.Predict(base, features.Extract(base))
	long := userReq("Summarize the document")
	big := make([]byte, 400000)
	for i := range big {
		big[i] = "abcdefgh "[i%9]
	}
	long.Messages = append([]canon.Message{{Role: canon.RoleUser, Parts: []canon.Part{{Type: canon.PartText, Text: string(big)}}}}, long.Messages...)
	pr1, _, _ := p.Predict(long, features.Extract(long))
	if pr1.Difficulty <= pr0.Difficulty || pr1.Axes[catalog.LongContext] <= pr0.Axes[catalog.LongContext] {
		t.Fatalf("long context should raise difficulty: %.3f → %.3f", pr0.Difficulty, pr1.Difficulty)
	}
}

func TestSuccessProbMonotone(t *testing.T) {
	if !(SuccessProb(0.9, 0.5) > SuccessProb(0.7, 0.5) && SuccessProb(0.7, 0.3) > SuccessProb(0.7, 0.6)) {
		t.Fatal("success probability must increase with ability and decrease with difficulty")
	}
}

func BenchmarkPredict(b *testing.B) {
	p := newTestPredictor(b)
	req := userReq("There's a race in this Go code that deadlocks under load every few hours. Find it and fix it.")
	f := features.Extract(req)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Predict(req, f)
	}
}
