// Whitebox: these tests register throwaway stub mutators to exercise the
// shared package-level registry (Register/New/List/All), then must remove
// those entries again so they don't leak into other tests sharing this test
// binary's process. There is no exported way to unregister a mutator — by
// design, Register's once-only contract mirrors database/sql.Register — so
// cleanup needs direct access to the unexported registry map and its mutex.
// That's the only reason this file lives in package mutator rather than
// mutator_test.
package mutator

import (
	"go/ast"
	"reflect"
	"testing"
)

// stub is a minimal Mutator used to exercise the registry.
type stub struct {
	name string
}

func (s stub) Name() string { return s.name }

func (s stub) Applies(node ast.Node) bool {
	_, ok := node.(*ast.Ident)

	return ok
}

func (s stub) Mutate(node ast.Node) []Mutation {
	ident, ok := node.(*ast.Ident)
	if !ok {
		return nil
	}

	original := ident.Name

	return []Mutation{{
		Description: original + " -> mutated",
		Apply:       func() { ident.Name = "mutated" },
		Revert:      func() { ident.Name = original },
	}}
}

// registerStubs registers the given names for the duration of the test.
//
// It mutates the shared package-level registry, so neither the caller nor
// any sibling subtest that also touches the registry may run in parallel
// with it — the same class of exception as t.Setenv.
func registerStubs(t *testing.T, names ...string) {
	t.Helper()

	for _, name := range names {
		Register(name, func() Mutator { return stub{name: name} })
	}

	t.Cleanup(func() {
		registryMu.Lock()
		defer registryMu.Unlock()

		for _, name := range names {
			delete(registry, name)
		}
	})
}

// TestRegister exercises Register's panic contract: an empty name, a nil
// constructor, and a duplicate name are all programmer errors that panic
// rather than returning an error.
//
// The outer test is not marked parallel because one of its subtests mutates
// the shared package-level registry; see that subtest for why it, in turn,
// isn't parallel either.
func TestRegister(t *testing.T) {
	tests := []struct {
		name            string
		touchesRegistry bool
		run             func(t *testing.T)
	}{
		{
			name: "empty name panics",
			run: func(_ *testing.T) {
				Register("", func() Mutator { return stub{} })
			},
		},
		{
			name: "nil constructor panics",
			run: func(_ *testing.T) {
				Register("test/nil", nil)
			},
		},
		{
			name:            "duplicate name panics",
			touchesRegistry: true,
			run: func(t *testing.T) {
				registerStubs(t, "test/dup")
				Register("test/dup", func() Mutator { return stub{name: "test/dup"} })
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Both panic before ever touching the registry, so they're safe
			// to run alongside each other; the duplicate case registers
			// (and cleans up) a real entry, so it stays serial.
			if !tt.touchesRegistry {
				t.Parallel()
			}

			defer func() {
				if recover() == nil {
					t.Error("Register() did not panic")
				}
			}()

			tt.run(t)
		})
	}
}

// TestNew exercises both branches of New: a registered name resolves to a
// constructed Mutator, and an unregistered name returns an error instead of
// panicking (names reach New from user input, per mutator.go's doc comment).
//
// The outer test is not marked parallel because its first subtest mutates
// the shared package-level registry.
func TestNew(t *testing.T) {
	t.Run("returns the registered mutator", func(t *testing.T) {
		// Not parallel: registers into the shared package-level registry.
		registerStubs(t, "test/alpha")

		m, err := New("test/alpha")
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if m.Name() != "test/alpha" {
			t.Errorf("Name() = %q, want %q", m.Name(), "test/alpha")
		}
	})

	t.Run("unknown name returns an error", func(t *testing.T) {
		t.Parallel()

		if _, err := New("test/nope-unregistered"); err == nil {
			t.Fatal("New() error = nil, want an error for an unregistered name")
		}
	})
}

// TestList exercises List: it returns the names of every registered
// mutator, sorted, regardless of registration order.
//
// Not parallel: registers into, and asserts the exact contents of, the
// shared package-level registry — a sibling parallel test's own
// registrations would corrupt the expected result.
func TestList(t *testing.T) {
	registerStubs(t, "test/zulu", "test/list-alpha", "test/mike")

	want := []string{"test/list-alpha", "test/mike", "test/zulu"}

	if got := List(); !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}
}

// TestAll exercises All: it constructs one instance of every registered
// mutator, in the same sorted order List reports.
//
// Not parallel: registers into, and asserts the exact contents of, the
// shared package-level registry — a sibling parallel test's own
// registrations would corrupt the expected result.
func TestAll(t *testing.T) {
	registerStubs(t, "test/zulu", "test/all-alpha", "test/mike")

	want := []string{"test/all-alpha", "test/mike", "test/zulu"}

	all := All()

	got := make([]string, len(all))
	for i, m := range all {
		got[i] = m.Name()
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("All() names = %v, want %v", got, want)
	}
}

// TestMutationApplyRevertRoundTrips locks in the interface contract phase 2
// implements against: Apply mutates the live AST, Revert restores it exactly.
//
// Parallel: touches only a local *ast.Ident, never the shared registry.
func TestMutationApplyRevertRoundTrips(t *testing.T) {
	t.Parallel()

	ident := ast.NewIdent("original")

	m := stub{name: "test/roundtrip"}
	if !m.Applies(ident) {
		t.Fatal("Applies() = false for a node the mutator handles")
	}

	mutations := m.Mutate(ident)
	if len(mutations) != 1 {
		t.Fatalf("Mutate() returned %d mutations, want 1", len(mutations))
	}
	if ident.Name != "original" {
		t.Errorf("Mutate() modified the AST before Apply was called: %q", ident.Name)
	}

	mutations[0].Apply()

	if ident.Name != "mutated" {
		t.Errorf("after Apply, name = %q, want %q", ident.Name, "mutated")
	}

	mutations[0].Revert()

	if ident.Name != "original" {
		t.Errorf("after Revert, name = %q, want %q", ident.Name, "original")
	}
}
