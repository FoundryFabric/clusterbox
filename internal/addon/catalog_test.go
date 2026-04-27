package addon

import (
	"errors"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// TestDefaultCatalog_ListIncludesStub verifies that the embedded catalog can be
// loaded and contains the gha-runner-scale-set stub introduced alongside this
// package.
func TestDefaultCatalog_ListIncludesStub(t *testing.T) {
	c := DefaultCatalog()

	names, err := c.List()
	if err != nil {
		t.Fatalf("List: unexpected error: %v", err)
	}

	if !contains(names, "gha-runner-scale-set") {
		t.Fatalf("expected catalog to include gha-runner-scale-set, got %v", names)
	}
}

// TestDefaultCatalog_GetStub verifies that the embedded gha-runner-scale-set
// addon parses cleanly and exposes the expected fields.
func TestDefaultCatalog_GetStub(t *testing.T) {
	c := DefaultCatalog()

	a, err := c.Get("gha-runner-scale-set")
	if err != nil {
		t.Fatalf("Get: unexpected error: %v", err)
	}

	if a.Name != "gha-runner-scale-set" {
		t.Errorf("Name: got %q, want %q", a.Name, "gha-runner-scale-set")
	}
	if a.Version == "" {
		t.Error("Version: must not be empty")
	}
	if a.Description == "" {
		t.Error("Description: must not be empty")
	}
	if a.Strategy != StrategyHelmChart {
		t.Errorf("Strategy: got %q, want %q", a.Strategy, StrategyHelmChart)
	}
	if len(a.Secrets) != 4 {
		t.Errorf("Secrets: got %d, want 4", len(a.Secrets))
	}
	// The addon ships the controller chart, a namespace, and a credentials
	// Secret manifest, plus the HelmChart CRD; verify the loader picked them
	// all up under their addon-relative paths.
	for _, want := range []string{
		"manifests/namespace.yaml",
		"manifests/secret.yaml",
		"manifests/helmchart.yaml",
	} {
		if _, ok := a.Manifests[want]; !ok {
			t.Errorf("Manifests: missing %q (got keys %v)", want, manifestKeys(a.Manifests))
		}
	}
}

// TestDefaultCatalog_GetUnknownReturnsTypedError verifies that a missing addon
// produces an error wrapping ErrNotFound so callers can branch on it.
func TestDefaultCatalog_GetUnknownReturnsTypedError(t *testing.T) {
	c := DefaultCatalog()

	_, err := c.Get("does-not-exist")
	if err == nil {
		t.Fatal("expected error for unknown addon, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected error to wrap ErrNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("expected error to mention addon name, got %q", err.Error())
	}
}

// TestLoadFromFS_HappyPath drives the loader against a synthetic fs.FS so the
// test does not depend on the embedded layout. It also confirms the manifests
// map is keyed by addon-relative path.
func TestLoadFromFS_HappyPath(t *testing.T) {
	fsys := fstest.MapFS{
		"root/foo/addon.yaml": &fstest.MapFile{Data: []byte(`
name: foo
version: 1.2.3
description: foo addon
strategy: manifests
secrets:
  - key: API_TOKEN
    description: token
    required: true
`)},
		"root/foo/manifests/deployment.yaml": &fstest.MapFile{Data: []byte("kind: Deployment\n")},
		"root/foo/manifests/service.yaml":    &fstest.MapFile{Data: []byte("kind: Service\n")},
	}

	addons, err := loadFromFS(fsys, "root")
	if err != nil {
		t.Fatalf("loadFromFS: %v", err)
	}
	a, ok := addons["foo"]
	if !ok {
		t.Fatalf("foo addon missing from %v", addons)
	}
	if a.Strategy != StrategyManifests {
		t.Errorf("Strategy: got %q, want %q", a.Strategy, StrategyManifests)
	}
	if got := string(a.Manifests["manifests/deployment.yaml"]); got != "kind: Deployment\n" {
		t.Errorf("manifests/deployment.yaml: got %q", got)
	}
	if got := string(a.Manifests["manifests/service.yaml"]); got != "kind: Service\n" {
		t.Errorf("manifests/service.yaml: got %q", got)
	}
}

// TestLoadFromFS_MalformedYAML verifies that a syntactically broken addon.yaml
// surfaces a parse error that includes the file name (line numbers come from
// yaml.v3 itself).
func TestLoadFromFS_MalformedYAML(t *testing.T) {
	fsys := fstest.MapFS{
		"root/bad/addon.yaml":         &fstest.MapFile{Data: []byte("name: bad\nversion: : :\n")},
		"root/bad/manifests/.gitkeep": &fstest.MapFile{Data: []byte("")},
	}

	_, err := loadFromFS(fsys, "root")
	if err == nil {
		t.Fatal("expected parse error for malformed addon.yaml")
	}
	if !strings.Contains(err.Error(), "addon.yaml") {
		t.Errorf("error should mention addon.yaml, got %q", err.Error())
	}
}

// TestLoadFromFS_UnknownField verifies that yaml.v3 KnownFields(true) is in
// effect: typos in addon.yaml fail loudly rather than being silently ignored.
func TestLoadFromFS_UnknownField(t *testing.T) {
	fsys := fstest.MapFS{
		"root/typo/addon.yaml": &fstest.MapFile{Data: []byte(`
name: typo
version: 0.0.1
description: typo addon
strategy: manifests
descriptionn: oops
`)},
		"root/typo/manifests/.gitkeep": &fstest.MapFile{Data: []byte("")},
	}

	_, err := loadFromFS(fsys, "root")
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	if !strings.Contains(err.Error(), "descriptionn") {
		t.Errorf("error should mention unknown field name, got %q", err.Error())
	}
}

// TestLoadFromFS_MissingRequiredField verifies that addon.yaml without a name
// is rejected with a descriptive error.
func TestLoadFromFS_MissingRequiredField(t *testing.T) {
	fsys := fstest.MapFS{
		"root/nameless/addon.yaml": &fstest.MapFile{Data: []byte(`
version: 0.0.1
description: nameless
strategy: manifests
`)},
		"root/nameless/manifests/.gitkeep": &fstest.MapFile{Data: []byte("")},
	}

	_, err := loadFromFS(fsys, "root")
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error should mention missing field, got %q", err.Error())
	}
}

// TestLoadFromFS_InvalidStrategy verifies the strategy enum is enforced.
func TestLoadFromFS_InvalidStrategy(t *testing.T) {
	fsys := fstest.MapFS{
		"root/badstrat/addon.yaml": &fstest.MapFile{Data: []byte(`
name: badstrat
version: 0.0.1
description: bad strategy
strategy: kustomize
`)},
		"root/badstrat/manifests/.gitkeep": &fstest.MapFile{Data: []byte("")},
	}

	_, err := loadFromFS(fsys, "root")
	if err == nil {
		t.Fatal("expected error for invalid strategy")
	}
	if !strings.Contains(err.Error(), "strategy") {
		t.Errorf("error should mention strategy, got %q", err.Error())
	}
}

// TestLoadFromFS_DirNameMismatch verifies that the directory name is the
// canonical lookup key and a mismatched addon.yaml.name is rejected.
func TestLoadFromFS_DirNameMismatch(t *testing.T) {
	fsys := fstest.MapFS{
		"root/alpha/addon.yaml": &fstest.MapFile{Data: []byte(`
name: beta
version: 0.0.1
description: name mismatch
strategy: manifests
`)},
		"root/alpha/manifests/.gitkeep": &fstest.MapFile{Data: []byte("")},
	}

	_, err := loadFromFS(fsys, "root")
	if err == nil {
		t.Fatal("expected error for directory/name mismatch")
	}
	if !strings.Contains(err.Error(), "alpha") {
		t.Errorf("error should mention directory name, got %q", err.Error())
	}
}

// TestLoadFromFS_MissingManifestsDir verifies that an addon without a
// manifests/ tree is rejected.
func TestLoadFromFS_MissingManifestsDir(t *testing.T) {
	fsys := fstest.MapFS{
		"root/lonely/addon.yaml": &fstest.MapFile{Data: []byte(`
name: lonely
version: 0.0.1
description: no manifests dir
strategy: manifests
`)},
	}

	_, err := loadFromFS(fsys, "root")
	if err == nil {
		t.Fatal("expected error for missing manifests/ directory")
	}
	if !strings.Contains(err.Error(), "manifests") {
		t.Errorf("error should mention manifests, got %q", err.Error())
	}
}

// TestStrategy_Valid confirms the typed enum's reported validity.
func TestStrategy_Valid(t *testing.T) {
	for _, tc := range []struct {
		s    Strategy
		want bool
	}{
		{StrategyManifests, true},
		{StrategyHelmChart, true},
		{StrategyStaged, true},
		{Strategy(""), false},
		{Strategy("kustomize"), false},
	} {
		if got := tc.s.Valid(); got != tc.want {
			t.Errorf("%q.Valid(): got %v, want %v", tc.s, got, tc.want)
		}
	}
}

// manifestKeys returns the sorted key list of a manifests map for use in
// test failure messages.
func manifestKeys(m map[string][]byte) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
