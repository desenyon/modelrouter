// Package embed provides the router's local text embedder: a pure-Go
// Model2Vec static-embedding model (potion-base-8M by default). Encoding is a
// WordPiece tokenization plus a mean of token vectors — tens of microseconds,
// no CGO, no network on the hot path.
package embed

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
)

// Model is a loaded static embedding model.
type Model struct {
	tok   *WordPiece
	dim   int
	rows  int
	vecs  []float32 // rows × dim, row-major
	norms []float32 // per-row L2 norm (Zipf-weighted: rarer tokens are longer)
	name  string
}

// Stats are lexical statistics gathered while encoding.
type Stats struct {
	Tokens   int     // contributing tokens
	MeanNorm float64 // mean per-token vector norm (vocabulary rarity)
	MaxNorm  float64 // rarest token's norm
}

// Dim returns the embedding dimensionality.
func (m *Model) Dim() int { return m.dim }

// Name returns the model directory name.
func (m *Model) Name() string { return m.name }

// Tokenizer exposes the tokenizer (used for token estimates and tests).
func (m *Model) Tokenizer() *WordPiece { return m.tok }

// Load reads model.safetensors + tokenizer.json from dir.
func Load(dir string) (*Model, error) {
	tok, err := LoadWordPiece(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("embedder tokenizer: %w", err)
	}
	vecs, rows, dim, err := loadSafetensorsF32(filepath.Join(dir, "model.safetensors"), "embeddings")
	if err != nil {
		return nil, fmt.Errorf("embedder weights: %w", err)
	}
	if rows < tok.VocabSize() {
		return nil, fmt.Errorf("embedder: %d rows < vocab %d", rows, tok.VocabSize())
	}
	norms := make([]float32, rows)
	for r := 0; r < rows; r++ {
		var ss float64
		for _, x := range vecs[r*dim : r*dim+dim] {
			ss += float64(x) * float64(x)
		}
		norms[r] = float32(math.Sqrt(ss))
	}
	return &Model{tok: tok, dim: dim, rows: rows, vecs: vecs, norms: norms, name: filepath.Base(dir)}, nil
}

// MaxChars bounds the text fed to the embedder; longer inputs keep head and
// tail, where task statements and questions concentrate.
const MaxChars = 6000

// Encode returns the L2-normalized mean token embedding of text. The second
// return value is the number of tokens that contributed.
func (m *Model) Encode(text string) ([]float32, int) {
	out := make([]float32, m.dim)
	n := m.EncodeInto(text, out)
	return out, n
}

// EncodeInto writes the embedding into out (len ≥ Dim) and returns token count.
func (m *Model) EncodeInto(text string, out []float32) int {
	return m.EncodeStats(text, out).Tokens
}

// EncodeStats writes the embedding into out and returns lexical statistics.
func (m *Model) EncodeStats(text string, out []float32) Stats {
	text = clip(text)
	var st Stats
	for i := range out[:m.dim] {
		out[i] = 0
	}
	n := 0
	dim := m.dim
	m.tok.Encode(text, func(id int32) {
		if int(id) >= m.rows {
			return
		}
		row := m.vecs[int(id)*dim : int(id)*dim+dim]
		for i, v := range row {
			out[i] += v
		}
		nm := float64(m.norms[id])
		st.MeanNorm += nm
		if nm > st.MaxNorm {
			st.MaxNorm = nm
		}
		n++
	})
	st.Tokens = n
	if n == 0 {
		return st
	}
	st.MeanNorm /= float64(n)
	// Mean then normalize: the mean's scale cancels under normalization.
	Normalize(out[:dim])
	return st
}

func clip(s string) string {
	if len(s) <= MaxChars {
		return s
	}
	head := s[:MaxChars*2/3]
	tail := s[len(s)-MaxChars/3:]
	return head + " " + tail
}

// Normalize scales v to unit L2 norm in place (no-op for zero vectors).
func Normalize(v []float32) {
	var ss float64
	for _, x := range v {
		ss += float64(x) * float64(x)
	}
	if ss == 0 {
		return
	}
	inv := float32(1 / math.Sqrt(ss))
	for i := range v {
		v[i] *= inv
	}
}

// Dot returns the inner product (cosine similarity for unit vectors).
func Dot(a, b []float32) float32 {
	var s0, s1, s2, s3 float32
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for ; i+4 <= n; i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < n; i++ {
		s0 += a[i] * b[i]
	}
	return s0 + s1 + s2 + s3
}

type tensorInfo struct {
	Dtype   string   `json:"dtype"`
	Shape   []int    `json:"shape"`
	Offsets [2]int64 `json:"data_offsets"`
}

func loadSafetensorsF32(path, name string) ([]float32, int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, 0, err
	}
	defer f.Close()
	var hlen uint64
	if err := binary.Read(f, binary.LittleEndian, &hlen); err != nil {
		return nil, 0, 0, fmt.Errorf("read header length: %w", err)
	}
	if hlen > 1<<24 {
		return nil, 0, 0, fmt.Errorf("safetensors header too large (%d)", hlen)
	}
	hdr := make([]byte, hlen)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, 0, 0, err
	}
	var meta map[string]json.RawMessage
	if err := json.Unmarshal(hdr, &meta); err != nil {
		return nil, 0, 0, fmt.Errorf("parse header: %w", err)
	}
	raw, ok := meta[name]
	if !ok {
		return nil, 0, 0, fmt.Errorf("tensor %q not found", name)
	}
	var ti tensorInfo
	if err := json.Unmarshal(raw, &ti); err != nil {
		return nil, 0, 0, err
	}
	if ti.Dtype != "F32" || len(ti.Shape) != 2 {
		return nil, 0, 0, fmt.Errorf("tensor %q: want F32 2-D, got %s %v", name, ti.Dtype, ti.Shape)
	}
	rows, dim := ti.Shape[0], ti.Shape[1]
	size := ti.Offsets[1] - ti.Offsets[0]
	if size != int64(rows*dim*4) {
		return nil, 0, 0, fmt.Errorf("tensor %q: size mismatch", name)
	}
	if _, err := f.Seek(8+int64(hlen)+ti.Offsets[0], io.SeekStart); err != nil {
		return nil, 0, 0, err
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, 0, 0, err
	}
	out := make([]float32, rows*dim)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
	}
	return out, rows, dim, nil
}
