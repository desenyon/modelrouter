package learn

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/desenyon/modelrouter/internal/catalog"
	emb "github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/predict"
)

func setup(t *testing.T) (*Learner, *catalog.Catalog, *predict.Predictor) {
	dir := emb.DefaultDir()
	if err := emb.Present(dir); err != nil {
		t.Skipf("embedder not available: %v", err)
	}
	m, err := emb.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := predict.New(m, predict.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := catalog.New(catalog.Builtin())
	return New(cat, p, Config{StatePath: filepath.Join(t.TempDir(), "state.json")}), cat, p
}

func decision(p *predict.Predictor, id, model, text string, d float64) *Decision {
	pr := p.PredictText(text)
	v, _ := p.Embed(predict.Labeled{Text: text, D: d, O: 300}, 1, false).Vec, 0
	return &Decision{ID: id, Vec: v, Axes: pr.Axes, Difficulty: d, ModelID: model, At: time.Now(), OutTokens: 300}
}

func TestNegativeFeedbackLowersAbility(t *testing.T) {
	l, cat, p := setup(t)
	m, _ := cat.Lookup("gpt-6-luna")
	before := l.Ability(m)
	for i := 0; i < 20; i++ {
		d := decision(p, string(rune('a'+i)), m.ID, "implement a lock-free queue", 0.5)
		l.Remember(d, "", "")
		if err := l.Feedback(d.ID, 0); err != nil {
			t.Fatal(err)
		}
	}
	after := l.Ability(m)
	if after[catalog.Coding] >= before[catalog.Coding] {
		t.Fatalf("coding ability should drop: %.3f → %.3f", before[catalog.Coding], after[catalog.Coding])
	}
	if after[catalog.Coding] < before[catalog.Coding]-0.12-1e-9 {
		t.Fatal("offset must be capped at MaxDelta")
	}
}

func TestRegenerationIsNegativeAndContinuationPositive(t *testing.T) {
	l, cat, p := setup(t)
	m, _ := cat.Lookup("gpt-6-luna")
	d1 := decision(p, "r1", m.ID, "write a poem", 0.2)
	l.Remember(d1, "F1", "")
	d2 := decision(p, "r2", m.ID, "write a poem", 0.2)
	if sigs := l.Remember(d2, "F1", ""); len(sigs) != 1 || sigs[0] != SignalRegenerate {
		t.Fatalf("expected regenerate signal, got %v", sigs)
	}
	d3 := decision(p, "r3", m.ID, "now shorter", 0.1)
	if sigs := l.Remember(d3, "F3", "F1"); len(sigs) != 1 || sigs[0] != SignalContinue {
		t.Fatalf("expected continue signal for r2, got %v", sigs)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	l, cat, p := setup(t)
	m, _ := cat.Lookup("claude-sonnet-5-5")
	d := decision(p, "x", m.ID, "debug this deadlock", 0.6)
	l.Remember(d, "", "")
	_ = l.Feedback("x", 0)
	if err := l.Save("potion"); err != nil {
		t.Fatal(err)
	}
	l2 := New(cat, p, l.cfg)
	if err := l2.Load("potion"); err != nil {
		t.Fatal(err)
	}
	if l2.Ability(m) != l.Ability(m) {
		t.Fatal("abilities should survive a restart")
	}
	if err := l.Feedback("missing", 1); err != ErrUnknownRequest {
		t.Fatalf("want ErrUnknownRequest, got %v", err)
	}
}
