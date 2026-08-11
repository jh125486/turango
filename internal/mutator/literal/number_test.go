package literal_test

import (
	"go/ast"
	"testing"

	"github.com/jh125486/turango/internal/mutator/literal"
)

func TestNumberMutate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		src     string
		wantUp  string // rendered after applying the "+" mutation
		wantDwn string // rendered after applying the "-" mutation
	}{
		{name: "zero", src: "0", wantUp: "1", wantDwn: "-1"},
		{name: "positive decimal", src: "20", wantUp: "21", wantDwn: "19"},
		{name: "hex", src: "0x10", wantUp: "17", wantDwn: "15"},
		{name: "octal", src: "0o17", wantUp: "16", wantDwn: "14"},
		{name: "underscore separator", src: "1_000", wantUp: "1001", wantDwn: "999"},
		// Float cases: the shift is a relative 0.1% nudge, not a flat ±1
		// (see number.go's shiftFloat doc for why), so the expected values
		// below are the literal's own value ± 0.1% of itself, rendered by
		// go/constant's Value.String (confirmed empirically valid, minimal
		// Go float syntax during the gap-10 investigation — see ROADMAP.md).
		{name: "plain decimal", src: "0.95", wantUp: "0.95095", wantDwn: "0.94905"},
		{name: "exponent notation", src: "1.5e10", wantUp: "1.5015e+10", wantDwn: "1.4985e+10"},
		// Float zero is a special case: a relative shift of 0 is 0, so
		// shiftFloat falls back to using its shift magnitude as an
		// absolute delta instead of a relative one.
		{name: "float zero", src: "0.0", wantUp: "0.001", wantDwn: "-0.001"},
	}

	var m literal.NumberMutator

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

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

// TestNumberIgnoresOtherLiterals checks that string, char, and imaginary
// literals, which have their own distinct mutation concerns not handled
// here (or none at all), are left alone. Int and float are the only two
// BasicLit kinds NumberMutator applies to.
func TestNumberIgnoresOtherLiterals(t *testing.T) {
	t.Parallel()

	var m literal.NumberMutator

	for _, src := range []string{`"hello"`, "'a'", "3.14i"} {
		t.Run(src, func(t *testing.T) {
			t.Parallel()

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
	t.Parallel()

	_, file := parseFunc(t, "a := 1")

	var m literal.NumberMutator

	stmt := findNode[*ast.AssignStmt](t, file)

	if m.Applies(stmt) {
		t.Error("Applies() = true for an assignment statement")
	}

	if got := m.Mutate(stmt); got != nil {
		t.Errorf("Mutate() = %v, want nil", got)
	}
}
