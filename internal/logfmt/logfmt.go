// Package logfmt provides small helpers for consistent, privacy-preserving log labels.
package logfmt

// Label renders a stable log label for an identity: the first 8 characters of the
// uid plus an optional nickname. Guest uids are 24 hex chars; truncating keeps
// request logs readable without printing the full identity.
func Label(uid, nickname string) string {
	id := uid
	if len(id) > 8 {
		id = id[:8]
	}
	if nickname == "" {
		return id
	}
	return id + "/" + nickname
}
