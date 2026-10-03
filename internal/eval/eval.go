// Package eval measures predictor quality: leave-one-out on the exemplar bank
// and generalization on a held-out set the predictor never sees.
package eval

import (
	_ "embed"
	"fmt"
	"io"
	"math"
	"sort"
	"time"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	"github.com/desenyon/modelrouter/internal/features"
	"github.com/desenyon/modelrouter/internal/optimize"
	"github.com/desenyon/modelrouter/internal/predict"
)

//go:embed data/heldout.jsonl
var heldoutData []byte

// HeldOut returns the held-out labeled set.
func HeldOut() ([]predict.Labeled, error) { return predict.ParseLabeled(heldoutData) }

// Metrics summarizes difficulty prediction quality.
type Metrics struct {
	N         int     `json:"n"`
	MAE       float64 `json:"mae"`
	RMSE      float64 `json:"rmse"`
	Spearman  float64 `json:"spearman"`
	BandAcc   float64 `json:"band_accuracy"` // same difficulty band (5 bands)
	Under     float64 `json:"under_rate"`    // predicted ≥0.2 easier than truth (risk of under-routing)
	Over      float64 `json:"over_rate"`     // predicted ≥0.2 harder than truth (wasted spend)
	AxisTop1  float64 `json:"axis_top1"`     // dominant skill axis matches
	OutLogMAE float64 `json:"out_log_mae"`   // |log(pred/true)| for output length
	Worst     []Miss  `json:"worst,omitempty"`
}

// Miss is one large error.
type Miss struct {
	Text string  `json:"text"`
	True float64 `json:"true"`
	Pred float64 `json:"pred"`
}

type pair struct {
	truth, pred float64
	axT, axP    catalog.Vec
	outT, outP  float64
	text        string
}

// Band maps difficulty to five planning bands.
func Band(d float64) int {
	switch {
	case d < 0.15:
		return 0
	case d < 0.35:
		return 1
	case d < 0.55:
		return 2
	case d < 0.75:
		return 3
	default:
		return 4
	}
}

func compute(ps []pair, worst int) Metrics {
	m := Metrics{N: len(ps)}
	if len(ps) == 0 {
		return m
	}
	var abs, sq, band, under, over, ax, outl float64
	for _, p := range ps {
		e := p.pred - p.truth
		abs += math.Abs(e)
		sq += e * e
		if Band(p.pred) == Band(p.truth) {
			band++
		}
		if e <= -0.2 {
			under++
		}
		if e >= 0.2 {
			over++
		}
		if argmax(p.axT) == argmax(p.axP) {
			ax++
		}
		outl += math.Abs(math.Log(p.outP / p.outT))
	}
	n := float64(len(ps))
	m.MAE, m.RMSE = abs/n, math.Sqrt(sq/n)
	m.BandAcc, m.Under, m.Over, m.AxisTop1, m.OutLogMAE = band/n, under/n, over/n, ax/n, outl/n
	m.Spearman = spearman(ps)
	sorted := append([]pair(nil), ps...)
	sort.Slice(sorted, func(i, j int) bool {
		return math.Abs(sorted[i].pred-sorted[i].truth) > math.Abs(sorted[j].pred-sorted[j].truth)
	})
	for i := 0; i < worst && i < len(sorted); i++ {
		m.Worst = append(m.Worst, Miss{Text: sorted[i].text, True: sorted[i].truth, Pred: round3(sorted[i].pred)})
	}
	return m
}

// LeaveOneOut evaluates kNN predictions on the bank with each exemplar held out.
// (The ridge head is fit on the full bank, so LOO slightly flatters it; the
// held-out set is the honest number.)
func LeaveOneOut(p *predict.Predictor, worst int) Metrics {
	bank := p.Bank()
	ps := make([]pair, 0, len(bank))
	for i, e := range bank {
		if e.Learned {
			continue
		}
		pr := p.PredictLOO(e.Vec, e.Lex, i)
		ps = append(ps, pair{truth: e.D, pred: pr.Difficulty, axT: e.Axes, axP: pr.Axes, outT: math.Exp(e.LogOut), outP: float64(pr.OutTokens), text: e.Text})
	}
	return compute(ps, worst)
}

