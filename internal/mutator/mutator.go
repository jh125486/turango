// Package mutator defines the contract every mutation operator implements and
// the name-keyed plugin registry the mutation engine discovers operators
// through.
//
// # Interface shape
//
// The original go-turango used a channel-based generator: a mutator returned a
// channel of "revert" funcs and the engine advanced it one mutation at a time.
// That shape leaks a goroutine whenever the engine abandons a walk early
// (timeout, SIGINT, a NotViable mutant short-circuit), and it makes a mutator
// impossible to unit-test without draining the channel. This package keeps the
// same conceptual model — a mutator yields a sequence of individually
// applicable, individually reversible edits to a live AST — but materialises it
// as a slice:
//
//	Applies(node) bool          // cheap type/shape check, no allocation
//	Mutate(node) []Mutation     // the edits this mutator would make to node
//
// Each [Mutation] carries an Apply/Revert pair that closes over the node it
// edits. The engine's contract is strictly nested: apply one mutation, print
// and test the file, revert it, then move to the next. Mutations are not
// required to compose with each other, and a Revert must restore the exact
// prior value (not merely a semantically equivalent one) so the AST can be
// reused for the whole walk instead of being reparsed per mutant.
//
// Mutators are registered from package init functions, so an operator package
// is enabled purely by being imported.
package mutator

import (
	"fmt"
	"go/ast"
	"sort"
	"sync"
)

// Mutation is a single reversible edit to a node in a live AST.
//
// Apply performs the edit; Revert undoes it, restoring the node to exactly the
// state it had before Apply was called. Both must be safe to call once, in that
// order. Applying two Mutations concurrently against the same file is not
// supported — the engine serialises them per AST.
type Mutation struct {
	// Description is a short human-readable summary of the edit, used in
	// reports to show a user what was changed, e.g. "== -> !=" or
	// "remove if body".
	Description string

	// Apply performs the mutation on the AST.
	Apply func()

	// Revert undoes the mutation, restoring the original AST.
	Revert func()
}

// Mutator produces mutations for the AST nodes it recognises.
//
// Implementations must be stateless with respect to the AST: all per-node state
// belongs in the closures held by the returned [Mutation] values, so a single
// Mutator instance can be walked over many files.
type Mutator interface {
	// Name reports the operator's registry name. It must equal the name the
	// constructor was registered under, since [All] returns Mutators without
	// their keys and reports identify operators by this value.
	Name() string

	// Applies reports whether this mutator can produce any mutation for node.
	// It is a cheap pre-filter run for every node of every walk and must not
	// modify the AST or allocate on the common (non-matching) path.
	Applies(node ast.Node) bool

	// Mutate returns the mutations this mutator would make to node, or nil if
	// there are none. Calling Mutate does not modify the AST; only the returned
	// [Mutation.Apply] funcs do. Mutate must return nil whenever Applies
	// reports false.
	Mutate(node ast.Node) []Mutation
}

var (
	registryMu sync.RWMutex
	registry   = map[string]func() Mutator{}
)

// Register makes a mutator constructor available under name.
//
// It is intended to be called from an operator package's init function.
// Register panics if name is empty, if constructor is nil, or if name is
// already registered — all three are programmer errors detectable at process
// start, following the precedent of database/sql.Register and http.Handle.
func Register(name string, constructor func() Mutator) {
	if name == "" {
		panic("mutator: Register called with an empty name")
	}
	if constructor == nil {
		panic("mutator: Register called with a nil constructor for " + name)
	}

	registryMu.Lock()
	defer registryMu.Unlock()

	if _, dup := registry[name]; dup {
		panic("mutator: Register called twice for " + name)
	}
	registry[name] = constructor
}

// New constructs the mutator registered under name.
//
// It returns an error, rather than panicking, because names reach it from user
// input (-mutateoperators).
func New(name string) (Mutator, error) {
	registryMu.RLock()
	constructor, ok := registry[name]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("mutator: no mutator registered as %q (registered: %v)", name, List())
	}

	return constructor(), nil
}

// List returns the names of every registered mutator, sorted.
func List() []string {
	registryMu.RLock()
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	registryMu.RUnlock()

	sort.Strings(names)

	return names
}

// All constructs one instance of every registered mutator, ordered by name so
// that a mutation run is reproducible regardless of import or map order.
func All() []Mutator {
	names := List()

	mutators := make([]Mutator, 0, len(names))
	for _, name := range names {
		registryMu.RLock()
		constructor := registry[name]
		registryMu.RUnlock()

		mutators = append(mutators, constructor())
	}

	return mutators
}
