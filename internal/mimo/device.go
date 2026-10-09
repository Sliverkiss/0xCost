package mimo

import "strings"

// Device identity, kept separate from the SESSION cookies.
//
// The passport flow has two layers (docs/login-chain-reverse.md §2, §5 item 1):
//
//   - the session (xiaomichatbot_serviceToken/slh/ph + serviceToken) — what the
//     upstream API authorises on, minted fresh by every STS exchange;
//   - the device identity — cookies the passport plants to recognise the
//     hardware, notably `deviceId` (+ `pass_ua`, `uLocale`). These survive
//     sessions and are what the "trusted device" state is bound to.
//
// Live evidence (2026-10-09, free 3-attempt experiment):
//
//	Auth2 without any cookie            → plants deviceId, pass_ua, uLocale;
//	                                      answers code 0 + notificationUrl
//	                                      (new-device verification demanded)
//	Auth2 replaying those three         → same demand
//	Auth2 replaying deviceId alone      → same demand
//
// so replaying the cookies does not by itself skip verification — the trust
// must first be established by a completed verification bound to that device.
// What the experiment does prove is the failure mode this file fixes: with no
// device identity presented at all, EVERY login is a brand-new device, so the
// trust can never attach. The pool therefore persists the harvested identity
// and replays it on every subsequent login/renewal hop.
//
// deviceCookieNames is the whitelist harvested out of the chain jars. Session
// cookies are deliberately excluded: they are rewritten by every STS exchange
// and must not be carried into a later login.
var deviceCookieNames = []string{"deviceId", "pass_ua", "uLocale", "cUserId", "sdkVersion"}

// harvestDevice copies the device-identity cookies out of a chain jar. Returns
// nil when the jar holds none, so an account that has never logged in stays
// JSON-clean (`device` omitted).
func harvestDevice(jar map[string]string) map[string]string {
	var out map[string]string
	for _, k := range deviceCookieNames {
		if v := jar[k]; v != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[k] = v
		}
	}
	return out
}

// MergeDevice merges a freshly harvested device identity over the stored one:
// values the server just handed out win, values it stayed silent about are kept
// (a login that plants no device cookies must NOT wipe the stored identity).
// Returns nil when neither side has anything. Callers persist the result on the
// account (api/session_apply.go, cmd/mimo2api).
func MergeDevice(stored, fresh map[string]string) map[string]string {
	if len(stored) == 0 && len(fresh) == 0 {
		return nil
	}
	out := make(map[string]string, len(stored)+len(fresh))
	for k, v := range stored {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range fresh {
		if v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// seedDeviceJar returns a jar pre-seeded with the device identity, so the
// whole SSO chain (which renders the jar into one Cookie header per hop)
// presents the same device the real browser would.
func seedDeviceJar(device map[string]string) map[string]string {
	jar := map[string]string{}
	for k, v := range device {
		if v != "" {
			jar[k] = v
		}
	}
	return jar
}

// deviceCookieHeader renders the device cookies for a request that does not go
// through the SSO jar (the two plain requests: serviceLoginAuth2 and the
// renewal serviceLogin). Order follows deviceCookieNames so captures and tests
// stay deterministic.
func deviceCookieHeader(device map[string]string) string {
	if len(device) == 0 {
		return ""
	}
	parts := make([]string, 0, len(device))
	for _, k := range deviceCookieNames {
		if v := device[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	// Anything stored under a name outside the whitelist still belongs to the
	// device blob (e.g. a future passport cookie) — append it deterministically.
	if len(parts) != len(device) {
		extra := make([]string, 0, len(device))
		for k, v := range device {
			if v == "" || containsString(deviceCookieNames, k) {
				continue
			}
			extra = append(extra, k+"="+v)
		}
		sortStrings(extra)
		parts = append(parts, extra...)
	}
	return strings.Join(parts, "; ")
}

func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
