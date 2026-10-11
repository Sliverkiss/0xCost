package api

import (
	"errors"
	"time"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
)

// persistDeviceFromError stores the device identity a login attempt harvested
// before it stopped (an OTP demand, a captcha, a refusal).
//
// Why it matters: the new-device trust attaches to a device that has completed
// a verification. If the login aborts and we drop the identity it just received,
// the next attempt presents a different deviceId, the code the operator is
// holding belongs to a device we no longer claim, and the platform rate-limits
// the next SMS (live: code=20024). Persisting it here makes the retry the same
// device — the only way the code can still be used.
func persistDeviceFromError(acct *accounts.Account, err error) bool {
	var dev map[string]string
	var pending *mimo.ErrOTPPending
	var action *mimo.ErrAccountAction
	switch {
	case errors.As(err, &pending):
		dev = pending.Device
	case errors.As(err, &action):
		dev = action.Device
	}
	if len(dev) == 0 {
		return false
	}
	merged := mimo.MergeDevice(acct.Device, dev)
	if len(merged) == len(acct.Device) {
		same := true
		for k, v := range merged {
			if acct.Device[k] != v {
				same = false
				break
			}
		}
		if same {
			return false
		}
	}
	acct.Device = merged
	return true
}

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