// Generalization evaluates on labeled items the predictor has never seen.
func Generalization(p *predict.Predictor, items []predict.Labeled, worst int) Metrics {
	ps := make([]pair, 0, len(items))
	for _, it := range items {
		pr := p.PredictText(it.Text)
		ps = append(ps, pair{truth: it.D, pred: pr.Difficulty, axT: predict.AxesFromMap(it.A), axP: pr.Axes, outT: it.O, outP: float64(pr.OutTokens), text: it.Text})
	}
	return compute(ps, worst)
}

// Print renders metrics.
func Print(w io.Writer, name string, m Metrics) {
	fmt.Fprintf(w, "%s (n=%d)\n", name, m.N)
	fmt.Fprintf(w, "  difficulty  MAE %.3f  RMSE %.3f  Spearman %.3f  band-acc %.1f%%\n", m.MAE, m.RMSE, m.Spearman, 100*m.BandAcc)
	fmt.Fprintf(w, "  risk        under-routed %.1f%%  over-routed %.1f%%  (|err| ≥ 0.2)\n", 100*m.Under, 100*m.Over)
	fmt.Fprintf(w, "  skills      top-axis match %.1f%%   output-length log-MAE %.2f\n", 100*m.AxisTop1, m.OutLogMAE)
	for _, x := range m.Worst {
		fmt.Fprintf(w, "    miss  true %.2f pred %.2f  %s\n", x.True, x.Pred, trunc(x.Text, 80))
	}
}

func argmax(v catalog.Vec) int {
	best := 0
	for i := range v {
		if v[i] > v[best] {
			best = i
		}
	}
	return best
}

func spearman(ps []pair) float64 {
	n := len(ps)
	if n < 3 {
		return 0
	}
	rank := func(get func(pair) float64) []float64 {
		idx := make([]int, n)
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return get(ps[idx[a]]) < get(ps[idx[b]]) })
		r := make([]float64, n)
		for i := 0; i < n; {
			j := i
			for j+1 < n && get(ps[idx[j+1]]) == get(ps[idx[i]]) {
				j++
			}
			avg := float64(i+j)/2 + 1
			for k := i; k <= j; k++ {
				r[idx[k]] = avg
			}
			i = j + 1
		}
		return r
	}
	rt := rank(func(p pair) float64 { return p.truth })
	rp := rank(func(p pair) float64 { return p.pred })
	var mt, mp float64
	for i := range rt {
		mt += rt[i]
		mp += rp[i]
	}
	mt /= float64(n)
	mp /= float64(n)
	var num, dt, dp float64
	for i := range rt {
		num += (rt[i] - mt) * (rp[i] - mp)
		dt += (rt[i] - mt) * (rt[i] - mt)
		dp += (rp[i] - mp) * (rp[i] - mp)
	}
	if dt == 0 || dp == 0 {
		return 0
	}
	return num / math.Sqrt(dt*dp)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// RoutingReport summarizes routing decisions on labeled prompts, judged with
// the true difficulty labels.
type RoutingReport struct {
	Mode          string             `json:"mode"`
	N             int                `json:"n"`
	TierMix       map[string]float64 `json:"tier_mix"`
	ProviderMix   map[string]float64 `json:"provider_mix"`
	MeanCostUSD   float64            `json:"mean_est_cost_usd"`
	BaselineUSD   float64            `json:"baseline_cost_usd"` // always the most capable model
	Savings       float64            `json:"savings_vs_baseline"`
	MeanTrueP     float64            `json:"mean_true_p_success"`
	BaselineP     float64            `json:"baseline_true_p_success"`
	UnderServed   float64            `json:"under_served"` // true P < 0.7
	Overspent     float64            `json:"overspent"`    // a cheaper arm had true P ≥ floor
	MeanRoutingUs float64            `json:"mean_routing_us"`
}

