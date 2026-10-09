package telemetry

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDecisionLogCloseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "decisions.jsonl")
	d, err := OpenDecisionLog(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	d.Log(map[string]int{"request": 1})
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"request":1`) {
		t.Fatalf("record not flushed: %s %v", b, err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d.Log(map[string]int{"after_close": 1})
}

func TestDecisionLogConcurrentCloseAndLog(t *testing.T) {
	d, err := OpenDecisionLog(filepath.Join(t.TempDir(), "log.jsonl"), 1)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			for range 100 {
				d.Log(map[string]int{"n": 1})
			}
		})
		wg.Go(func() {
			if err := d.Close(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestDecisionLogReportsWriteErrorsOnClose(t *testing.T) {
	d, err := OpenDecisionLog(filepath.Join(t.TempDir(), "log.jsonl"), 1)
	if err != nil {
		t.Fatal(err)
	}
	d.Log(make(chan int)) // JSON encoding failure must not be silently lost
	if err := d.Close(); err == nil {
		t.Fatal("encoding error suppressed")
	}
}
