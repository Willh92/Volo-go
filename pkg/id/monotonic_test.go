package id

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestMonotonicGeneratorStrictOrdering(t *testing.T) {
	g := NewGenerator()
	const count = 10000
	var prev int64
	for i := 0; i < count; i++ {
		raw := g.Next()
		val, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatalf("expected numeric ID, got %q: %v", raw, err)
		}
		if val <= prev {
			t.Fatalf("non-monotonic ID at %d: prev=%d, got=%d", i, prev, val)
		}
		prev = val
	}
}

func TestMonotonicGeneratorConcurrentNoSleep(t *testing.T) {
	g := NewGenerator()
	const workers = 32
	const perWorker = 1000
	const total = workers * perWorker

	results := make(chan string, total)
	start := time.Now()

	var wg sync.WaitGroup
	wg.Add(workers)
	for w := 0; w < workers; w++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				results <- g.Next()
			}
		}()
	}
	wg.Wait()
	close(results)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("generator took too long (%v) for %d IDs; expected lock-free non-sleeping execution", elapsed, total)
	}

	seen := make(map[string]struct{}, total)
	for v := range results {
		if _, dup := seen[v]; dup {
			t.Fatalf("duplicate ID generated under concurrency: %s", v)
		}
		seen[v] = struct{}{}
	}
	if len(seen) != total {
		t.Fatalf("expected %d unique IDs, got %d", total, len(seen))
	}
}

func TestRandomHexAndPrefixed(t *testing.T) {
	p := NextPrefixed("tok")
	if len(p) < 17 || p[:4] != "tok_" {
		t.Fatalf("unexpected prefixed ID: %s", p)
	}
	h1, err := RandomHex(16)
	if err != nil || len(h1) != 32 {
		t.Fatalf("unexpected RandomHex(16): %q, err=%v", h1, err)
	}
	h2, err := RandomHex(16)
	if err != nil || h1 == h2 {
		t.Fatalf("expected distinct random hex values, got %q and %q", h1, h2)
	}
	if _, err := RandomHex(0); err == nil {
		t.Fatal("expected error on non-positive RandomHex length")
	}
}
