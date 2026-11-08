package deps

import (
	_ "embed"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

//go:embed known.yaml
var defaultKnown []byte

// KnownDep lists dependencies an operator has that its catalog metadata does not declare.
type KnownDep struct {
	Package  string          `json:"package"`
	Requires []KnownRequired `json:"requires"`
}

// KnownRequired is one undeclared dependency.
type KnownRequired struct {
	Package string `json:"package"`
	Channel string `json:"channel,omitempty"` // "same" or "default"
	Source  string `json:"source"`
}

// DefaultKnownDeps returns the built-in list.
func DefaultKnownDeps() ([]KnownDep, error) {
	return parseKnown(defaultKnown, "built-in known.yaml")
}

// LoadKnownDeps reads an additional list from a file.
func LoadKnownDeps(path string) ([]KnownDep, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseKnown(data, path)
}

func parseKnown(data []byte, where string) ([]KnownDep, error) {
	var out []KnownDep
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", where, err)
	}
	for _, k := range out {
		if k.Package == "" {
			return nil, fmt.Errorf("%s: entry with empty package", where)
		}
		for _, r := range k.Requires {
			if r.Package == "" {
				return nil, fmt.Errorf("%s: %s has a requirement with empty package", where, k.Package)
			}
			switch r.Channel {
			case "", "same", "default":
			default:
				return nil, fmt.Errorf("%s: %s -> %s: channel must be 'same' or 'default'", where, k.Package, r.Package)
			}
		}
	}
	return out, nil
}
