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

func TestRegisterAndNew(t *testing.T) {
	registerStubs(t, "test/alpha")

	m, err := New("test/alpha")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if m.Name() != "test/alpha" {
		t.Errorf("Name() = %q, want %q", m.Name(), "test/alpha")
	}
}

func TestNewUnknown(t *testing.T) {
	if _, err := New("test/nope"); err == nil {
		t.Fatal("New() error = nil, want an error for an unregistered name")
	}
}

func TestListAndAllAreSortedByName(t *testing.T) {
	registerStubs(t, "test/zulu", "test/alpha", "test/mike")

	want := []string{"test/alpha", "test/mike", "test/zulu"}

	if got := List(); !reflect.DeepEqual(got, want) {
		t.Errorf("List() = %v, want %v", got, want)
	}

	all := All()

	got := make([]string, len(all))
	for i, m := range all {
		got[i] = m.Name()
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("All() names = %v, want %v", got, want)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	registerStubs(t, "test/dup")

	defer func() {
		if recover() == nil {
			t.Error("Register() with a duplicate name did not panic")
		}
	}()

	Register("test/dup", func() Mutator { return stub{name: "test/dup"} })
}

func TestRegisterNilConstructorPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register() with a nil constructor did not panic")
		}
	}()

	Register("test/nil", nil)
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register() with an empty name did not panic")
		}
	}()

	Register("", func() Mutator { return stub{} })
}

// TestMutationApplyRevertRoundTrips locks in the interface contract phase 2
// implements against: Apply mutates the live AST, Revert restores it exactly.
func TestMutationApplyRevertRoundTrips(t *testing.T) {
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
