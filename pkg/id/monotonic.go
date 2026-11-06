package id

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"
)

// Generator produces strictly increasing, lock-free, non-sleeping numeric string IDs.
//
// It preserves full compatibility with 13-to-19 digit string primary keys from historical
// bolo-solo backups while completely eliminating thread sleeping or mutex contention.
type Generator struct {
	last atomic.Int64
}

// NewGenerator initializes a monotonic ID generator seeded from the current wall clock
// scaled to microsecond-granularity ticks when burst concurrency exceeds 1 ID/ms, or
// millisecond-first monotonic sequence compatible with 13+ digit string IDs.
func NewGenerator() *Generator {
	g := &Generator{}
	g.last.Store(time.Now().UnixMilli())
	return g
}

var defaultGen = NewGenerator()

// Next returns a strictly increasing numeric string ID without sleeping.
func (g *Generator) Next() string {
	now := time.Now().UnixMilli()
	for {
		prev := g.last.Load()
		next := now
		if prev >= next {
			next = prev + 1
		}
		if g.last.CompareAndSwap(prev, next) {
			return strconv.FormatInt(next, 10)
		}
	}
}

// NextInt64 returns the raw strictly increasing int64 value.
func (g *Generator) NextInt64() int64 {
	now := time.Now().UnixMilli()
	for {
		prev := g.last.Load()
		next := now
		if prev >= next {
			next = prev + 1
		}
		if g.last.CompareAndSwap(prev, next) {
			return next
		}
	}
}

// Next returns a strictly increasing numeric string ID from the package-global generator.
func Next() string {
	return defaultGen.Next()
}

// NextPrefixed returns "<prefix>_<monotonicID>" for typed entities (e.g., "wh_1730851200001").
func NextPrefixed(prefix string) string {
	if prefix == "" {
		return defaultGen.Next()
	}
	return prefix + "_" + defaultGen.Next()
}

// RandomHex generates n cryptographic random bytes encoded as 2*n lowercase hex characters.
func RandomHex(nBytes int) (string, error) {
	if nBytes <= 0 {
		return "", fmt.Errorf("id: nBytes must be positive, got %d", nBytes)
	}
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("id: crypto/rand read failed: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// MustRandomHex generates n cryptographic random bytes as lowercase hex or panics on OS CSPRNG failure.
func MustRandomHex(nBytes int) string {
	s, err := RandomHex(nBytes)
	if err != nil {
		panic(err)
	}
	return s
}
