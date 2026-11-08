// Package catalog loads OLM file-based catalogs (FBC) from a local directory or
// from a catalog container image.
package catalog

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// ConfigsLabel is the label catalog images use to point at their FBC directory.
const ConfigsLabel = "operators.operatorframework.io.index.configs.v1"

// Catalog is a loaded file-based catalog.
type Catalog struct {
	// Ref is the catalog reference as written in the ImageSetConfiguration.
	Ref string
	// Dir is the local directory the FBC was loaded from.
	Dir string
	// Digest is the image digest when loaded from an image ("" for local dirs).
	Digest string
	DC     *declcfg.DeclarativeConfig
}

// Loader resolves catalog references to loaded catalogs, with caching.
type Loader struct {
	// Overrides maps a catalog reference to a local FBC directory.
	Overrides map[string]string
	// CacheDir holds extracted catalog images.
	CacheDir string
	// Keychain provides registry credentials.
	Keychain authn.Keychain
	// Platform used when the catalog is a multi-arch image.
	Platform v1.Platform
	// Log receives progress messages; may be nil.
	Log func(format string, args ...any)

	loaded map[string]*Catalog
}

// NewLoader returns a Loader with sensible defaults.
func NewLoader() *Loader {
	cache, err := os.UserCacheDir()
	if err != nil {
		cache = os.TempDir()
	}
	return &Loader{
		Overrides: map[string]string{},
		CacheDir:  filepath.Join(cache, "olm-reconstruct", "catalogs"),
		Keychain:  authn.DefaultKeychain,
		Platform:  v1.Platform{OS: "linux", Architecture: "amd64"},
		loaded:    map[string]*Catalog{},
	}
}

func (l *Loader) logf(format string, args ...any) {
	if l.Log != nil {
		l.Log(format, args...)
	}
}

// Load returns the catalog for ref. Refs that are existing directories, or
// that have an override, are loaded from disk; anything else is pulled.
func (l *Loader) Load(ctx context.Context, ref string) (*Catalog, error) {
	if c, ok := l.loaded[ref]; ok {
		return c, nil
	}
	var (
		dir, digest string
		err         error
	)
	switch {
	case l.Overrides[ref] != "":
		dir = l.Overrides[ref]
	case isDir(ref):
		dir = ref
	case strings.HasPrefix(ref, "oci://") || strings.HasPrefix(ref, "file://"):
		return nil, fmt.Errorf("catalog %q: oci:// and file:// catalogs are not supported yet; extract /configs and pass it with --catalog-dir", ref)
	default:
		dir, digest, err = l.pull(ctx, strings.TrimPrefix(ref, "docker://"))
		if err != nil {
			return nil, fmt.Errorf("catalog %q: %w", ref, err)
		}
	}
	l.logf("loading catalog %s from %s", ref, dir)
	dc, err := LoadDir(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("catalog %q: %w", ref, err)
	}
	c := &Catalog{Ref: ref, Dir: dir, Digest: digest, DC: dc}
	l.loaded[ref] = c
	return c, nil
}

// LoadDir loads an FBC directory in any layout opm understands.
func LoadDir(ctx context.Context, dir string) (*declcfg.DeclarativeConfig, error) {
	if !isDir(dir) {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	return declcfg.LoadFS(ctx, os.DirFS(dir))
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// pull resolves the image for the configured platform, and extracts its FBC
// directory into the cache (once per digest).
func (l *Loader) pull(ctx context.Context, ref string) (string, string, error) {
	r, err := name.ParseReference(ref)
	if err != nil {
		return "", "", err
	}
	l.logf("resolving %s (%s/%s)", ref, l.Platform.OS, l.Platform.Architecture)
	img, err := remote.Image(r,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(l.Keychain),
		remote.WithPlatform(l.Platform),
	)
	if err != nil {
		return "", "", err
	}
	dgst, err := img.Digest()
	if err != nil {
		return "", "", err
	}
	dest := filepath.Join(l.CacheDir, strings.ReplaceAll(dgst.String(), ":", "-"))
	configs := filepath.Join(dest, "configs")
	if isDir(configs) {
		if _, err := os.Stat(filepath.Join(dest, ".complete")); err == nil {
			return configs, dgst.String(), nil
		}
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		return "", "", err
	}
	configsPath := "/configs"
	if v := cfg.Config.Labels[ConfigsLabel]; v != "" {
		configsPath = v
	}
	l.logf("extracting %s from %s@%s", configsPath, ref, dgst)
	if err := os.RemoveAll(dest); err != nil {
		return "", "", err
	}
	if err := extract(img, configsPath, configs); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(filepath.Join(dest, ".complete"), []byte(ref+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return configs, dgst.String(), nil
}

// extract copies the files under srcDir in the flattened image filesystem to destDir.
func extract(img v1.Image, srcDir, destDir string) error {
	prefix := strings.Trim(path.Clean(srcDir), "/") + "/"
	rc := mutate.Extract(img)
	defer rc.Close()
	tr := tar.NewReader(rc)
	found := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rel := strings.TrimPrefix(name, prefix)
		target := filepath.Join(destDir, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, filepath.Clean(destDir)+string(os.PathSeparator)) {
			return fmt.Errorf("refusing to extract %q outside %s", hdr.Name, destDir)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			found = true
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Symlinks and other entry types are not expected in FBC directories.
		}
	}
	if !found {
		return fmt.Errorf("image has no files under %s", srcDir)
	}
	return nil
}
