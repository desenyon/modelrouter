package optimize

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/features"
	"github.com/desenyon/modelrouter/internal/predict"
	"github.com/desenyon/modelrouter/internal/session"
)

func testCatalog(t *testing.T) *catalog.Catalog {
	c, err := catalog.New(catalog.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func input(text string, d float64, axes map[string]float64, out int) Input {
	req := &canon.Request{Messages: []canon.Message{{Role: "user", Parts: []canon.Part{{Type: "text", Text: text}}}}}
	return Input{
		Req:  req,
		Feat: features.Extract(req),
		Pred: predict.Prediction{Difficulty: d, Uncertainty: 0.08, Axes: predict.AxesFromMap(axes), OutTokens: out},
		Mode: ModeBalance, Obj: DefaultObjectives()[ModeBalance],
	}
}

var now = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func TestEasyGoesToLuna(t *testing.T) {
	p, err := Optimize(testCatalog(t), input("hi", 0.03, map[string]float64{"knowledge": 1}, 20), Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if p.Chosen.Tier != catalog.Luna {
		t.Fatalf("easy request routed to %s (%s)", p.Chosen.ModelID, p.Chosen.Tier)
	}
}

func TestHardGoesToFrontier(t *testing.T) {
	p, err := Optimize(testCatalog(t), input("lock-free queue", 0.84, map[string]float64{"coding": 0.7, "reasoning": 0.3}, 2000), Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if r := p.Chosen.Tier.Rank(); r < catalog.Sol.Rank() {
		b, _ := json.MarshalIndent(p.Table, "", " ")
		t.Fatalf("hard request routed to %s (%s)\n%s", p.Chosen.ModelID, p.Chosen.Tier, b)
	}
	if !p.Chosen.MeetsFloor {
		t.Fatalf("chosen arm should meet the floor: %+v", p.Chosen)
	}
}

func TestQualityModeSpendsMore(t *testing.T) {
	in := input("design", 0.6, map[string]float64{"reasoning": 1}, 800)
	bal, _ := Optimize(testCatalog(t), in, Env{Now: now})
	in.Mode, in.Obj = ModeQuality, DefaultObjectives()[ModeQuality]
	q, _ := Optimize(testCatalog(t), in, Env{Now: now})
	if q.Chosen.P < bal.Chosen.P || q.Chosen.CostUSD < bal.Chosen.CostUSD {
		t.Fatalf("quality should buy higher P at higher cost: balance=%s P=%.3f $%.5f quality=%s P=%.3f $%.5f",
			bal.Chosen.ModelID, bal.Chosen.P, bal.Chosen.CostUSD, q.Chosen.ModelID, q.Chosen.P, q.Chosen.CostUSD)
	}
}

func TestCredentialsFilter(t *testing.T) {
	env := Env{Now: now, ProviderReady: func(p string) bool { return p == "gemini" }}
	p, err := Optimize(testCatalog(t), input("lock-free queue", 0.84, map[string]float64{"coding": 1}, 2000), env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Chosen.Model.Provider != "gemini" {
		t.Fatalf("only gemini is configured, got %s", p.Chosen.ModelID)
	}
	for _, f := range p.Fallbacks {
		if f.Model.Provider != "gemini" {
			t.Fatalf("fallback on unconfigured provider: %s", f.ModelID)
		}
	}
}

func TestTierPinRespected(t *testing.T) {
	in := input("hi", 0.03, map[string]float64{"knowledge": 1}, 20)
	in.Pin = Pin{Kind: PinTier, Tier: catalog.Astra}
	p, err := Optimize(testCatalog(t), in, Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if p.Chosen.Tier != catalog.Astra {
		t.Fatalf("astra pin ignored: %s", p.Chosen.ModelID)
	}
}

func TestModelPinRespectedEvenWhenEasy(t *testing.T) {
	cat := testCatalog(t)
	m, _ := cat.Lookup("claude-opus-5-5")
	in := input("rename x to count", 0.02, map[string]float64{"coding": 1}, 30)
	in.Pin = Pin{Kind: PinModel, Model: m}
	p, err := Optimize(cat, in, Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if p.Chosen.Model != m {
		t.Fatalf("model pin ignored: %s", p.Chosen.ModelID)
	}
}

func TestContextWindowExcludes(t *testing.T) {
	in := input("summarize", 0.3, map[string]float64{"long_context": 1}, 500)
	in.Feat.InputTokens = 400_000 // > Haiku's 200K
	p, err := Optimize(testCatalog(t), in, Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append([]*Candidate{p.Chosen}, p.Fallbacks...) {
		if c.Model.UpstreamID == "claude-haiku-4-5" {
			t.Fatal("haiku cannot hold 400K tokens")
		}
	}
	found := false
	for _, e := range p.Excluded {
		if e.ModelID == "anthropic/claude-haiku-4-5" && e.Reason == "context_window" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected context_window exclusion for haiku")
	}
}

func TestForcedToolPrefersCapableModels(t *testing.T) {
	in := input("call the tool", 0.5, map[string]float64{"agentic": 1}, 100)
	in.Req.Tools = []canon.Tool{{Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)}}
	in.Req.ToolChoice = canon.ToolChoice{Mode: canon.ToolChoiceRequired}
	in.Feat = features.Extract(in.Req)
	p, err := Optimize(testCatalog(t), in, Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Chosen.Model.Caps.ForcedToolChoice {
		t.Fatalf("forced tool choice routed to %s which can't force tools", p.Chosen.ModelID)
	}
}

func TestStickinessViaWarmCache(t *testing.T) {
	cat := testCatalog(t)
	in := input("continue", 0.45, map[string]float64{"coding": 1}, 400)
	in.Feat.InputTokens = 120_000
	sonnet, _ := cat.Lookup("claude-sonnet-5-5")
	env := Env{Now: now, SwitchUSD: 0.0005, Lambda: 1}
	cold := evaluate(sonnet, "medium", 0.5, in, env)
	in.Session = &session.State{ModelID: sonnet.ID, Provider: "anthropic", TierRank: sonnet.Tier.Rank(), InputTokens: 118_000, At: now.Add(-time.Minute)}
	warm := evaluate(sonnet, "medium", 0.5, in, env)
	if warm.CachedToks == 0 || warm.CostUSD >= cold.CostUSD/2 {
		t.Fatalf("warm prompt cache should make the session model much cheaper: cold $%.5f warm $%.5f", cold.CostUSD, warm.CostUSD)
	}
	other, _ := cat.Lookup("gpt-6.1-sol")
	sw := evaluate(other, "medium", 0.5, in, env)
	in.Session = nil
	nosw := evaluate(other, "medium", 0.5, in, env)
	if sw.Utility >= nosw.Utility {
		t.Fatal("switching away from the session model should carry a penalty")
	}
}

func TestBudgetLambdaShiftsDown(t *testing.T) {
	in := input("analysis", 0.55, map[string]float64{"reasoning": 1}, 900)
	a, _ := Optimize(testCatalog(t), in, Env{Now: now, Lambda: 1})
	b, _ := Optimize(testCatalog(t), in, Env{Now: now, Lambda: 8})
	if b.Chosen.CostUSD > a.Chosen.CostUSD {
		t.Fatalf("higher λ must not increase spend: %.5f → %.5f", a.Chosen.CostUSD, b.Chosen.CostUSD)
	}
}

func BenchmarkOptimize(b *testing.B) {
	cat, _ := catalog.New(catalog.Builtin())
	in := input("lock-free queue", 0.7, map[string]float64{"coding": 0.7, "reasoning": 0.3}, 1500)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = Optimize(cat, in, Env{Now: now})
	}
}

func TestMaxCostNeverRelaxed(t *testing.T) {
	cat := testCatalog(t)
	in := input("hi", .03, map[string]float64{"knowledge": 1}, 20)
	in.Req.Router.MaxCostUSD = 1e-12
	p, err := Optimize(cat, in, Env{Now: now})
	if err == nil || p.Chosen != nil {
		t.Fatalf("impossible cost cap admitted a model: %+v", p.Chosen)
	}
}

func TestFallbacksRespectMaxCost(t *testing.T) {
	in := input("hi", .03, map[string]float64{"knowledge": 1}, 20)
	cat := testCatalog(t)
	unlimited, err := Optimize(cat, in, Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	in.Req.Router.MaxCostUSD = unlimited.Chosen.CostUSD
	p, err := Optimize(cat, in, Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append([]*Candidate{p.Chosen}, p.Fallbacks...) {
		if c.CostUSD > in.Req.Router.MaxCostUSD {
			t.Errorf("over-budget arm %s: %g > %g", c.ModelID, c.CostUSD, in.Req.Router.MaxCostUSD)
		}
	}
}

func TestAudioOnlyRoutesToCapableModels(t *testing.T) {
	in := input("transcribe", .3, map[string]float64{"knowledge": 1}, 20)
	in.Req.Messages[0].Parts = append(in.Req.Messages[0].Parts, canon.Part{Type: canon.PartAudio, MediaType: "audio/wav", Data: "AAAA"})
	in.Feat = features.Extract(in.Req)
	p, err := Optimize(testCatalog(t), in, Env{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range append([]*Candidate{p.Chosen}, p.Fallbacks...) {
		if c.Model.Provider != "gemini" {
			t.Errorf("audio routed to unsupported adapter: %s", c.ModelID)
		}
	}
	_, err = Optimize(testCatalog(t), in, Env{Now: now, ProviderReady: func(p string) bool { return p == "openai" }})
	if err == nil {
		t.Fatal("audio accepted without a capable provider")
	}
}

func TestAudioCannotEnableAnUnsupportedNativeAdapter(t *testing.T) {
	cat := testCatalog(t)
	m, _ := cat.Lookup("gpt-6-luna")
	m.Caps.Audio = true // a catalog override cannot add protocol support
	in := input("transcribe", .3, map[string]float64{"knowledge": 1}, 20)
	in.Pin = Pin{Kind: PinModel, Model: m}
	in.Req.Messages[0].Parts = []canon.Part{{Type: canon.PartAudio, MediaType: "audio/wav", Data: "AAAA"}}
	in.Feat = features.Extract(in.Req)
	if _, err := Optimize(cat, in, Env{Now: now}); err == nil {
		t.Fatal("unsupported adapter admitted audio")
	}
}

func TestPinnedModelStillRespectsMaxCost(t *testing.T) {
	cat := testCatalog(t)
	m, _ := cat.Lookup("gpt-6-astra")
	in := input("hi", .03, map[string]float64{"knowledge": 1}, 20)
	in.Pin = Pin{Kind: PinModel, Model: m}
	in.Req.Router.MaxCostUSD = 1e-12
	if _, err := Optimize(cat, in, Env{Now: now}); err == nil {
		t.Fatal("model pin bypassed cap")
	}
}
