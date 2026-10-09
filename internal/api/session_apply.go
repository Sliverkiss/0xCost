package api

import (
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
)

// applySession folds a freshly obtained session into the account record.
//
// Identity fields the flow could not refresh are kept, not blanked: the
// landing chain plants passToken/userId only on some passport variants, and
// blanking them would break the next renewal. The device identity is merged
// the same way — a login that plants no device cookies must not wipe a stored
// one (internal/mimo/device.go).
func applySession(acct *accounts.Account, sess *mimo.Session) {
	if sess == nil {
		return
	}
	if sess.UserID != "" {
		acct.UserID = sess.UserID
	}
	if sess.PassToken != "" {
		acct.PassToken = sess.PassToken
	}
	acct.Device = mimo.MergeDevice(acct.Device, sess.Device)
	acct.Session = &accounts.Session{Cookie: sess.Cookie, PH: sess.PH, FetchedAt: time.Now().Unix()}
}
