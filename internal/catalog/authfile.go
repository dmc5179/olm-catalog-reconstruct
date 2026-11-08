package catalog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
)

// authFile is the podman/docker auth.json format.
type authFile struct {
	Auths map[string]struct {
		Auth     string `json:"auth"`
		Username string `json:"username"`
		Password string `json:"password"`
	} `json:"auths"`
}

// AuthFileKeychain reads credentials from a single podman/docker auth file.
// Keys may be a registry host or a host/repository prefix; the longest
// matching key wins, like containers-auth.json(5).
type AuthFileKeychain struct {
	entries map[string]authn.AuthConfig
}

// NewAuthFileKeychain loads path.
func NewAuthFileKeychain(path string) (*AuthFileKeychain, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var af authFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("parse auth file %s: %w", path, err)
	}
	kc := &AuthFileKeychain{entries: map[string]authn.AuthConfig{}}
	for key, e := range af.Auths {
		cfg := authn.AuthConfig{Username: e.Username, Password: e.Password}
		if e.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(e.Auth)
			if err != nil {
				return nil, fmt.Errorf("auth file %s: bad auth for %s: %w", path, key, err)
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok {
				return nil, fmt.Errorf("auth file %s: bad auth for %s", path, key)
			}
			cfg.Username, cfg.Password = user, pass
		}
		key = strings.TrimPrefix(strings.TrimPrefix(key, "https://"), "http://")
		kc.entries[strings.TrimSuffix(key, "/")] = cfg
	}
	return kc, nil
}

// Resolve implements authn.Keychain.
func (k *AuthFileKeychain) Resolve(res authn.Resource) (authn.Authenticator, error) {
	target := res.RegistryStr()
	if repo, ok := res.(interface{ RepositoryStr() string }); ok {
		target += "/" + repo.RepositoryStr()
	}
	best := ""
	for key := range k.entries {
		if (target == key || strings.HasPrefix(target, key+"/")) && len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return authn.Anonymous, nil
	}
	return authn.FromConfig(k.entries[best]), nil
}
