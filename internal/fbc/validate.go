package fbc

import (
	"os"

	"github.com/operator-framework/operator-registry/alpha/declcfg"
)

// Validate runs opm's own model conversion and validation.
func Validate(dc declcfg.DeclarativeConfig) error {
	m, err := declcfg.ConvertToModel(dc)
	if err != nil {
		return err
	}
	return m.Validate()
}

// WriteDir writes dc as an FBC directory tree (one directory per package),
// replacing any existing content at dir.
func WriteDir(dc declcfg.DeclarativeConfig, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return declcfg.WriteFS(dc, dir, declcfg.WriteJSON, ".json")
}
