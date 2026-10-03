// Package predict estimates, for one request, how hard it is (difficulty δ on
// a shared 0–1 scale), which skills it exercises (axis weights), how long the
// answer will be, and — given a model's ability vector — the probability the
// model succeeds. Semantic signals come from the local embedder (kNN over a
// labeled exemplar bank blended with a ridge-regression head); structural
// features adjust for things embeddings can't see (context size, tools,
// schemas, constraint density).
package predict

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/desenyon/modelrouter/internal/canon"
	"github.com/desenyon/modelrouter/internal/catalog"
	emb "github.com/desenyon/modelrouter/internal/embed"
	"github.com/desenyon/modelrouter/internal/features"
)

//go:embed data/*.jsonl
var bankFS embed.FS

// Labeled is one labeled prompt (bank / held-out format).
type Labeled struct {
	Text string             `json:"t"`
	D    float64            `json:"d"`
	A    map[string]float64 `json:"a"`
	O    float64            `json:"o"`
}

// Exemplar is a labeled, embedded prompt.
type Exemplar struct {
	Text    string
	D       float64
	Axes    catalog.Vec
	LogOut  float64
	Vec     []float32
	Lex     Lex
	Weight  float64
	Learned bool
}

// Neighbor is a kNN hit reported in explanations.
type Neighbor struct {
	Text string  `json:"text"`
	Sim  float64 `json:"sim"`
	D    float64 `json:"d"`
}

// Prediction is the predictor's output for one request.
type Prediction struct {
	Difficulty  float64     `json:"difficulty"`  // final δ
	Uncertainty float64     `json:"uncertainty"` // σ of δ
	Axes        catalog.Vec `json:"-"`
	OutTokens   int         `json:"out_tokens"` // expected visible output tokens
	Semantic    float64     `json:"semantic_difficulty"`
	KNN         float64     `json:"knn_difficulty"`
	Ridge       float64     `json:"ridge_difficulty"`
	MaxSim      float64     `json:"max_similarity"`
	Neighbors   []Neighbor  `json:"neighbors,omitempty"`
	Adjust      []string    `json:"adjustments,omitempty"`
	EmbedTokens int         `json:"embed_tokens"`
}

// AxesMap renders axis weights by name.
func (p Prediction) AxesMap() map[string]float64 { return p.Axes.Map() }

// MarshalJSON includes the axes map.
func (p Prediction) MarshalJSON() ([]byte, error) {
	type alias Prediction
	return json.Marshal(struct {
		alias
		Axes map[string]float64 `json:"axes"`
	}{alias(p), p.Axes.Map()})
}

// Config tunes the predictor.
type Config struct {
	K           int     // neighbors
	Temperature float64 // softmax temperature over cosine similarity
	Lambda      float64 // ridge regularization
	MaxLearned  int     // cap on learned exemplars
	// kNN weight α = clamp((maxSim − BlendCenter)/BlendWidth, BlendLo, BlendHi);
	// the ridge head gets 1 − α.
	BlendCenter, BlendWidth, BlendLo, BlendHi float64
}

// DefaultConfig values were chosen by leave-one-out evaluation on the bank
// (see `modelrouter eval`).
func DefaultConfig() Config {
	return Config{K: 8, Temperature: 0.02, Lambda: 0.3, MaxLearned: 20000, BlendCenter: 0.30, BlendWidth: 0.4, BlendLo: 0.7, BlendHi: 0.85}
}

type bank struct {
	ex    []Exemplar
	ridge *ridge
}

// Predictor is safe for concurrent use. Learned exemplars are added with
// copy-on-write so the hot path never locks.
type Predictor struct {
	emb  *emb.Model
	cfg  Config
	bank atomic.Pointer[bank]
	mu   sync.Mutex // serializes writers
	pool sync.Pool  // []float32 scratch
}

