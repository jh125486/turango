// Whitebox: parseGolden and entryName are unexported, so exercising them
// directly requires package access. Discover's own behaviour (subdirectory
// walk, sibling-golden disambiguation, sort order, missing-corpusDir
// handling) is covered from the outside in corpus_test.go, since Discover
// itself is exported.
package corpus

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseGolden(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		build   func(t *testing.T) (path string, want Entry)
		wantErr bool
		errIs   error
	}{
		{
			name: "valid golden decodes every field and sets Path to its own file",
			build: func(t *testing.T) (string, Entry) {
				t.Helper()

				dir := t.TempDir()
				path := filepath.Join(dir, "golden.json")
				const data = `{
					"description": "sample fixture",
					"modulePath": "corpus/sample/module",
					"target": "./example/...",
					"scope": "package",
					"operators": ["arithmetic", "conditional"],
					"timeout": "30s",
					"tce": true,
					"expect": {
						"mutants": 4,
						"killed": 3,
						"survived": 1,
						"notViable": 0,
						"suppressed": 1,
						"equivalent": 2
					}
				}`

				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}

				suppressed, equivalent := 1, 2

				want := Entry{
					Path:        path,
					Description: "sample fixture",
					ModulePath:  "corpus/sample/module",
					Target:      "./example/...",
					Scope:       "package",
					Operators:   []string{"arithmetic", "conditional"},
					Timeout:     "30s",
					TCE:         true,
					Expect: Expect{
						Mutants:    4,
						Killed:     3,
						Survived:   1,
						NotViable:  0,
						Suppressed: &suppressed,
						Equivalent: &equivalent,
					},
				}

				return path, want
			},
		},
		{
			name: "malformed JSON wraps the decode error rather than panicking",
			build: func(t *testing.T) (string, Entry) {
				t.Helper()

				dir := t.TempDir()
				path := filepath.Join(dir, "golden.json")

				if err := os.WriteFile(path, []byte(`{"description": not valid`), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}

				return path, Entry{}
			},
			wantErr: true,
		},
		{
			name: "nonexistent path wraps the read error rather than panicking",
			build: func(t *testing.T) (string, Entry) {
				t.Helper()

				return filepath.Join(t.TempDir(), "does-not-exist.json"), Entry{}
			},
			wantErr: true,
			errIs:   os.ErrNotExist,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path, want := tt.build(t)

			got, err := parseGolden(path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseGolden(%s) error = nil, want error", path)
				}

				if tt.errIs != nil && !errors.Is(err, tt.errIs) {
					t.Errorf("parseGolden(%s) error = %v, want errors.Is(%v)", path, err, tt.errIs)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseGolden(%s) error = %v", path, err)
			}

			if !reflect.DeepEqual(got, want) {
				t.Errorf("parseGolden(%s) = %+v, want %+v", path, got, want)
			}
		})
	}
}

func TestEntryName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dirName    string
		goldenPath string
		want       string
	}{
		{
			name:       "lone golden.json gets a plain suffix",
			dirName:    "stdlib-x509-pkix",
			goldenPath: "/corpus/stdlib-x509-pkix/golden.json",
			want:       "stdlib-x509-pkix/golden",
		},
		{
			name:       "a sibling golden file disambiguates by its own base name",
			dirName:    "stdlib-crypto-aes",
			goldenPath: "/corpus/stdlib-crypto-aes/golden-full.json",
			want:       "stdlib-crypto-aes/golden-full",
		},
		{
			name:       "only the golden file's own base name is used, not its directory depth",
			dirName:    "op-tce-equivalent",
			goldenPath: "/a/b/c/op-tce-equivalent/golden.json",
			want:       "op-tce-equivalent/golden",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := entryName(tt.dirName, tt.goldenPath); got != tt.want {
				t.Errorf("entryName(%q, %q) = %q, want %q", tt.dirName, tt.goldenPath, got, tt.want)
			}
		})
	}
}
