// Whitebox (package main, not main_test): package main has no exported API to
// import and exercise from outside — run, parseMutateFlags, checkAlias, gate
// and friends are all unexported, so there is no blackbox path to them at all.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jh125486/turango/internal/mutate"
)

// TestParseMutateFlagsRecognition covers the one question asked of every
// `turango test` invocation: is this a mutation request, or the real go
// command's business?
func TestParseMutateFlagsRecognition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"none", []string{"./..."}, false},
		{"equals form", []string{"-mutate=."}, true},
		{"double dash equals form", []string{"--mutate=."}, true},
		{"other mutate flag alone is not a request", []string{"-mutatescope=package"}, false},
		{"after -args is the test binary's", []string{"-v", "-args", "-mutate=."}, false},
		{"after --args is the test binary's", []string{"--args", "-mutate=."}, false},
		{"before -args still counts", []string{"-mutate=.", "-args", "-x"}, true},
		{"not a prefix match on another flag", []string{"-run=Test-mutate=x"}, false},
		{"plain test flags pass through", []string{"-v", "-race", "-run=TestX", "./..."}, false},
		{
			"stuff after -args is ignored even if it would otherwise error",
			[]string{"-mutate=.", "-args", "-v", "whatever", "-mutatescope"}, true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, found, err := parseMutateFlags(tt.args)
			if err != nil {
				t.Fatalf("parseMutateFlags(%q) error = %v", tt.args, err)
			}

			if found != tt.want {
				t.Errorf("parseMutateFlags(%q) recognised = %v, want %v", tt.args, found, tt.want)
			}
		})
	}
}

