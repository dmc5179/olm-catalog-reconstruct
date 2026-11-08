// Package isc parses the operator portion of oc-mirror v2 ImageSetConfiguration
// files and converts it into catalog-filter configuration exactly the way
// oc-mirror v2 (4.18+) does.
//
// The types below are a trimmed copy of
// github.com/openshift/oc-mirror/v2/internal/pkg/api/v2alpha1 (type_config.go,
// type_config_include.go). They are copied because Go forbids importing
// another module's internal packages. Only fields that influence which
// operator bundles get mirrored are kept.
package isc

import (
	"bytes"

	"fmt"
	"os"
	"strings"

	filter "github.com/sherine-k/catalog-filter/pkg/filter/mirror-config/v1alpha1"
	yamlv3 "gopkg.in/yaml.v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	Kind       = "ImageSetConfiguration"
	APIVersion = "mirror.openshift.io/v2alpha1"
)

// ImageSetConfiguration mirrors v2alpha1.ImageSetConfiguration.
type ImageSetConfiguration struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	Mirror          Mirror `json:"mirror" yaml:"mirror"`

	// Path is the file the configuration was loaded from (not serialized).
	Path string `json:"-" yaml:"-"`
}

// Mirror mirrors v2alpha1.Mirror, operators only.
type Mirror struct {
	Operators []Operator `json:"operators,omitempty" yaml:"operators,omitempty"`
}

// Operator mirrors v2alpha1.Operator.
type Operator struct {
	Catalog       string `json:"catalog" yaml:"catalog"`
	IncludeConfig `json:",inline" yaml:",inline"`
	TargetCatalog string `json:"targetCatalog,omitempty" yaml:"targetCatalog,omitempty"`
	TargetTag     string `json:"targetTag,omitempty" yaml:"targetTag,omitempty"`
	Full          bool   `json:"full,omitempty" yaml:"full,omitempty"`
}

// IncludeConfig mirrors v2alpha1.IncludeConfig.
type IncludeConfig struct {
	Packages []IncludePackage `json:"packages,omitempty" yaml:"packages,omitempty"`
}

// IncludePackage mirrors v2alpha1.IncludePackage.
type IncludePackage struct {
	Name           string           `json:"name" yaml:"name"`
	Channels       []IncludeChannel `json:"channels,omitempty" yaml:"channels,omitempty"`
	DefaultChannel string           `json:"defaultChannel,omitempty" yaml:"defaultChannel,omitempty"`
	IncludeBundle  `json:",inline" yaml:",inline"`
}

// IncludeChannel mirrors v2alpha1.IncludeChannel.
type IncludeChannel struct {
	Name          string `json:"name" yaml:"name"`
	IncludeBundle `json:",inline" yaml:",inline"`
}

// IncludeBundle mirrors v2alpha1.IncludeBundle.
type IncludeBundle struct {
	MinVersion string `json:"minVersion,omitempty" yaml:"minVersion,omitempty"`
	MaxVersion string `json:"maxVersion,omitempty" yaml:"maxVersion,omitempty"`
}

// Load reads and validates an ImageSetConfiguration file.
func Load(path string) (*ImageSetConfiguration, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &ImageSetConfiguration{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Kind != Kind {
		return nil, fmt.Errorf("%s: kind is %q, expected %q", path, cfg.Kind, Kind)
	}
	if cfg.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: apiVersion is %q, expected %q (only oc-mirror v2 configs are supported)", path, cfg.APIVersion, APIVersion)
	}
	for i, op := range cfg.Mirror.Operators {
		if strings.TrimSpace(op.Catalog) == "" {
			return nil, fmt.Errorf("%s: mirror.operators[%d].catalog is empty", path, i)
		}
	}
	cfg.Path = path
	return cfg, nil
}

// IsFullCatalog reports whether oc-mirror would mirror the catalog unfiltered.
// Copied from oc-mirror internal/pkg/operator/filtered_collector.go isFullCatalog.
func (o Operator) IsFullCatalog() bool {
	return len(o.Packages) == 0 && o.Full
}

// TargetKey identifies the catalog image oc-mirror would push for this entry.
// Entries with the same key end up in the same rebuilt catalog.
func (o Operator) TargetKey() string {
	key := o.Catalog
	if o.TargetCatalog != "" || o.TargetTag != "" {
		key += " -> " + o.TargetCatalog + ":" + o.TargetTag
	}
	return key
}

// FilterConfig converts the operator entry into a catalog-filter configuration.
// Copied from oc-mirror internal/pkg/operator/catalog_handler.go
// (filterPackage, filterFromImageSetConfig).
func (o Operator) FilterConfig() (filter.FilterConfiguration, error) {
	catFilter := filter.FilterConfiguration{
		TypeMeta: metav1.TypeMeta{
			Kind:       filter.FilterKind,
			APIVersion: filter.FilterAPIVersion,
		},
	}
	catFilter.Packages = make([]filter.Package, 0, len(o.Packages))
	for _, op := range o.Packages {
		catFilter.Packages = append(catFilter.Packages, filterPackage(op))
	}
	if err := catFilter.Validate(); err != nil {
		return catFilter, fmt.Errorf("failed to validate catalog filter: %w", err)
	}
	return catFilter, nil
}

func filterPackage(op IncludePackage) filter.Package {
	p := filter.Package{
		Name:           op.Name,
		DefaultChannel: op.DefaultChannel,
	}
	if op.MinVersion != "" {
		p.VersionRange = ">=" + op.MinVersion
	}
	if op.MaxVersion != "" {
		p.VersionRange += " <=" + op.MaxVersion
	}
	if len(op.Channels) == 0 {
		return p
	}
	p.Channels = make([]filter.Channel, 0, len(op.Channels))
	for _, ch := range op.Channels {
		fc := filter.Channel{Name: ch.Name}
		if ch.MinVersion != "" {
			fc.VersionRange = ">=" + ch.MinVersion
		}
		if ch.MaxVersion != "" {
			fc.VersionRange += " <=" + ch.MaxVersion
		}
		p.Channels = append(p.Channels, fc)
	}
	return p
}

// Marshal renders the configuration as YAML with fields in the conventional
// order (apiVersion, kind, mirror; name before channels).
func (c *ImageSetConfiguration) Marshal() ([]byte, error) {
	out := struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Mirror     Mirror `yaml:"mirror"`
	}{APIVersion: c.APIVersion, Kind: c.Kind, Mirror: c.Mirror}
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
