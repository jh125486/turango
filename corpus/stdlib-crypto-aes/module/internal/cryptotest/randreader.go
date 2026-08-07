// newRandReader is a shared test helper originally defined in
// crypto/internal/cryptotest/hash.go (deleted from this isolated fixture
// since the rest of that file needed internal/testhash, which is unrelated
// to AES block-cipher testing). Kept standalone since block.go/stream.go/
// blockmode.go all depend on it.
package cryptotest

import (
	"io"
	"math/rand"
	"testing"
	"time"
)

func newRandReader(t *testing.T) io.Reader {
	seed := time.Now().UnixNano()
	t.Logf("Deterministic RNG seed: 0x%x", seed)
	return rand.New(rand.NewSource(seed))
}