// TestParseMutateFlagsValues covers every flag's value, and the defaults that
// apply when it is absent.
func TestParseMutateFlagsValues(t *testing.T) {
	t.Parallel()

	t.Run("defaults", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := parseMutateFlags([]string{"-mutate=."})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		switch {
		case cfg.options.FuncPattern != ".":
			t.Errorf("FuncPattern = %q, want \".\"", cfg.options.FuncPattern)
		case len(cfg.options.Packages) != 0:
			// No trailing positional args were given, mirroring -run/-bench/
			// -fuzz's own default: an absent package pattern means "." (the
			// package in Dir), resolved later by Options.patterns(), not
			// injected here.
			t.Errorf("Packages = %v, want none", cfg.options.Packages)
		case cfg.options.Scope != mutate.ScopeFull:
			t.Errorf("Scope = %v, want %v", cfg.options.Scope, mutate.ScopeFull)
		case cfg.options.Parallel != runtime.GOMAXPROCS(0):
			t.Errorf("Parallel = %d, want GOMAXPROCS (%d)", cfg.options.Parallel, runtime.GOMAXPROCS(0))
		case cfg.options.Operators != nil:
			t.Errorf("Operators = %v, want nil (every operator)", cfg.options.Operators)
		case cfg.options.TestTimeout != 0:
			t.Errorf("TestTimeout = %v, want 0 (derive it)", cfg.options.TestTimeout)
		case cfg.output != "":
			t.Errorf("output = %q, want empty (no report)", cfg.output)
		case cfg.hasMin:
			t.Errorf("hasMin = true, want false (no gate)")
		case cfg.options.TCE:
			t.Errorf("TCE = true, want false")
		case cfg.options.Workspace != mutate.WorkspaceCopy:
			t.Errorf("Workspace = %v, want %v", cfg.options.Workspace, mutate.WorkspaceCopy)
		}
	})

	t.Run("every flag set", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := parseMutateFlags([]string{
			"-mutate=Foo.*",
			"-mutatescope=impact",
			"-mutateoperators=control/if,operator/binary",
			"-mutateparallel=3",
			"-mutatetimeout=90s",
			"-mutateoutput=/tmp/reports",
			"-mutatemin=0.75",
			"-mutatemutant=a1b2c3d4e5f6",
			"-mutatetce=true",
			"-mutateworkspace=worktree",
			"./internal/...",
		})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		switch {
		case cfg.options.FuncPattern != "Foo.*":
			t.Errorf("FuncPattern = %q, want \"Foo.*\"", cfg.options.FuncPattern)
		case !reflect.DeepEqual(cfg.options.Packages, []string{"./internal/..."}):
			t.Errorf("Packages = %v", cfg.options.Packages)
		case cfg.options.Scope != mutate.ScopeImpact:
			t.Errorf("Scope = %v, want impact", cfg.options.Scope)
		case !reflect.DeepEqual(cfg.options.Operators, []string{"control/if", "operator/binary"}):
			t.Errorf("Operators = %v", cfg.options.Operators)
		case cfg.options.Parallel != 3:
			t.Errorf("Parallel = %d, want 3", cfg.options.Parallel)
		case cfg.options.TestTimeout != 90*time.Second:
			t.Errorf("TestTimeout = %v, want 90s", cfg.options.TestTimeout)
		case cfg.output != "/tmp/reports":
			t.Errorf("output = %q", cfg.output)
		case !cfg.hasMin || cfg.min != 0.75:
			t.Errorf("min = %v, %v; want 0.75, true", cfg.min, cfg.hasMin)
		case cfg.options.MutantID != "a1b2c3d4e5f6":
			t.Errorf("MutantID = %q, want %q", cfg.options.MutantID, "a1b2c3d4e5f6")
		case !cfg.options.TCE:
			t.Errorf("TCE = false, want true")
		case cfg.options.Workspace != mutate.WorkspaceWorktree:
			t.Errorf("Workspace = %v, want %v", cfg.options.Workspace, mutate.WorkspaceWorktree)
		}
	})

	t.Run("mutatemutant defaults to empty", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := parseMutateFlags([]string{"-mutate=."})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		if cfg.options.MutantID != "" {
			t.Errorf("MutantID = %q, want empty", cfg.options.MutantID)
		}
	})

	t.Run("multiple positional package patterns", func(t *testing.T) {
		t.Parallel()

		// Package selection is ordinary trailing positional arguments now,
		// exactly as with -run/-bench/-fuzz — not a comma-separated -mutate
		// value. Each space-separated pattern is its own argument.
		cfg, _, err := parseMutateFlags([]string{"-mutate=.", "./a/...", "./b"})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		if want := []string{"./a/...", "./b"}; !reflect.DeepEqual(cfg.options.Packages, want) {
			t.Errorf("Packages = %v, want %v", cfg.options.Packages, want)
		}
	})

	t.Run("a zero threshold is still a threshold", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := parseMutateFlags([]string{"-mutate=.", "-mutatemin=0"})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		if !cfg.hasMin || cfg.min != 0 {
			t.Errorf("min = %v, %v; want 0, true", cfg.min, cfg.hasMin)
		}
	})

	t.Run("scope defaults survive an unrelated flag", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := parseMutateFlags([]string{"-mutateparallel=2", "-mutate=."})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		if cfg.options.Scope != mutate.ScopeFull || cfg.options.Parallel != 2 {
			t.Errorf("cfg = %+v, want full scope and 2 workers", cfg.options)
		}
	})

	t.Run("trailing package pattern after -mutate", func(t *testing.T) {
		t.Parallel()

		// A bare, dash-less argument after -mutate is a legitimate
		// positional package pattern, exactly like -run/-bench/-fuzz's own
		// trailing package args — this used to be rejected as "unsupported
		// argument" before -mutate stopped taking packages as its own value.
		cfg, _, err := parseMutateFlags([]string{"-mutate=.", "./cmd/..."})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		if want := []string{"./cmd/..."}; !reflect.DeepEqual(cfg.options.Packages, want) {
			t.Errorf("Packages = %v, want %v", cfg.options.Packages, want)
		}
	})

	t.Run("trailing package pattern after other mutate flags", func(t *testing.T) {
		t.Parallel()

		cfg, _, err := parseMutateFlags([]string{"-mutate=.", "-mutatemin=0.5", "extra"})
		if err != nil {
			t.Fatalf("parseMutateFlags() error = %v", err)
		}

		if want := []string{"extra"}; !reflect.DeepEqual(cfg.options.Packages, want) {
			t.Errorf("Packages = %v, want %v", cfg.options.Packages, want)
		}
	})
}