// New embeds the built-in bank and fits the ridge head.
func New(m *emb.Model, cfg Config) (*Predictor, error) {
	if m == nil {
		return nil, fmt.Errorf("predict: embedder is required")
	}
	if cfg.K <= 0 {
		cfg = DefaultConfig()
	}
	if cfg.BlendWidth == 0 {
		d := DefaultConfig()
		cfg.BlendCenter, cfg.BlendWidth, cfg.BlendLo, cfg.BlendHi = d.BlendCenter, d.BlendWidth, d.BlendLo, d.BlendHi
	}
	items, err := LoadBuiltinBank()
	if err != nil {
		return nil, err
	}
	p := &Predictor{emb: m, cfg: cfg}
	p.pool.New = func() any { s := make([]float32, m.Dim()); return &s }
	ex := make([]Exemplar, 0, len(items))
	for _, it := range items {
		ex = append(ex, p.Embed(it, 1, false))
	}
	b := &bank{ex: ex}
	b.ridge = fitRidge(ex, m.Dim(), cfg.Lambda)
	p.bank.Store(b)
	return p, nil
}

// LoadBuiltinBank returns the embedded labeled exemplars.
func LoadBuiltinBank() ([]Labeled, error) {
	var out []Labeled
	err := fs.WalkDir(bankFS, "data", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := bankFS.ReadFile(path)
		if err != nil {
			return err
		}
		items, err := ParseLabeled(b)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, items...)
		return nil
	})
	return out, err
}

