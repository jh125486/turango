// Stand-in for internal/fips140deps/cpu, avoiding a dependency on the
// stdlib-internal internal/cpu package for this isolated mutation-testing
// fixture. All hardware-acceleration feature flags are forced false, so the
// AES package's pure-Go generic fallback path is what gets built and
// mutated (the assembly fast paths are out of turango's reach regardless of
// these flags -- they're a separate, un-mutatable .s file).
package cpu

const (
	BigEndian = false
	AMD64     = false
	ARM64     = false
	PPC64     = false
	PPC64le   = false
)

var (
	ARM64HasAES    = false
	ARM64HasPMULL  = false
	ARM64HasSHA2   = false
	ARM64HasSHA512 = false
	ARM64HasSHA3   = false

	LOONG64HasLSX  = false
	LOONG64HasLASX = false

	RISCV64HasV = false

	S390XHasAES    = false
	S390XHasAESCBC = false
	S390XHasAESCTR = false
	S390XHasAESGCM = false
	S390XHasECDSA  = false
	S390XHasGHASH  = false
	S390XHasSHA256 = false
	S390XHasSHA3   = false
	S390XHasSHA512 = false

	X86HasAES       = false
	X86HasADX       = false
	X86HasAVX       = false
	X86HasAVX2      = false
	X86HasBMI2      = false
	X86HasPCLMULQDQ = false
	X86HasSHA       = false
	X86HasSSE41     = false
	X86HasSSSE3     = false
)
