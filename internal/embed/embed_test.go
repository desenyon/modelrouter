package embed

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

type golden struct {
	Text string    `json:"text"`
	IDs  []int32   `json:"ids"`
	Emb  []float32 `json:"emb"`
}

// loadTestModel loads the cached default embedder or skips.
func loadTestModel(t testing.TB) *Model {
	t.Helper()
	dir := os.Getenv("MODELROUTER_EMBEDDER_DIR")
	if dir == "" {
		dir = DefaultDir()
	}
	if err := Present(dir); err != nil {
		if os.Getenv("MODELROUTER_REQUIRE_EMBEDDER") == "1" {
			t.Fatalf("required embedder not available: %v", err)
		}
		t.Skipf("embedder not available (%v); run `modelrouter embedder fetch`", err)
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Golden fixtures were produced with the reference Python model2vec package.
func TestMatchesReferenceImplementation(t *testing.T) {
	m := loadTestModel(t)
	b, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var gs []golden
	if err := json.Unmarshal(b, &gs); err != nil {
		t.Fatal(err)
	}
	for _, g := range gs {
		ids := m.Tokenizer().IDs(g.Text)
		// model2vec drops [UNK]=1 ids; reference ids include them.
		var want []int32
		for _, id := range g.IDs {
			if id != 1 {
				want = append(want, id)
			}
		}
		if len(ids) != len(want) {
			t.Fatalf("%q: ids %v want %v", g.Text, ids, want)
		}
		for i := range ids {
			if ids[i] != want[i] {
				t.Fatalf("%q: ids %v want %v", g.Text, ids, want)
			}
		}
		if len(g.Emb) == 0 {
			continue
		}
		v, _ := m.Encode(g.Text)
		for i := range v {
			if math.Abs(float64(v[i]-g.Emb[i])) > 1e-5 {
				t.Fatalf("%q: dim %d = %f want %f", g.Text, i, v[i], g.Emb[i])
			}
		}
	}
}

func TestSimilarityIsSemantic(t *testing.T) {
	m := loadTestModel(t)
	a, _ := m.Encode("implement a thread-safe lock-free queue in C++")
	b, _ := m.Encode("write a concurrent wait-free ring buffer in Rust")
	c, _ := m.Encode("what is a good recipe for banana bread")
	if Dot(a, b) <= Dot(a, c) {
		t.Fatalf("expected concurrency prompts closer: ab=%f ac=%f", Dot(a, b), Dot(a, c))
	}
}

func BenchmarkEncode(b *testing.B) {
	m := loadTestModel(b)
	text := "Write a Rust lock-free MPMC queue with correct memory orderings and explain how you avoid the ABA problem. Include tests and benchmarks."
	out := make([]float32, m.Dim())
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		m.EncodeInto(text, out)
	}
}
