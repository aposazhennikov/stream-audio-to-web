package listeners

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
)

// Known is a listener the family knows: matched by receiver model (User-Agent substring)
// or by address range, named and placed by hand, because a city found by IP is often only
// where the provider registered its addresses.
type Known struct {
	Name      string   `json:"name"`
	Place     string   `json:"place,omitempty"`
	UserAgent string   `json:"userAgent,omitempty"` // substring of the User-Agent
	Networks  []string `json:"networks,omitempty"`  // address ranges, e.g. "185.213.228.0/22"

	nets []*net.IPNet
}

// LoadKnown reads the known listeners file: a JSON list of Known.
func LoadKnown(path string) ([]Known, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var known []Known
	if err := json.Unmarshal(data, &known); err != nil {
		return nil, fmt.Errorf("known listeners file %s: %w", path, err)
	}
	for i := range known {
		for _, cidr := range known[i].Networks {
			_, network, err := net.ParseCIDR(strings.TrimSpace(cidr))
			if err != nil {
				return nil, fmt.Errorf("known listener %q: %w", known[i].Name, err)
			}
			known[i].nets = append(known[i].nets, network)
		}
	}
	return known, nil
}

// match returns the first known listener this connection belongs to.
func match(known []Known, ip net.IP, userAgent string) (Known, bool) {
	for _, k := range known {
		if k.UserAgent != "" && strings.Contains(userAgent, k.UserAgent) {
			return k, true
		}
		if ip != nil {
			for _, network := range k.nets {
				if network.Contains(ip) {
					return k, true
				}
			}
		}
	}
	return Known{}, false
}
