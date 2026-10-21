// Package egress produces the X-Forwarded-For value sent upstream.
//
// Findings (docs/DESIGN-ANON.md §6): the server applies a per-IP gate that can
// reject a second message from the same X-Forwarded-For, while the identity
// budget itself is per identity. XFF is therefore a pacing dimension.
//
// Live measurement (2026-10-20, this project): random *public* addresses are
// rejected with 429 10061 当前访客资源不足 on essentially every request, while
// RFC1918 addresses are accepted — a fixed 10.x address worked for three
// consecutive identities, and rapid random 10.x addresses all succeeded. The
// upstream therefore recognises private ranges as "not a real client IP" and
// skips the gate. The default mode is consequently "private"; "random" (public)
// remains available but is not recommended.
package egress

import (
	"math/rand/v2"
	"sync"

	"chatglm2api/internal/config"
)

// Manager is safe for concurrent use.
type Manager struct {
	mode  string
	fixed string
	pool  []string
	mu    sync.Mutex
	next  int
	rng   *rand.Rand
}

// New builds a Manager from configuration. It assumes the config was already
// validated (config.Normalize checks mode/IP shapes).
func New(cfg config.Egress) *Manager {
	m := &Manager{mode: cfg.Mode, fixed: cfg.XFFFixed, pool: cfg.XFFPool}
	if m.mode == "" {
		m.mode = "private"
	}
	m.rng = rand.New(rand.NewPCG(uint64(rand.Uint64()), uint64(rand.Uint64())))
	return m
}

// Next returns the X-Forwarded-For value for one upstream call. An empty
// result means no header is sent.
func (m *Manager) Next() string {
	if m == nil {
		return ""
	}
	switch m.mode {
	case "fixed":
		return m.fixed
	case "pool":
		if len(m.pool) == 0 {
			return ""
		}
		m.mu.Lock()
		ip := m.pool[m.next%len(m.pool)]
		m.next++
		m.mu.Unlock()
		return ip
	case "random":
		return m.randomPublic()
	default:
		return m.randomPrivate()
	}
}

// Mode reports the configured mode.
func (m *Manager) Mode() string {
	if m == nil {
		return "none"
	}
	return m.mode
}

// randomPrivate mints a random RFC1918 address. The upstream accepts these as
// X-Forwarded-For values without applying its per-IP guest gate (measured).
func (m *Manager) randomPrivate() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	octet := func() int { return m.rng.IntN(254) + 1 }
	switch m.rng.IntN(3) {
	case 0:
		return "10." + itoa(octet()) + "." + itoa(octet()) + "." + itoa(octet())
	case 1:
		return "172." + itoa(m.rng.IntN(16)+16) + "." + itoa(octet()) + "." + itoa(octet())
	default:
		return "192.168." + itoa(octet()) + "." + itoa(octet())
	}
}

// randomPublic mints a random IPv4 outside the ranges that the real client
// would never present (loopback, private, link-local, multicast, reserved).
// The upstream accepted plain values including private 10.x during probing;
// public addresses are still preferable because the per-IP gate is real.
func (m *Manager) randomPublic() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < 32; i++ {
		a := m.rng.IntN(223) + 1 // 1..223
		if a == 10 || a == 127 || a == 0 || (a >= 224) {
			continue
		}
		b := m.rng.IntN(256)
		c := m.rng.IntN(256)
		d := m.rng.IntN(254) + 1
		if a == 172 && b >= 16 && b <= 31 {
			continue
		}
		if a == 192 && b == 168 {
			continue
		}
		if a == 169 && b == 254 {
			continue
		}
		if a == 100 && b >= 64 && b <= 127 {
			continue
		}
		return itoa(a) + "." + itoa(b) + "." + itoa(c) + "." + itoa(d)
	}
	return "203.0.113.1" // TEST-NET-3 fallback; never likely to be gated
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [4]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
