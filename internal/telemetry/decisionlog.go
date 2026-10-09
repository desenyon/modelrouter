package telemetry

import (
	"bufio"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// DecisionLog writes sampled routing records as JSONL from a background
// goroutine; when the buffer is full records are dropped rather than
// blocking the request path.
type DecisionLog struct {
	writeErr  error        // written by run, read only after wg.Wait
	mu        sync.RWMutex // guards channel closure against concurrent Log
	closed    bool
	closeOnce sync.Once
	closeErr  error
	ch        chan any
	sample    float64
	dropped   atomic.Uint64
	wg        sync.WaitGroup
	f         *os.File
}

// OpenDecisionLog opens path for appending. sample ∈ (0,1] is the fraction kept.
func OpenDecisionLog(path string, sample float64) (*DecisionLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if sample <= 0 || sample > 1 {
		sample = 1
	}
	d := &DecisionLog{ch: make(chan any, 4096), sample: sample, f: f}
	d.wg.Add(1)
	go d.run()
	return d, nil
}

func (d *DecisionLog) run() {
	defer d.wg.Done()
	w := bufio.NewWriterSize(d.f, 256<<10)
	enc := json.NewEncoder(w)
	recordError := func(err error) {
		if d.writeErr == nil {
			d.writeErr = err
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case rec, ok := <-d.ch:
			if !ok {
				recordError(w.Flush())
				return
			}
			recordError(enc.Encode(rec))
		case <-tick.C:
			recordError(w.Flush())
		}
	}
}

// Log enqueues a record (subject to sampling).
func (d *DecisionLog) Log(rec any) {
	if d == nil {
		return
	}
	if d.sample < 1 && rand.Float64() > d.sample {
		return
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return
	}
	select {
	case d.ch <- rec:
	default:
		d.dropped.Add(1)
	}
}

// Close flushes and closes the log.
func (d *DecisionLog) Close() error {
	if d == nil {
		return nil
	}
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		close(d.ch)
		d.mu.Unlock()
		d.wg.Wait()
		d.closeErr = errors.Join(d.writeErr, d.f.Close())
	})
	return d.closeErr
}
