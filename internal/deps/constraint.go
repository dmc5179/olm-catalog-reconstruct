package deps

import (
	"encoding/json"
	"fmt"
	"strings"
)

// constraint is the olm.constraint property value
// (https://olm.operatorframework.io/docs/concepts/olm-architecture/dependency-resolution/).
type constraint struct {
	FailureMessage string          `json:"failureMessage,omitempty"`
	Package        *pkgConstraint  `json:"package,omitempty"`
	GVK            *gvk            `json:"gvk,omitempty"`
	All            *compound       `json:"all,omitempty"`
	Any            *compound       `json:"any,omitempty"`
	Not            *compound       `json:"not,omitempty"`
	CEL            *celConstraint  `json:"cel,omitempty"`
	Raw            json.RawMessage `json:"-"`
}

type pkgConstraint struct {
	PackageName  string `json:"packageName"`
	VersionRange string `json:"versionRange"`
}

type compound struct {
	Constraints []constraint `json:"constraints"`
}

type celConstraint struct {
	Rule string `json:"rule"`
}

func parseConstraint(raw json.RawMessage) (constraint, error) {
	var c constraint
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, err
	}
	c.Raw = raw
	return c, nil
}

func (c constraint) String() string {
	switch {
	case c.Package != nil:
		return pkgReq(c.Package.PackageName, c.Package.VersionRange)
	case c.GVK != nil:
		return c.GVK.String()
	case c.All != nil:
		return "all(" + joinConstraints(c.All.Constraints) + ")"
	case c.Any != nil:
		return "any(" + joinConstraints(c.Any.Constraints) + ")"
	case c.Not != nil:
		return "not(" + joinConstraints(c.Not.Constraints) + ")"
	case c.CEL != nil:
		return "cel(" + c.CEL.Rule + ")"
	}
	return "unknown constraint " + string(c.Raw)
}

func joinConstraints(cs []constraint) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = c.String()
	}
	return strings.Join(parts, ", ")
}

func pkgReq(name, rng string) string {
	if rng == "" {
		return "package " + name
	}
	return fmt.Sprintf("package %s %s", name, rng)
}

type tri int

const (
	unsat tri = iota
	sat
	unknown
)
