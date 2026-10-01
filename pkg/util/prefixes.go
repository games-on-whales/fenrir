package util

import (
	"fmt"
	"net/netip"
	"strings"
)

// ParsePrefixes parses a comma-separated list of CIDRs, masking each.
func ParsePrefixes(s string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	for cidr := range strings.SplitSeq(s, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(cidr))
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}
