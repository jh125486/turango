package literal

import (
	"go/ast"
	"testing"
)

func TestNumberMutate(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantUp  string // rendered after applying the +1 mutation
		wantDwn string // rendered after applying the -1 mutation
	}{
		{name: "zero", src: "0", wantUp: "1", wantDwn: "-1"},
		{name: "positive decimal", src: "20", wantUp: "21", wantDwn: "19"},
		{name: "hex", src: "0x10", wantUp: "17", wantDwn: "15"},
		{name: "octal", src: "0o17", wantUp: "16", wantDwn: "14"},
		{name: "underscore separator", src: "1_000", wantUp: "1001", wantDwn: "999"},
	}

	var m NumberMutator

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset, file := parseFunc(t, "_ = "+tt.src)
			lit := findNode[*ast.BasicLit](t, file)

			if !m.Applies(lit) {
				t.Fatal("Applies() = false, want true")
			}

			before := render(t, fset, lit)

			mutations := m.Mutate(lit)

			if got := render(t, fset, lit); got != before {
				t.Fatalf("Mutate modified the AST: got %q, want %q", got, before)
			}

			if len(mutations) != 2 {
				t.Fatalf("Mutate() returned %d mutations, want 2", len(mutations))
			}

			applyRoundTrip(t, fset, lit, mutations[0], tt.wantUp)
			applyRoundTrip(t, fset, lit, mutations[1], tt.wantDwn)
		})
	}
}

// TestNumberIgnoresOtherLiterals checks that string and float literals,
// which have their own distinct mutation concerns not handled here, are
// left alone.
func TestNumberIgnoresOtherLiterals(t *testing.T) {
	var m NumberMutator

	for _, src := range []string{`"hello"`, "3.14"} {
		t.Run(src, func(t *testing.T) {
			_, file := parseFunc(t, "_ = "+src)
			lit := findNode[*ast.BasicLit](t, file)

			if m.Applies(lit) {
				t.Errorf("Applies(%q) = true, want false", src)
			}

			if got := m.Mutate(lit); got != nil {
				t.Errorf("Mutate(%q) = %v, want nil", src, got)
			}
		})
	}
}

func TestNumberIgnoresOtherNodes(t *testing.T) {
	_, file := parseFunc(t, "a := 1")

	var m NumberMutator

	stmt := findNode[*ast.AssignStmt](t, file)

	if m.Applies(stmt) {
		t.Error("Applies() = true for an assignment statement")
	}

	if got := m.Mutate(stmt); got != nil {
		t.Errorf("Mutate() = %v, want nil", got)
	}
}