// TestParseMutateFlagsErrors covers everything turango refuses to guess at.
func TestParseMutateFlagsErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		args []string
		want string
	}{
		"bare -mutate":          {args: []string{"-mutate", "."}, want: "= form"},
		"bare -mutatescope":     {args: []string{"-mutate=.", "-mutatescope"}, want: "= form"},
		"bare -mutateparallel":  {args: []string{"-mutateparallel"}, want: "= form"},
		"invalid regexp":        {args: []string{"-mutate=(unclosed"}, want: "mutate"},
		"unknown mutate flag":   {args: []string{"-mutate=.", "-mutatescopes=full"}, want: "unknown flag"},
		"unknown scope":         {args: []string{"-mutate=.", "-mutatescope=module"}, want: "unknown scope"},
		"non-numeric parallel":  {args: []string{"-mutate=.", "-mutateparallel=lots"}, want: "positive integer"},
		"zero parallel":         {args: []string{"-mutate=.", "-mutateparallel=0"}, want: "positive integer"},
		"bad duration":          {args: []string{"-mutate=.", "-mutatetimeout=30"}, want: "mutatetimeout"},
		"negative duration":     {args: []string{"-mutate=.", "-mutatetimeout=-5s"}, want: "positive duration"},
		"empty output":          {args: []string{"-mutate=.", "-mutateoutput="}, want: "requires a directory"},
		"non-numeric min":       {args: []string{"-mutate=.", "-mutatemin=high"}, want: "mutatemin"},
		"empty mutant id":       {args: []string{"-mutate=.", "-mutatemutant="}, want: "mutatemutant"},
		"uppercase mutant id":   {args: []string{"-mutate=.", "-mutatemutant=A1B2C3"}, want: "mutatemutant"},
		"non-hex mutant id":     {args: []string{"-mutate=.", "-mutatemutant=not-hex!"}, want: "mutatemutant"},
		"non-bool tce":          {args: []string{"-mutate=.", "-mutatetce=sometimes"}, want: "want true or false"},
		"unknown workspace":     {args: []string{"-mutate=.", "-mutateworkspace=network"}, want: "unknown workspace"},
		"leftover go test flag": {args: []string{"-mutate=.", "-v"}, want: "unsupported flag"},
		"leftover before the flag": {
			args: []string{"-count=1", "-mutate=."}, want: "unsupported flag",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cfg, _, err := parseMutateFlags(tt.args)
			if err == nil {
				t.Fatalf("parseMutateFlags(%q) = %+v, want an error", tt.args, cfg)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseMutateFlags(%q) error = %v, want it to mention %q", tt.args, err, tt.want)
			}
		})
	}
}

// TestRunRejectsBadFlags checks that a flag error is reported as a usage error
// rather than being forwarded to the real go command, which would report it as
// its own unknown flag and send the user to the wrong manual.
func TestRunRejectsBadFlags(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), []string{"turango", "test", "-mutate=.", "-v"}, &stdout, &stderr)
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}

	if !strings.Contains(stderr.String(), "unsupported flag") {
		t.Errorf("stderr = %q, want it to name the unsupported flag", stderr.String())
	}
}

// TestWriteSummary covers what the command adds to the report package's
// formatting: the counts, the score, the suppression ratio and the survivor
// list all reach stdout, with paths shown relative to the working directory.
//
// The formatting itself is [mutate.Result.WriteSummary]'s and is tested there.
func TestWriteSummary(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	var buf bytes.Buffer

	writeSummary(&buf, &mutate.Result{
		Mutants: []mutate.MutantResult{
			{Status: mutate.Killed},
			{Status: mutate.Killed},
			{Status: mutate.Killed},
			{
				File:        filepath.Join(cwd, "pricing.go"),
				Line:        12,
				Operator:    "control/if",
				Description: "remove if body",
				Status:      mutate.Survived,
			},
			{Status: mutate.NotViable},
		},
		Suppressions: []mutate.SuppressionResult{{Line: 3}},
	})

	for _, want := range []string{
		"mutants:    5",
		"killed:     3",
		"survived:   1",
		"not-viable: 1",
		"suppressed: 1 of 5 nodes",
		"score:      75.0%",
		"Surviving mutants (1):",
		"pricing.go:12  control/if  remove if body",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("summary = %q, want it to contain %q", buf.String(), want)
		}
	}

	// The survivor's path is relativised against the working directory, so the
	// absolute prefix must be gone entirely.
	if strings.Contains(buf.String(), cwd) {
		t.Errorf("summary = %q, want relative paths", buf.String())
	}

	buf.Reset()
	writeSummary(&buf, &mutate.Result{})

	if !strings.Contains(buf.String(), "n/a") {
		t.Errorf("summary = %q, want an unscored run to say so", buf.String())
	}
}

