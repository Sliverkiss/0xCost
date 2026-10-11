// Package egress resolves per-account egress LABELS into proxies (SDD §4.8,
// spec §7).
//
// An account's entry in accounts.json carries a label and nothing else
// ("egress": "hk1"). The endpoint and any credentials live in the environment
// (MIMO2API_EGRESS_HK1=http://user:pass@host:port), so a leaked or shared
// accounts.json never carries a credential, and the pool's view can print the
// label without printing how to use it.
//
// There is deliberately no fallback: a label with no endpoint is a startup error
// that names it. Falling back to a direct connection would send the account's
// traffic from the host's own IP — the exact thing the label exists to avoid —
// and it would fail silently, at the worst possible moment.
package egress

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// EnvPrefix is the environment prefix for a label's endpoint.
const EnvPrefix = "MIMO2API_EGRESS_"

// labelRe is the accepted label charset: lowercase letters, digits and dashes
// (owner decision, spec §10.4). It keeps the env name derivable and unambiguous.
var labelRe = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

// Valid reports whether a label is well formed.
func Valid(label string) bool { return labelRe.MatchString(label) }

// Validate returns a descriptive error for a malformed label.
func Validate(label string) error {
	if label == "" {
		return errors.New("egress label is empty")
	}
	if !Valid(label) {
		return fmt.Errorf("egress label %q is not %s", label, labelRe.String())
	}
	return nil
}

// EnvName is the environment variable holding a label's endpoint: "hk1" →
// MIMO2API_EGRESS_HK1. Dashes become underscores because a shell cannot export a
// name containing one.
func EnvName(label string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(label, "-", "_"))
}

// Resolve maps every label to its endpoint, reading them from the environment.
// An empty map (no labelled accounts) is not an error.
//
// Errors name the labels, never the values: an endpoint carries credentials, and
// this function's error text ends up in logs and in operator-facing output.
func Resolve(labels []string, getenv func(string) string) (map[string]*url.URL, error) {
	out := map[string]*url.URL{}
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	var missing, malformed []string
	seen := map[string]bool{}
	for _, label := range labels {
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		if err := Validate(label); err != nil {
			return nil, err
		}
		name := EnvName(label)
		raw := strings.TrimSpace(getenv(name))
		if raw == "" {
			missing = append(missing, fmt.Sprintf("%s (for label %q)", name, label))
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			malformed = append(malformed, fmt.Sprintf("%s (for label %q)", name, label))
			continue
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			malformed = append(malformed, fmt.Sprintf("%s (for label %q: scheme %q unsupported)", name, label, u.Scheme))
			continue
		}
		out[label] = u
	}
	sort.Strings(missing)
	sort.Strings(malformed)
	if len(missing) > 0 || len(malformed) > 0 {
		parts := []string{}
		if len(missing) > 0 {
			parts = append(parts, "no endpoint set for "+strings.Join(missing, ", "))
		}
		if len(malformed) > 0 {
			parts = append(parts, "endpoint unusable for "+strings.Join(malformed, ", "))
		}
		return nil, fmt.Errorf("egress: %s — set the variable, or remove the label from accounts.json", strings.Join(parts, "; "))
	}
	return out, nil
}

// Labels returns the distinct, validated labels in a set of account labels,
// skipping the empty one (which means the default, unlabelled egress).
func Labels(accountLabels []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range accountLabels {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}
