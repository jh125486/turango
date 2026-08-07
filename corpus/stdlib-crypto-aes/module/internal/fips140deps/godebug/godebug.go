// Minimal stand-in for internal/fips140deps/godebug, avoiding a dependency
// on the stdlib-internal internal/godebug package for this isolated
// mutation-testing fixture. Behavior (always report unset) is irrelevant to
// AES's cipher logic; only compilability matters here.
package godebug

type Setting struct {
	name string
}

func New(name string) *Setting {
	return &Setting{name: name}
}

func (s *Setting) Value() string {
	return ""
}

func Value(name string) string {
	return ""
}
