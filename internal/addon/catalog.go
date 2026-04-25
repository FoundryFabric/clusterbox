package addon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/foundryfabric/clusterbox/addons"
)

// ErrNotFound is returned by Catalog.Get when the requested addon name is not
// present in the catalog. Callers can detect it with errors.Is.
var ErrNotFound = errors.New("addon: not found")

// Catalog is a read-only registry of addons compiled into the binary. It is
// safe for concurrent use; loading happens once on first access.
type Catalog struct {
	loadOnce sync.Once
	loadErr  error
	addons   map[string]*Addon
}

// DefaultCatalog returns a Catalog backed by the addons/ directory embedded
// into this binary at build time.
func DefaultCatalog() *Catalog {
	return &Catalog{}
}

// load is invoked exactly once; subsequent calls return the cached result.
func (c *Catalog) load() error {
	c.loadOnce.Do(func() {
		c.addons, c.loadErr = loadFromFS(addons.FS, addons.Root)
	})
	return c.loadErr
}

// List returns the names of all addons in the catalog, sorted lexicographically.
func (c *Catalog) List() ([]string, error) {
	if err := c.load(); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(c.addons))
	for name := range c.addons {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// Get returns the named Addon. If no addon with the given name exists, it
// returns an error wrapping ErrNotFound.
func (c *Catalog) Get(name string) (*Addon, error) {
	if err := c.load(); err != nil {
		return nil, err
	}
	a, ok := c.addons[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return a, nil
}

// loadFromFS walks the given filesystem rooted at root and returns a map of
// parsed addons keyed by addon name. It is exported (lowercase) to the package
// so tests can drive it from a synthetic fs.FS without depending on //go:embed.
func loadFromFS(fsys fs.FS, root string) (map[string]*Addon, error) {
	entries, err := fs.ReadDir(fsys, root)
	if err != nil {
		// An empty/missing root is not a hard error: the binary may simply
		// have been built with no addons. Surface real I/O failures though.
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]*Addon{}, nil
		}
		return nil, fmt.Errorf("addon: read root %q: %w", root, err)
	}

	addons := make(map[string]*Addon, len(entries))
	for _, ent := range entries {
		if !ent.IsDir() {
			// Top-level files inside addons/ are not part of any addon.
			continue
		}
		dirName := ent.Name()
		dirPath := path.Join(root, dirName)

		a, err := loadAddon(fsys, dirPath)
		if err != nil {
			return nil, fmt.Errorf("addon %q: %w", dirName, err)
		}

		// Defend against name mismatch between directory and addon.yaml: the
		// directory name is the canonical lookup key.
		if a.Name != dirName {
			return nil, fmt.Errorf("addon %q: addon.yaml name=%q does not match directory name", dirName, a.Name)
		}
		if _, dup := addons[a.Name]; dup {
			return nil, fmt.Errorf("addon %q: duplicate name", a.Name)
		}
		addons[a.Name] = a
	}
	return addons, nil
}

// loadAddon parses a single addon directory.
func loadAddon(fsys fs.FS, dir string) (*Addon, error) {
	manifestPath := path.Join(dir, "addon.yaml")
	raw, err := fs.ReadFile(fsys, manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", manifestPath, err)
	}

	var a Addon
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // surface typos in addon.yaml as errors with line numbers
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("parse %s: %w", manifestPath, err)
	}

	if a.Name == "" {
		return nil, fmt.Errorf("%s: name is required", manifestPath)
	}
	if a.Version == "" {
		return nil, fmt.Errorf("%s: version is required", manifestPath)
	}
	if a.Description == "" {
		return nil, fmt.Errorf("%s: description is required", manifestPath)
	}
	if !a.Strategy.Valid() {
		return nil, fmt.Errorf("%s: strategy must be one of %q or %q (got %q)",
			manifestPath, StrategyManifests, StrategyHelmChart, a.Strategy)
	}
	for i, s := range a.Secrets {
		if s.Key == "" {
			return nil, fmt.Errorf("%s: secrets[%d].key is required", manifestPath, i)
		}
	}

	manifests, err := loadManifests(fsys, dir)
	if err != nil {
		return nil, err
	}
	a.Manifests = manifests
	return &a, nil
}

// loadManifests reads every regular file under <dir>/manifests/ into memory,
// keyed by path relative to <dir>. The .gitkeep marker is intentionally
// preserved so callers can tell an empty manifests/ tree apart from a missing
// one; the installer (T3) skips it by name.
func loadManifests(fsys fs.FS, dir string) (map[string][]byte, error) {
	manifestsDir := path.Join(dir, "manifests")
	out := map[string][]byte{}

	err := fs.WalkDir(fsys, manifestsDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		rel, err := relPath(dir, p)
		if err != nil {
			return err
		}
		out[rel] = data
		return nil
	})
	if err != nil {
		// A missing manifests/ directory is a manifest-format violation: every
		// addon must ship a manifests/ tree (possibly with just .gitkeep).
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: manifests/ directory is required", dir)
		}
		return nil, err
	}
	return out, nil
}

// relPath returns p expressed relative to base, using forward slashes.
func relPath(base, p string) (string, error) {
	base = strings.TrimSuffix(base, "/")
	if !strings.HasPrefix(p, base+"/") {
		return "", fmt.Errorf("addon: path %q is not under %q", p, base)
	}
	return p[len(base)+1:], nil
}
