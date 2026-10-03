package eval

import (
	"fmt"
	"os"
	"testing"

	emb "github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/predict"
)

func loadModel(t *testing.T) *emb.Model {
	dir := emb.DefaultDir()
	if err := emb.Present(dir); err != nil {
		t.Skipf("embedder not available: %v", err)
	}
	m, err := emb.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Quality gate: the default predictor must generalize to the held-out set.
func TestHeldOutQuality(t *testing.T) {
	m := loadModel(t)
	p, err := predict.New(m, predict.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	items, err := HeldOut()
	if err != nil {
		t.Fatal(err)
	}
	g := Generalization(p, items, 8)
	l := LeaveOneOut(p, 0)
	if testing.Verbose() {
		Print(os.Stdout, "leave-one-out", l)
		Print(os.Stdout, "held-out", g)
	}
	if g.Spearman < 0.75 || g.MAE > 0.14 || g.Under > 0.12 {
		Print(os.Stdout, "held-out", g)
		t.Fatalf("held-out quality below gate: spearman %.3f mae %.3f under %.3f", g.Spearman, g.MAE, g.Under)
	}
}

// TestSweep prints a hyperparameter grid (run with -run Sweep -v SWEEP=1).
func TestSweep(t *testing.T) {
	if os.Getenv("SWEEP") == "" {
		t.Skip("set SWEEP=1")
	}
	m := loadModel(t)
	items, _ := HeldOut()
	for _, k := range []int{4, 6, 8, 12, 16} {
		for _, temp := range []float64{0.02, 0.04, 0.08, 0.15} {
			for _, lam := range []float64{0.1, 0.3, 1, 3} {
				p, _ := predict.New(m, predict.Config{K: k, Temperature: temp, Lambda: lam, MaxLearned: 10})
				g := Generalization(p, items, 0)
				l := LeaveOneOut(p, 0)
				fmt.Printf("k=%2d T=%.2f λ=%.1f | held MAE %.3f ρ %.3f under %.3f | loo MAE %.3f ρ %.3f\n", k, temp, lam, g.MAE, g.Spearman, g.Under, l.MAE, l.Spearman)
			}
		}
	}
}

func TestBlendSweep(t *testing.T) {
	if os.Getenv("SWEEP") == "" {
		t.Skip("set SWEEP=1")
	}
	m := loadModel(t)
	items, _ := HeldOut()
	for _, lo := range []float64{0.7, 0.8, 0.9, 1.0} {
		for _, hi := range []float64{0.85, 0.95, 1.0} {
			if hi < lo {
				continue
			}
			for _, c := range []float64{0.3} {
				cfg := predict.DefaultConfig()
				cfg.BlendLo, cfg.BlendHi, cfg.BlendCenter = lo, hi, c
				p, _ := predict.New(m, cfg)
				g := Generalization(p, items, 0)
				fmt.Printf("lo=%.1f hi=%.2f c=%.2f | MAE %.3f ρ %.3f under %.3f over %.3f\n", lo, hi, c, g.MAE, g.Spearman, g.Under, g.Over)
			}
		}
	}
}
