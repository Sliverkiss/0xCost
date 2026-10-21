package pool

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// stateFile is the on-disk pool state: the idle virgin stock only.
//
// Identities are one-shot cartridges, so the file deliberately excludes leased
// rows (they may already have spent their message upstream). A discarded
// identity is removed at discard time and never restored.
type stateFile struct {
	Version    int         `json:"version"`
	SavedAt    time.Time   `json:"saved_at"`
	Identities []*Identity `json:"identities"`
}

// stateVersion 3 has no usage, retirement, cooling or refresh fields: a
// cartridge is either a stocked virgin or it does not exist. Versions 1 and 2
// are still read; their non-virgin rows are dropped on load.
const stateVersion = 3

// legacyRow is a version 1/2 identity row. It exists only to migrate old
// files: those versions tracked usage and retirement, which v3 deliberately
// cannot represent.
type legacyRow struct {
	Identity
	Used       int    `json:"used"`
	Retired    bool   `json:"retired"`
	RefreshExp string `json:"refresh_exp"`
}

// load restores the virgin stock; a missing or corrupt file is a zero-state
// start.
func (p *Pool) load() {
	raw, err := os.ReadFile(p.cfg.StateFile)
	if err != nil {
		return
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		p.quarantine(err)
		return
	}
	if header.Version != 1 && header.Version != 2 && header.Version != stateVersion {
		// A future version is not corrupt, just unknown: preserve it instead
		// of overwriting it with this version's state on the first flush.
		p.quarantine(fmt.Errorf("state version %d (want %d)", header.Version, stateVersion))
		return
	}
	now := time.Now()
	restored, dropped := 0, 0
	p.mu.Lock()
	if header.Version == stateVersion {
		var sf stateFile
		if err := json.Unmarshal(raw, &sf); err != nil {
			p.mu.Unlock()
			p.quarantine(err)
			return
		}
		for _, id := range sf.Identities {
			if id == nil || id.UID == "" || id.AccessToken == "" {
				continue
			}
			id.InFlight = false
			if !id.usableLocked(now) {
				dropped++
				continue
			}
			if _, dup := p.byUID[id.UID]; dup {
				continue
			}
			p.byUID[id.UID] = id
			p.stock = append(p.stock, id.UID)
			restored++
		}
	} else {
		var sf struct {
			Version    int         `json:"version"`
			Identities []legacyRow `json:"identities"`
		}
		if err := json.Unmarshal(raw, &sf); err != nil {
			p.mu.Unlock()
			p.quarantine(err)
			return
		}
		for _, row := range sf.Identities {
			id := row.Identity
			// Only virgins are restored. A row with Used > 0, a retired row or
			// an expired token is not a cartridge we can guarantee: its
			// remaining messages are not ours to squeeze, so it is dropped and
			// replaced.
			if id.UID == "" || id.AccessToken == "" || row.Used > 0 || row.Retired || !id.usableLocked(now) {
				dropped++
				continue
			}
			if _, dup := p.byUID[id.UID]; dup {
				continue
			}
			id.InFlight = false
			p.byUID[id.UID] = &id
			p.stock = append(p.stock, id.UID)
			restored++
		}
	}
	p.mu.Unlock()
	if restored > 0 || dropped > 0 {
		log.Printf("[pool] restored %d virgin identities from %s (dropped %d non-virgin/expired)", restored, p.cfg.StateFile, dropped)
	}
}

// saveLocked writes the idle virgin stock atomically (temp file + rename).
// Leased identities are omitted: the process cannot prove they are unused.
func (p *Pool) saveLocked() {
	if p.cfg.StateFile == "" {
		return
	}
	now := time.Now()
	ids := make([]*Identity, 0, len(p.stock))
	for _, uid := range p.stock {
		id, ok := p.byUID[uid]
		if !ok || !id.usableLocked(now) {
			continue
		}
		cp := *id
		cp.InFlight = false
		ids = append(ids, &cp)
	}
	sf := stateFile{Version: stateVersion, SavedAt: now, Identities: ids}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		log.Printf("ERR: [pool] encode state: %v", err)
		return
	}
	dir := filepath.Dir(p.cfg.StateFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			log.Printf("ERR: [pool] create state dir: %v", err)
			return
		}
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		log.Printf("ERR: [pool] create temp state: %v", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		log.Printf("ERR: [pool] chmod temp state: %v", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		log.Printf("ERR: [pool] write temp state: %v", err)
		return
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		log.Printf("ERR: [pool] sync temp state: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("ERR: [pool] close temp state: %v", err)
		return
	}
	if err := os.Rename(tmpName, p.cfg.StateFile); err != nil {
		log.Printf("ERR: [pool] rename state: %v", err)
		return
	}
	// Rename is atomic, but the new directory entry is only durable after the
	// directory itself is synced. Without this a power loss can leave the old
	// file (or neither) even though the write reported success.
	if dir != "" && dir != "." {
		if d, err := os.Open(dir); err == nil {
			if err := d.Sync(); err != nil {
				log.Printf("WARN: [pool] sync state dir: %v", err)
			}
			d.Close()
		}
	}
}

// quarantine preserves an unreadable or unknown-version state file next to the
// live one before the process starts writing a fresh document.
func (p *Pool) quarantine(reason error) {
	backup := p.cfg.StateFile + ".corrupt"
	log.Printf("WARN: [pool] state file %s is unusable (%v); preserving it as %s", p.cfg.StateFile, reason, backup)
	if err := os.Rename(p.cfg.StateFile, backup); err != nil {
		log.Printf("ERR: [pool] quarantine state file: %v", err)
	}
}

// Export returns the current state as JSON (acct CLI / debugging).
func (p *Pool) Export() ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	ids := make([]*Identity, 0, len(p.stock))
	for _, uid := range p.stock {
		id, ok := p.byUID[uid]
		if !ok || !id.usableLocked(now) {
			continue
		}
		cp := *id
		cp.InFlight = false
		ids = append(ids, &cp)
	}
	return json.MarshalIndent(stateFile{Version: stateVersion, SavedAt: now, Identities: ids}, "", "  ")
}

// SetAccessExpiryForTest backdates one identity's access-token expiry, letting
// tests in other packages exercise expiry and GC without reaching the
// unexported pool state.
func (p *Pool) SetAccessExpiryForTest(uid string, at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id, ok := p.byUID[uid]; ok {
		id.AccessExp = at
	}
}

// String describes the pool state for logs.
func (p *Pool) String() string {
	c := p.Counts()
	return fmt.Sprintf("stock=%d total=%d in_flight=%d waiters=%d", c.Stock, c.Total, c.InFlight, c.Waiters)
}
