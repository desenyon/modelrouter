package predict

import (
	"math"

	"github.com/desenyon/modelrouter/internal/catalog"
)

// ridge is a multi-output ridge regression from embeddings to
// [difficulty, axes…, log(out)], fit in closed form by Cholesky.
type ridge struct {
	dim    int
	w      [][]float64 // outputs × (dim+nLex+1); last column is bias
	mu, sd Lex         // standardization of lexical features
}

const ridgeOutputs = 2 + int(catalog.NumAxes)

func fitRidge(ex []Exemplar, dim int, lambda float64) *ridge {
	r := &ridge{dim: dim, w: make([][]float64, ridgeOutputs)}
	for _, e := range ex {
		for j := range r.mu {
			r.mu[j] += e.Lex[j]
		}
	}
	for j := range r.mu {
		r.mu[j] /= float64(max(len(ex), 1))
	}
	for _, e := range ex {
		for j := range r.sd {
			d := e.Lex[j] - r.mu[j]
			r.sd[j] += d * d
		}
	}
	for j := range r.sd {
		r.sd[j] = math.Sqrt(r.sd[j]/float64(max(len(ex), 1))) + 1e-9
	}
	n := dim + nLex + 1
	ata := make([]float64, n*n)
	aty := make([][]float64, ridgeOutputs)
	for o := range aty {
		aty[o] = make([]float64, n)
	}
	x := make([]float64, n)
	var y [ridgeOutputs]float64
	for _, e := range ex {
		r.fill(x, e.Vec, e.Lex)
		y[0] = e.D
		for a := 0; a < int(catalog.NumAxes); a++ {
			y[1+a] = e.Axes[a]
		}
		y[ridgeOutputs-1] = e.LogOut
		w := e.Weight
		for i := 0; i < n; i++ {
			xi := x[i] * w
			if xi == 0 {
				continue
			}
			row := ata[i*n:]
			for j := i; j < n; j++ {
				row[j] += xi * x[j]
			}
			for o := 0; o < ridgeOutputs; o++ {
				aty[o][i] += xi * y[o]
			}
		}
	}
	for i := 0; i < n; i++ {
		for j := 0; j < i; j++ {
			ata[i*n+j] = ata[j*n+i]
		}
		if i < n-1 {
			ata[i*n+i] += lambda // don't regularize bias
		} else {
			ata[i*n+i] += 1e-6
		}
	}
	l := cholesky(ata, n)
	for o := 0; o < ridgeOutputs; o++ {
		r.w[o] = cholSolve(l, n, aty[o])
	}
	return r
}

func (r *ridge) fill(x []float64, v []float32, lex Lex) {
	for i, f := range v {
		x[i] = float64(f)
	}
	for j := range lex {
		x[r.dim+j] = (lex[j] - r.mu[j]) / r.sd[j]
	}
	x[r.dim+nLex] = 1
}

func (r *ridge) predict(v []float32, lex Lex) (d float64, axes catalog.Vec, logOut float64) {
	if r == nil {
		return 0.3, axes, math.Log(300)
	}
	var buf [512]float64
	var x []float64
	if n := r.dim + nLex + 1; n <= len(buf) {
		x = buf[:n]
	} else {
		x = make([]float64, n)
	}
	r.fill(x, v, lex)
	out := func(o int) float64 {
		s := 0.0
		for i, w := range r.w[o] {
			s += w * x[i]
		}
		return s
	}
	d = clamp(out(0), 0, 1)
	for a := 0; a < int(catalog.NumAxes); a++ {
		axes[a] = out(1 + a)
	}
	logOut = out(ridgeOutputs - 1)
	return
}

// cholesky returns lower-triangular L with A = L·Lᵀ (A is n×n, SPD).
func cholesky(a []float64, n int) []float64 {
	l := make([]float64, n*n)
	for i := 0; i < n; i++ {
		for j := 0; j <= i; j++ {
			s := a[i*n+j]
			li, lj := l[i*n:i*n+j], l[j*n:j*n+j]
			for k := range li {
				s -= li[k] * lj[k]
			}
			if i == j {
				if s <= 0 {
					s = 1e-9
				}
				l[i*n+i] = math.Sqrt(s)
			} else {
				l[i*n+j] = s / l[j*n+j]
			}
		}
	}
	return l
}

func cholSolve(l []float64, n int, b []float64) []float64 {
	y := make([]float64, n)
	for i := 0; i < n; i++ {
		s := b[i]
		for k := 0; k < i; k++ {
			s -= l[i*n+k] * y[k]
		}
		y[i] = s / l[i*n+i]
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := y[i]
		for k := i + 1; k < n; k++ {
			s -= l[k*n+i] * x[k]
		}
		x[i] = s / l[i*n+i]
	}
	return x
}