// TestWriteReport covers the JSON dump, including creating the directory, the
// status names it writes and the relativised paths that make the file portable.
func TestWriteReport(t *testing.T) {
	t.Parallel()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}

	dir := filepath.Join(t.TempDir(), "nested", "reports")

	want := &mutate.Result{
		Mutants: []mutate.MutantResult{{
			File:     filepath.Join(cwd, "a.go"),
			Line:     7,
			Operator: "control/if",
			Status:   mutate.Survived,
		}},
		Suppressions: []mutate.SuppressionResult{{File: filepath.Join(cwd, "a.go"), Line: 3}},
	}

	if err := writeReport(dir, want); err != nil {
		t.Fatalf("writeReport() error = %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, reportFile))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}

	// The status is a name on the wire, not the iota behind it: a consumer
	// reading this file must not have to know report.go's declaration order.
	if !strings.Contains(string(data), `"Status": "survived"`) {
		t.Errorf("report = %s, want the status spelled out", data)
	}

	var got mutate.Result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("the report is not valid JSON: %v", err)
	}

	if len(got.Mutants) != 1 || got.Mutants[0].File != "a.go" || got.Mutants[0].Line != 7 {
		t.Errorf("report = %+v, want the mutant it was given, relativised", got.Mutants)
	}

	if len(got.Suppressions) != 1 || got.Suppressions[0].File != "a.go" {
		t.Errorf("report suppressions = %+v, want them relativised too", got.Suppressions)
	}

	// Relativising is for the file only; the caller's Result still drives the
	// run in progress and must not have been rewritten under it.
	if want.Mutants[0].File == "a.go" {
		t.Error("writeReport rewrote the caller's Result")
	}
}

// TestGate covers -mutatemin, including the case that must not fail the build:
// a run with nothing to score.
func TestGate(t *testing.T) {
	t.Parallel()

	killed := &mutate.Result{Mutants: []mutate.MutantResult{
		{Status: mutate.Killed}, {Status: mutate.Killed}, {Status: mutate.Survived},
	}}

	tests := map[string]struct {
		cfg      *mutateConfig
		result   *mutate.Result
		wantCode int
		wantErr  string
	}{
		"no threshold": {
			cfg:      &mutateConfig{},
			result:   killed,
			wantCode: exitOK,
		},
		"above the threshold": {
			cfg:      &mutateConfig{min: 0.5, hasMin: true},
			result:   killed,
			wantCode: exitOK,
		},
		"exactly at the threshold": {
			cfg:      &mutateConfig{min: 2.0 / 3.0, hasMin: true},
			result:   killed,
			wantCode: exitOK,
		},
		"below the threshold": {
			cfg:      &mutateConfig{min: 0.9, hasMin: true},
			result:   killed,
			wantCode: exitBelowThreshold,
			wantErr:  "below",
		},
		"nothing to score": {
			cfg:      &mutateConfig{min: 0.9, hasMin: true},
			result:   &mutate.Result{},
			wantCode: exitOK,
			wantErr:  "no viable mutants",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer

			if got := gate(&stderr, tt.cfg, tt.result); got != tt.wantCode {
				t.Errorf("gate() = %d, want %d", got, tt.wantCode)
			}

			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to mention %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestCheckAlias(t *testing.T) {
	tests := []struct {
		name    string
		argv0   string
		optIn   string
		wantErr bool
	}{
		{"normal name", "/usr/local/bin/turango", "", false},
		{"normal name windows", "turango.exe", "", false},
		{"aliased to go without opt-in", "/usr/local/bin/go", "", true},
		{"aliased to go.exe without opt-in", "go.exe", "", true},
		{"aliased to GO uppercase", "GO.EXE", "", true},
		{"aliased to go with opt-in", "/usr/local/bin/go", "1", false},
		{"aliased to go with wrong opt-in value", "/usr/local/bin/go", "yes", true},
		{"name merely containing go", "/usr/local/bin/gopls", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(aliasEnvVar, tt.optIn)

			err := checkAlias(tt.argv0)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkAlias(%q) error = %v, wantErr %v", tt.argv0, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), aliasEnvVar) {
				t.Errorf("error message should name %s, got: %v", aliasEnvVar, err)
			}
		})
	}
}

func TestRunRefusesAliasWithoutOptIn(t *testing.T) {
	t.Setenv(aliasEnvVar, "")

	var stdout, stderr bytes.Buffer

	code := run(t.Context(), []string{"/usr/local/bin/go", "version"}, &stdout, &stderr)
	if code == 0 {
		t.Error("exit code = 0, want non-zero when aliased without opt-in")
	}
	if got := stderr.String(); !strings.Contains(got, "experimental") {
		t.Errorf("stderr = %q, want an explanation of the experimental mode", got)
	}
}