// SimulateRouting routes each labeled prompt through the real optimizer.
func SimulateRouting(cat *catalog.Catalog, p *predict.Predictor, items []predict.Labeled, mode optimize.Mode, obj optimize.Objective) RoutingReport {
	rep := RoutingReport{Mode: string(mode), N: len(items), TierMix: map[string]float64{}, ProviderMix: map[string]float64{}}
	var best *catalog.Model
	for _, m := range cat.All() {
		if m.Enabled && (best == nil || m.Ability.Dot(uniform()) > best.Ability.Dot(uniform())) {
			best = m
		}
	}
	now := time.Now()
	var under, over, totalUs float64
	for _, it := range items {
		req := &canon.Request{Messages: []canon.Message{{Role: canon.RoleUser, Parts: []canon.Part{{Type: canon.PartText, Text: it.Text}}}}}
		f := features.Extract(req)
		t0 := time.Now()
		pr, _, _ := p.Predict(req, f)
		plan, err := optimize.Optimize(cat, optimize.Input{Req: req, Feat: f, Pred: pr, Mode: mode, Obj: obj}, optimize.Env{Now: now})
		totalUs += float64(time.Since(t0).Microseconds())
		if err != nil {
			continue
		}
		c := plan.Chosen
		trueAxes := predict.AxesFromMap(it.A)
		trueP := func(m *catalog.Model, effort string) float64 {
			return predict.SuccessProb(predict.EffectiveAbility(m.Ability, m.DefaultEffort, effort, trueAxes), it.D)
		}
		tp := trueP(c.Model, c.Effort)
		rep.MeanTrueP += tp
		rep.MeanCostUSD += c.CostUSD
		rep.TierMix[string(c.Tier)]++
		rep.ProviderMix[c.Model.Provider]++
		if tp < 0.7 {
			under++
		}
		for _, alt := range plan.Table {
			if alt.CostUSD < c.CostUSD*0.7 && trueP(alt.Model, alt.Effort) >= obj.Floor && tp >= obj.Floor {
				over++
				break
			}
		}
		// Baseline: most capable model at its default effort.
		bin := optimize.Input{Req: req, Feat: f, Pred: pr, Mode: mode, Obj: obj, Pin: optimize.Pin{Kind: optimize.PinModel, Model: best}}
		if bp, err := optimize.Optimize(cat, bin, optimize.Env{Now: now}); err == nil {
			for _, bc := range bp.Table {
				if bc.Effort == best.DefaultEffort {
					rep.BaselineUSD += bc.CostUSD
					break
				}
			}
		}
		rep.BaselineP += trueP(best, best.DefaultEffort)
	}
	n := float64(len(items))
	for k := range rep.TierMix {
		rep.TierMix[k] /= n
	}
	for k := range rep.ProviderMix {
		rep.ProviderMix[k] /= n
	}
	rep.MeanCostUSD /= n
	rep.BaselineUSD /= n
	rep.MeanTrueP /= n
	rep.BaselineP /= n
	rep.UnderServed, rep.Overspent = under/n, over/n
	rep.MeanRoutingUs = totalUs / n
	if rep.BaselineUSD > 0 {
		rep.Savings = 1 - rep.MeanCostUSD/rep.BaselineUSD
	}
	return rep
}

func uniform() catalog.Vec {
	var v catalog.Vec
	for i := range v {
		v[i] = 1.0 / float64(catalog.NumAxes)
	}
	return v
}

// PrintRouting renders a routing report.
func PrintRouting(w io.Writer, r RoutingReport) {
	fmt.Fprintf(w, "routing simulation — mode=%s (n=%d, judged with true labels)\n", r.Mode, r.N)
	fmt.Fprintf(w, "  tier mix     luna %.0f%%  terra %.0f%%  sol %.0f%%  astra %.0f%%\n", 100*r.TierMix["luna"], 100*r.TierMix["terra"], 100*r.TierMix["sol"], 100*r.TierMix["astra"])
	fmt.Fprintf(w, "  providers    openai %.0f%%  anthropic %.0f%%  gemini %.0f%%\n", 100*r.ProviderMix["openai"], 100*r.ProviderMix["anthropic"], 100*r.ProviderMix["gemini"])
	fmt.Fprintf(w, "  cost         $%.5f/req vs $%.5f always-top-model  (%.0f%% saved)\n", r.MeanCostUSD, r.BaselineUSD, 100*r.Savings)
	fmt.Fprintf(w, "  success      mean P %.3f vs %.3f always-top-model\n", r.MeanTrueP, r.BaselineP)
	fmt.Fprintf(w, "  errors       under-served %.1f%% (true P<0.7)   overspent %.1f%%\n", 100*r.UnderServed, 100*r.Overspent)
	fmt.Fprintf(w, "  latency      %.0fµs mean decision time (embed + predict + optimize)\n", r.MeanRoutingUs)
}
