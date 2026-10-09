package egress

import (
	"strings"
	"testing"
)

// EGRESS-001: label validation — the charset is what keeps the env name derivable.
func TestLabelValidation(t *testing.T) {
	for _, ok := range []string{"hk1", "sg-2", "a", "0123456789abcdef"} {
		if err := Validate(ok); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "HK1", "hk 1", "hk_1", "hk.1", "中文", strings.Repeat("a", 17)} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", bad)
		}
	}
}

// EGRESS-002: an env name is derivable from the label, dashes included.
func TestEnvName(t *testing.T) {
	cases := map[string]string{"hk1": "MIMO2API_EGRESS_HK1", "sg-2": "MIMO2API_EGRESS_SG_2"}
	for label, want := range cases {
		if got := EnvName(label); got != want {
			t.Errorf("EnvName(%q) = %q, want %q", label, got, want)
		}
	}
}

// EGRESS-003: a label with no endpoint is an error that names the variable and
// the label — and never the value.
func TestResolveMissingEndpointNamesLabel(t *testing.T) {
	_, err := Resolve([]string{"hk1"}, func(string) string { return "" })
	if err == nil {
		t.Fatal("Resolve returned no error for a label without an endpoint")
	}
	msg := err.Error()
	if !strings.Contains(msg, "MIMO2API_EGRESS_HK1") || !strings.Contains(msg, "hk1") {
		t.Errorf("error does not name the variable: %v", err)
	}
	if !strings.Contains(msg, "accounts.json") {
		t.Errorf("error does not say what to change: %v", err)
	}
}

// EGRESS-004: the endpoint's credentials stay out of the error text (this string
// reaches logs and operator output).
func TestResolveErrorsNeverEchoTheEndpoint(t *testing.T) {
	secret := "http://user:s3cr3t@proxy.example:8080"
	_, err := Resolve([]string{"hk1", "sg2"}, func(name string) string {
		if name == EnvName("hk1") {
			return secret
		}
		return "not a url at all"
	})
	if err == nil {
		t.Fatal("Resolve accepted a malformed endpoint")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Errorf("the error leaks credentials: %v", err)
	}
	if !strings.Contains(err.Error(), "MIMO2API_EGRESS_SG2") {
		t.Errorf("the malformed variable is not named: %v", err)
	}
	// The one that was well-formed still resolved; only the broken one is fatal.
}

// EGRESS-005: a well-formed set resolves per label, and the empty label is not a
// label at all (it means the shared egress).
func TestResolvePerLabel(t *testing.T) {
	env := map[string]string{
		EnvName("hk1"): "http://proxy.example:8080",
		EnvName("sg2"): "socks5://proxy.example:1080",
	}
	proxies, err := Resolve([]string{"hk1", "sg2"}, func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("resolved %d endpoints, want 2", len(proxies))
	}
	if got := proxies["hk1"].Host; got != "proxy.example:8080" {
		t.Errorf("hk1 host = %q", got)
	}
	if got := proxies["sg2"].Scheme; got != "socks5" {
		t.Errorf("sg2 scheme = %q, want socks5", got)
	}
	if labels := Labels([]string{"hk1", "", "hk1", "sg2"}); len(labels) != 2 {
		t.Errorf("Labels = %v, want the two distinct non-empty ones", labels)
	}
	// No accounts labelled ⇒ no requirement, no error.
	if out, err := Resolve(nil, func(string) string { return "" }); err != nil || len(out) != 0 {
		t.Errorf("Resolve(nil) = %v, %v; want no endpoints and no error", out, err)
	}
}
