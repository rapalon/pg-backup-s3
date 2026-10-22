//go:build integration

package store

import (
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"
)

// patternReader yields size deterministic pseudo-random bytes.
type patternReader struct {
	rng  *rand.Rand
	left int64
}

func newPatternReader(seed uint64, size int64) *patternReader {
	return &patternReader{rng: rand.New(rand.NewPCG(seed, seed^0xdead)), left: size}
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.left <= 0 {
		return 0, io.EOF
	}
	n := len(b)
	if int64(n) > p.left {
		n = int(p.left)
	}
	for i := 0; i < n; i += 8 {
		v := p.rng.Uint64()
		for j := 0; j < 8 && i+j < n; j++ {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	p.left -= int64(n)
	return n, nil
}

// hashOf hashes everything in r.
func hashOf(t *testing.T, r io.Reader) (string, int64) {
	t.Helper()
	h := sha256.New()
	n, err := io.Copy(h, r)
	if err != nil {
		t.Fatalf("hashing: %v", err)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n
}
