package deps

import (
	"fmt"
	"slices"
	"strings"

	"github.com/dmc5179/olm-catalog-reconstruct/internal/isc"
)

// channelScope records, per catalog and package, whether any configuration
// selects the package without naming channels (oc-mirror v2 then mirrors every
// channel) and which channels configurations name explicitly.
type channelScope struct {
	allPackages map[string]bool                       // catalog -> entry with no packages (every package, every channel)
	noChannels  map[string]map[string]bool            // catalog -> package -> listed without channels
	explicit    map[string]map[string]map[string]bool // catalog -> package -> channel -> named (incl. defaultChannel overrides)
}

func newChannelScope(configs []*isc.ImageSetConfiguration) *channelScope {
	cs := &channelScope{
		allPackages: map[string]bool{},
		noChannels:  map[string]map[string]bool{},
		explicit:    map[string]map[string]map[string]bool{},
	}
	for _, cfg := range configs {
		for _, op := range cfg.Mirror.Operators {
			if len(op.Packages) == 0 {
				if !op.Full {
					cs.allPackages[op.Catalog] = true
				}
				continue
			}
			for _, p := range op.Packages {
				if cs.explicit[op.Catalog] == nil {
					cs.explicit[op.Catalog] = map[string]map[string]bool{}
					cs.noChannels[op.Catalog] = map[string]bool{}
				}
				if cs.explicit[op.Catalog][p.Name] == nil {
					cs.explicit[op.Catalog][p.Name] = map[string]bool{}
				}
				if len(p.Channels) == 0 {
					cs.noChannels[op.Catalog][p.Name] = true
				}
				for _, ch := range p.Channels {
					cs.explicit[op.Catalog][p.Name][ch.Name] = true
				}
				if p.DefaultChannel != "" {
					cs.explicit[op.Catalog][p.Name][p.DefaultChannel] = true
				}
			}
		}
	}
	return cs
}

// hint explains when a bundle the user's configuration selected is mirrored
// only because its package was listed without channels: it is not in the
// package's default channel or in any channel a configuration names. Returns
// "" when the bundle is in a channel the user evidently wants.
func (c *checker) hint(s *selection, cs *channelScope) string {
	if s.via != "" {
		return ""
	}
	cat, pkg := s.bi.cat, s.bi.b.Package
	ci := c.byRef[cat]
	if ci == nil {
		return ""
	}
	listedWithout := cs.noChannels[cat][pkg]
	wholeCatalog := cs.allPackages[cat] && cs.explicit[cat][pkg] == nil
	if !listedWithout && !wholeCatalog {
		return ""
	}
	wanted := cs.explicit[cat][pkg]
	for _, ch := range s.bi.channels {
		if ch == ci.defaults[pkg] || wanted[ch] {
			return ""
		}
	}
	channels := strings.Join(s.bi.channels, ", ")
	if wholeCatalog && !listedWithout {
		return fmt.Sprintf("mirrored only because the catalog entry lists no packages, so every channel of every package is mirrored; this bundle is only in %s, not the default channel %s",
			channels, ci.defaults[pkg])
	}
	wantedList := []string{ci.defaults[pkg]}
	for ch := range wanted {
		if ch != ci.defaults[pkg] {
			wantedList = append(wantedList, ch)
		}
	}
	slices.Sort(wantedList[1:])
	return fmt.Sprintf("mirrored only because %s is listed without channels, so all %d of its channels are mirrored; this bundle is only in %s. List just the channels you need (e.g. %s) to drop it and this dependency",
		pkg, len(ci.channels[pkg]), channels, strings.Join(wantedList, ", "))
}
