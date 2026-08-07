# turango

![turango mascot](turango.png)

Mutation testing for `go test`, added the same way fuzzing was: not a
separate tool bolted onto the ecosystem, but a `go`-compatible drop-in that
adds one new test mode.

`turango` is a transparent shim binary. `turango test -mutate=. ./...` runs
the mutation engine; every other invocation (`turango build ./...`,
`turango vet ./...`, plain `turango test ./...`, ...) is forwarded verbatim
to the real Go toolchain, unchanged.

Mutation testing measures test-suite *quality*, not code coverage. It
mechanically introduces small, reversible changes ("mutants") into a
package's AST — flip `+` to `-`, flip `<` to `<=`, delete a statement, empty
an `if` body — and reruns the test suite against each one. A mutant the
suite catches is "killed": proof the suite actually asserts on that
behavior. A mutant that survives is a gap — code that runs during tests but
that nothing checks.

See [`PROPOSAL.md`](PROPOSAL.md) for the full case for why this belongs in
`go test` itself, including evidence gathered by running turango against
real `crypto/...` stdlib packages.

## Install / build

```
go build ./cmd/turango
```

That produces a `turango` binary you run instead of `go`. There's no alias
step required for normal use — see the note on alias mode below.

## Usage

```
turango test -mutate=. ./...
```

`-mutate` works like `-run`/`-bench`/`-fuzz`: its value is a regular
expression matched against function/method names, not a package selector.
`-mutate=.` matches every function (mirroring `-bench=.`'s "run every
benchmark" convention); package selection is the ordinary trailing
argument(s), exactly as with those three flags.

Everything that isn't `test` with a `-mutate=` flag goes straight to the
real `go` command:

```
turango build ./...     # identical to `go build ./...`
turango test ./...      # identical to `go test ./...` — no mutation, -mutate not set
```

### Flags (mutation mode only)

All of turango's own flags require the `-flag=value` form (a bare
`-flag value` is rejected as ambiguous). They're only recognized following
`test`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-mutate=<regexp>` | — | Required to enter mutation mode. A regular expression matched against function/method names in the target packages — behaves like `-run`/`-bench`/`-fuzz`, not a package selector. `-mutate=.` matches every function (mirroring `-bench=.`). Package selection is the ordinary trailing argument(s) after `test`, exactly as with those three flags, e.g. `turango test -mutate=. ./...`. |
| `-mutatescope=full\|package\|impact` | `full` | How much of the test suite reruns per mutant. `full` reruns `go test ./...` for the whole module (catches mutants killed by a neighboring package's tests); `package` reruns only the mutated file's own package (much cheaper, recommended when mutating a package that lives inside a slow module — see `example/README.md`); `impact` builds a per-test coverage map once and reruns only the tests that actually cover the mutated line. |
| `-mutateoperators=<comma-list>` | all registered operators | Restrict which mutation operators run (see the operator list below for names). |
| `-mutateparallel=<n>` | `GOMAXPROCS` | Worker-pool size. Parallelizes at the file level, since one file's AST is mutated in place across its own mutants. |
| `-mutatetimeout=<duration>` | baseline-derived | Per-mutant budget. If unset, turango times the real suite three times, averages, and scales by CPU count, so a mutant that produces an infinite loop (e.g. `i++` flipped to `i--`) doesn't stall the run to `go test`'s own ~10-minute default. A timeout counts as a kill. |
| `-mutateoutput=<dir>` | none | Write a JSON report (`mutate-report.json`) into this directory. |
| `-mutatemin=<float>` | none | Exit with status 3 if the resulting mutation score falls below this threshold (0–1) — for CI gating. |

### Suppressing a mutant: `//nomutant`

A `//nomutant` (or `//nomutant: reason`) comment above a statement excludes
it from mutation. On a compound statement (`if`/`for`/`switch`) the
suppression cascades into its body. See `example/README.md` for a worked
example, including how it affects the reported score.

### Exit codes

`0` success, `1` failure/error, `2` bad usage, `3` mutation score below
`-mutatemin`.

## Mutation operators

Twelve operators, across five packages:

- **`control`** — `control/if`, `control/else`, `control/case`: remove a
  conditional body.
- **`expression`** — `expression/remove`: eliminate a `&&`/`||`
  short-circuit operand.
- **`statement`** — `statement/remover`: delete a statement.
- **`operator`** — `operator/assignment`, `operator/binary`,
  `operator/boundary` (`<`↔`<=`, `>`↔`>=` — the classic off-by-one mutant),
  `operator/inc_dec`, `operator/unary`: token-swap mutations.
- **`literal`** — `literal/number` (shifts an int/float literal by ±1),
  `literal/boolean` (`true`↔`false`).

Known gap (documented in `PROPOSAL.md`): none of these can currently
reproduce a wrong-constant or wrong-identifier substitution — that needs an
identifier/constant-swap operator built on `go/types`, which doesn't exist
yet.

## Try it

- [`example/`](example/) — ordinary, deliberately-imperfect order-pricing
  code with an ordinary test suite. `example/README.md` has a runnable
  command and real, unedited turango output, including what surviving
  mutants point to and what `//nomutant` costs the score.
- [`example/legacy/`](example/legacy/) — the original `go-turango`
  prototype's demo package, ported unchanged: one coarse assertion,
  38 mutants, 12 survivors — a clean illustration of what a single
  overall-result check misses.

## Alias mode (experimental, opt-in)

Renaming or symlinking `turango` to `go` and placing it ahead of the real
toolchain in `PATH` routes *every* tool on the machine that shells out to
`go` (editors, `gopls`, CI, `go generate`, ...) through turango's
passthrough. That's a large blast radius for one unhandled verb or flag, so
it's gated behind `TURANGO_EXPERIMENTAL_ALIAS=1` and not advertised as a
default way to use turango. Use `turango test -mutate=. ./...` directly
instead.

## More

- [`PROPOSAL.md`](PROPOSAL.md) — the case for adding `-mutate` to `go test`
  itself, modeled on the real `-fuzz` design draft, with crypto-package
  evidence and a historical-bug validation case study.
- [`PROGRESS.md`](PROGRESS.md) — build and development history for this
  project.

## License

MIT — see [`LICENSE`](LICENSE).