// ParseLabeled parses JSONL labeled prompts.
func ParseLabeled(b []byte) ([]Labeled, error) {
	var out []Labeled
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for sc.Scan() {
		line++
		t := bytes.TrimSpace(sc.Bytes())
		if len(t) == 0 || t[0] == '#' {
			continue
		}
		var l Labeled
		if err := json.Unmarshal(t, &l); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if l.O <= 0 {
			l.O = 300
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// AxesFromMap converts named weights to a normalized vector.
func AxesFromMap(m map[string]float64) catalog.Vec {
	var v catalog.Vec
	for k, w := range m {
		if a, ok := catalog.ParseAxis(k); ok {
			v[a] = w
		}
	}
	return normAxes(v)
}

// Lex are lexical features fed to the ridge head next to the embedding.
type Lex [nLex]float64

const nLex = 3

// LexFrom converts embedder statistics to ridge features.
func LexFrom(st emb.Stats) Lex {
	return Lex{math.Log1p(float64(st.Tokens)), st.MeanNorm, st.MaxNorm}
}

// Embed converts a labeled item into an exemplar.
func (p *Predictor) Embed(l Labeled, weight float64, learned bool) Exemplar {
	v := make([]float32, p.emb.Dim())
	st := p.emb.EncodeStats(l.Text, v)
	return Exemplar{Text: l.Text, D: l.D, Axes: AxesFromMap(l.A), LogOut: math.Log(l.O), Vec: v, Lex: LexFrom(st), Weight: weight, Learned: learned}
}

// Size returns (builtin, learned) exemplar counts.
func (p *Predictor) Size() (builtin, learned int) {
	for _, e := range p.bank.Load().ex {
		if e.Learned {
			learned++
		} else {
			builtin++
		}
	}
	return
}

// Learned returns a copy of learned exemplars (for persistence).
func (p *Predictor) Learned() []Exemplar {
	var out []Exemplar
	for _, e := range p.bank.Load().ex {
		if e.Learned {
			out = append(out, e)
		}
	}
	return out
}

// AddLearned appends learned exemplars (copy-on-write); refit re-fits ridge.
func (p *Predictor) AddLearned(ex []Exemplar, refit bool) {
	if len(ex) == 0 && !refit {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	old := p.bank.Load()
	nb := &bank{ex: make([]Exemplar, 0, len(old.ex)+len(ex)), ridge: old.ridge}
	learned := 0
	for _, e := range old.ex {
		if e.Learned {
			learned++
		}
	}
	drop := learned + len(ex) - p.cfg.MaxLearned
	for _, e := range old.ex {
		if e.Learned && drop > 0 {
			drop-- // FIFO eviction of oldest learned exemplars
			continue
		}
		nb.ex = append(nb.ex, e)
	}
	for _, e := range ex {
		e.Learned = true
		nb.ex = append(nb.ex, e)
	}
	if refit {
		nb.ridge = fitRidge(nb.ex, p.emb.Dim(), p.cfg.Lambda)
	}
	p.bank.Store(nb)
}

// RequestVector embeds the routing-relevant text of a request into out:
// the last user turn dominates; system prompt and the latest tool result add
// domain context. Lexical stats describe the last user turn.
func (p *Predictor) RequestVector(req *canon.Request, out []float32) (int, Lex) {
	user := req.LastUserText()
	st := p.emb.EncodeStats(user, out)
	n := st.Tokens
	lex := LexFrom(st)
	sys := req.SystemText()
	var extra []struct {
		text string
		w    float32
	}
	if len(sys) > 40 {
		if len(sys) > 1500 {
			sys = sys[:1500]
		}
		extra = append(extra, struct {
			text string
			w    float32
		}{sys, 0.2})
	}
	if req.InToolLoop() {
		t := req.Messages[len(req.Messages)-1].Text()
		if len(t) > 800 {
			t = t[:800]
		}
		extra = append(extra, struct {
			text string
			w    float32
		}{t, 0.15})
	}
	if len(extra) == 0 {
		return n, lex
	}
	sp := p.pool.Get().(*[]float32)
	defer p.pool.Put(sp)
	tmp := *sp
	wu := float32(1)
	if n == 0 {
		wu = 0
	}
	for i := range out {
		out[i] *= wu
	}
	for _, e := range extra {
		if k := p.emb.EncodeInto(e.text, tmp); k > 0 {
			for i := range out {
				out[i] += e.w * tmp[i]
			}
			n += k
		}
	}
	emb.Normalize(out)
	return n, lex
}

// Predict estimates difficulty, axes and output length for req. It also
// returns the request embedding and lexical features (kept for learning).
func (p *Predictor) Predict(req *canon.Request, f features.Features) (Prediction, []float32, Lex) {
	vec := make([]float32, p.emb.Dim())
	n, lex := p.RequestVector(req, vec)
	pr := p.PredictVec(vec, lex, n, true)
	applyStructure(&pr, f)
	return pr, vec, lex
}

// PredictText is a convenience for single-prompt evaluation (no structure).
func (p *Predictor) PredictText(text string) Prediction {
	v := make([]float32, p.emb.Dim())
	st := p.emb.EncodeStats(text, v)
	return p.PredictVec(v, LexFrom(st), st.Tokens, true)
}

type scored struct {
	i   int
	sim float32
}

// PredictVec runs kNN + ridge on an embedding.
func (p *Predictor) PredictVec(v []float32, lex Lex, ntok int, neighbors bool) Prediction {
	return p.predictVec(p.bank.Load(), v, lex, ntok, neighbors, -1)
}

// PredictLOO predicts while excluding exemplar index skip (leave-one-out).
func (p *Predictor) PredictLOO(v []float32, lex Lex, skip int) Prediction {
	return p.predictVec(p.bank.Load(), v, lex, 1, false, skip)
}

// Bank exposes the exemplar slice (read-only).
func (p *Predictor) Bank() []Exemplar { return p.bank.Load().ex }

func (p *Predictor) predictVec(b *bank, v []float32, lex Lex, ntok int, neighbors bool, skip int) Prediction {
	pr := Prediction{EmbedTokens: ntok}
	if ntok == 0 || len(b.ex) == 0 {
		// Nothing to embed (e.g. image-only turn): neutral prior.
		pr.Difficulty, pr.Semantic, pr.Uncertainty = 0.3, 0.3, 0.25
		pr.Axes = normAxes(catalog.Vec{catalog.Knowledge: 1})
		pr.OutTokens = 300
		return pr
	}
	k := p.cfg.K
	top := make([]scored, 0, k+1)
	for i := range b.ex {
		if i == skip {
			continue
		}
		s := emb.Dot(v, b.ex[i].Vec)
		if len(top) < k {
			top = append(top, scored{i, s})
			if len(top) == k {
				sort.Slice(top, func(a, c int) bool { return top[a].sim > top[c].sim })
			}
			continue
		}
		if s <= top[k-1].sim {
			continue
		}
		j := k - 1
		for j > 0 && top[j-1].sim < s {
			top[j] = top[j-1]
			j--
		}
		top[j] = scored{i, s}
	}
	if len(top) < k {
		sort.Slice(top, func(a, c int) bool { return top[a].sim > top[c].sim })
	}
	maxSim := float64(top[0].sim)
	var wsum, dsum, osum float64
	var axes catalog.Vec
	ws := make([]float64, len(top))
	for j, t := range top {
		e := &b.ex[t.i]
		w := math.Exp((float64(t.sim)-maxSim)/p.cfg.Temperature) * e.Weight
		ws[j] = w
		wsum += w
		dsum += w * e.D
		osum += w * e.LogOut
		for a := range axes {
			axes[a] += w * e.Axes[a]
		}
	}
	dk := dsum / wsum
	var varsum float64
	for j, t := range top {
		d := b.ex[t.i].D - dk
		varsum += ws[j] * d * d
	}
	sdK := math.Sqrt(varsum / wsum)
	for a := range axes {
		axes[a] /= wsum
	}
	outK := osum / wsum

	dr, axR, outR := b.ridge.predict(v, lex)
	// Trust kNN more when a close neighbor exists; ridge generalizes better
	// when the request is far from every exemplar.
	alpha := clamp((maxSim-p.cfg.BlendCenter)/p.cfg.BlendWidth, p.cfg.BlendLo, p.cfg.BlendHi)
	sem := alpha*dk + (1-alpha)*dr
	for a := range axes {
		axes[a] = alpha*axes[a] + (1-alpha)*math.Max(0, axR[a])
	}
	logOut := alpha*outK + (1-alpha)*outR

	unc := math.Sqrt(sdK*sdK+(dk-dr)*(dk-dr)/4) + 0.25*math.Max(0, 0.8-maxSim)
	pr.Semantic = clamp(sem, 0, 1)
	pr.Difficulty = pr.Semantic
	pr.KNN, pr.Ridge = round3(dk), round3(dr)
	pr.MaxSim = round3(maxSim)
	pr.Uncertainty = clamp(unc, 0.02, 0.4)
	pr.Axes = normAxes(axes)
	pr.OutTokens = int(math.Exp(clamp(logOut, math.Log(10), math.Log(20000))))
	if neighbors {
		for j := 0; j < len(top) && j < 3; j++ {
			e := &b.ex[top[j].i]
			pr.Neighbors = append(pr.Neighbors, Neighbor{Text: trunc(e.Text, 90), Sim: round3(float64(top[j].sim)), D: e.D})
		}
	}
	return pr
}

// applyStructure adjusts the semantic estimate with structural features.
func applyStructure(pr *Prediction, f features.Features) {
	d := pr.Difficulty
	ax := pr.Axes
	add := func(reason string, delta float64) {
		if delta == 0 {
			return
		}
		d += delta
		pr.Adjust = append(pr.Adjust, fmt.Sprintf("%s%+.3f", reason, delta))
	}
	if f.Trivial && f.Messages <= 3 && f.Tools == 0 {
		if d > 0.05 {
			pr.Adjust = append(pr.Adjust, "trivial_cap")
		}
		d = math.Min(d, 0.05)
	}
	if f.InputTokens > 8000 {
		lc := math.Log2(float64(f.InputTokens) / 8000)
		add("long_context", math.Min(0.15, 0.03*lc))
		ax[catalog.LongContext] += math.Min(0.6, 0.12*lc)
	}
	if f.Tools > 0 {
		n := math.Min(float64(f.Tools), 12)
		add("tools", math.Min(0.12, 0.03+0.008*n))
		ax[catalog.Agentic] += 0.25 + 0.03*n
		if f.ToolSchemaBytes > 6000 {
			add("tool_schemas", math.Min(0.05, float64(f.ToolSchemaBytes)/400000))
		}
	}
	if f.InToolLoop {
		ax[catalog.Agentic] += 0.3
		if f.ToolResults > 6 {
			add("long_tool_loop", math.Min(0.08, 0.01*float64(f.ToolResults-6)))
		}
	}
	if f.CodeLines > 40 {
		ax[catalog.Coding] += 0.25
		add("code_volume", math.Min(0.1, float64(f.CodeLines)/2500))
	}
	if f.StackTrace {
		ax[catalog.Coding] += 0.15
		ax[catalog.Reasoning] += 0.1
		add("stack_trace", 0.04)
	}
	if f.Diff {
		ax[catalog.Coding] += 0.15
		add("diff", 0.03)
	}
	if f.MathDensity > 0.015 {
		ax[catalog.Math] += 0.3
		add("math_density", math.Min(0.08, f.MathDensity))
	}
	if f.Constraints >= 4 {
		ax[catalog.Instruction] += math.Min(0.4, 0.05*float64(f.Constraints))
		add("constraints", math.Min(0.12, 0.02*float64(f.Constraints-3)))
	}
	if f.SchemaFields > 8 {
		ax[catalog.Instruction] += 0.2
		add("schema_complexity", math.Min(0.08, float64(f.SchemaFields)/250))
	}
	if f.Images > 0 {
		add("vision", 0.04)
	}
	if f.Files > 0 {
		ax[catalog.LongContext] += 0.2
		add("documents", 0.04)
	}
	if f.UserTurns > 8 {
		add("long_conversation", 0.03)
	}
	pr.Difficulty = clamp(d, 0, 1)
	pr.Axes = normAxes(ax)
	out := float64(pr.OutTokens)
	if f.LengthHint > 0 {
		out = math.Max(out, float64(f.LengthHint)*1.1)
	}
	if f.MaxTokens > 0 {
		out = math.Min(out, float64(f.MaxTokens))
	}
	pr.OutTokens = int(math.Max(1, out))
}

// IRT success model: P = σ(k·(θ − δ) + c). With k=9, c=1, a model whose
// effective ability equals the difficulty succeeds ~73% of the time, +0.2
// margin → 94%, −0.2 → 31%.
const (
	irtK = 9.0
	irtC = 1.0
)

// EffortLevels in ascending order.
var EffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

var effortAbility = map[string]float64{"none": -0.08, "minimal": -0.06, "low": -0.035, "medium": 0, "high": 0.025, "xhigh": 0.04, "max": 0.05}

// Reasoning-token multiplier relative to visible output, before difficulty scaling.
var effortThink = map[string]float64{"none": 0, "minimal": 0.1, "low": 0.4, "medium": 1.0, "high": 2.2, "xhigh": 3.5, "max": 5.0}

// ReasonWeight is how much the request's skill mix benefits from thinking.
func ReasonWeight(axes catalog.Vec) float64 {
	return axes[catalog.Reasoning] + axes[catalog.Math] + 0.8*axes[catalog.Coding] + 0.6*axes[catalog.Agentic] +
		0.4*axes[catalog.Instruction] + 0.3*axes[catalog.LongContext] + 0.15*(axes[catalog.Knowledge]+axes[catalog.Creative])
}

// EffectiveAbility is θ for a model at an effort level on this request.
func EffectiveAbility(ability catalog.Vec, defaultEffort, effort string, axes catalog.Vec) float64 {
	th := ability.Dot(axes)
	if effort != "" && defaultEffort != "" {
		th += (effortAbility[effort] - effortAbility[defaultEffort]) * ReasonWeight(axes)
	}
	return th
}

// SuccessProb returns P(success) for ability θ against difficulty δ.
func SuccessProb(theta, delta float64) float64 {
	return 1 / (1 + math.Exp(-(irtK*(theta-delta) + irtC)))
}

// SuccessGrad returns dP/dθ for the same parameters.
func SuccessGrad(theta, delta float64) float64 {
	p := SuccessProb(theta, delta)
	return irtK * p * (1 - p)
}

// ReasoningTokens estimates hidden reasoning tokens at an effort level.
func ReasoningTokens(effort string, visibleOut int, difficulty float64) int {
	m, ok := effortThink[effort]
	if !ok {
		m = effortThink["medium"]
	}
	if m == 0 {
		return 0
	}
	base := math.Max(float64(visibleOut), 150)
	return int(base * m * (0.3 + difficulty))
}

func normAxes(v catalog.Vec) catalog.Vec {
	s := 0.0
	for i := range v {
		if v[i] < 0 {
			v[i] = 0
		}
		s += v[i]
	}
	if s == 0 {
		v[catalog.Knowledge] = 1
		return v
	}
	for i := range v {
		v[i] /= s
	}
	return v
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
