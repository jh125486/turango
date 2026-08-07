// Minimal stand-in for crypto/internal/boring's non-cgo build (the default,
// "not compiled with GOEXPERIMENT=boringcrypto" case), for this isolated
// mutation-testing fixture. Only the two symbols crypto/aes actually uses
// are implemented; BoringCrypto is never enabled here, so NewAESCipher is
// never actually called.
package boring

import "crypto/cipher"

const Enabled = false

func NewAESCipher(key []byte) (cipher.Block, error) {
	panic("boring: not enabled in this fixture")
}
