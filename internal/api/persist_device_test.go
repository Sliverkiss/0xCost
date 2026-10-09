package api

// DEV-012 (ADR 0013): a login that ends in a demand still leaves the device
// identity it harvested in the account record. Without this the next attempt
// presents a different device, the operator's pending code no longer belongs to
// any device we claim, and the platform rate-limits the next SMS send
// (live: code=20024).

import (
	"errors"
	"testing"

	"github.com/yg/mimo2api/internal/accounts"
	"github.com/yg/mimo2api/internal/mimo"
)

func TestPersistDeviceFromError(t *testing.T) {
	t.Run("otp demand", func(t *testing.T) {
		acct := &accounts.Account{ID: "a1"}
		err := &mimo.ErrOTPPending{Device: map[string]string{"deviceId": "wb_1", "uLocale": "zh_CN"}}
		if !persistDeviceFromError(acct, err) {
			t.Fatal("device not persisted from the OTP demand")
		}
		if acct.Device["deviceId"] != "wb_1" || acct.Device["uLocale"] != "zh_CN" {
			t.Fatalf("device = %+v, want the harvested identity", acct.Device)
		}
	})

	t.Run("account action (auto-start refused)", func(t *testing.T) {
		acct := &accounts.Account{ID: "a1", Device: map[string]string{"deviceId": "wb_old", "cUserId": "cu_1"}}
		err := &mimo.ErrAccountAction{Reason: "new-device OTP verification required", Device: map[string]string{"deviceId": "wb_new"}}
		if !persistDeviceFromError(acct, err) {
			t.Fatal("device not persisted from the account action")
		}
		if acct.Device["deviceId"] != "wb_new" {
			t.Errorf("deviceId = %q, want the fresh wb_new (server wins)", acct.Device["deviceId"])
		}
		if acct.Device["cUserId"] != "cu_1" {
			t.Errorf("cUserId = %q, want the stored cu_1 kept (server stayed silent)", acct.Device["cUserId"])
		}
	})

	t.Run("unrelated error leaves the record alone", func(t *testing.T) {
		acct := &accounts.Account{ID: "a1", Device: map[string]string{"deviceId": "wb_old"}}
		if persistDeviceFromError(acct, errors.New("transport boom")) {
			t.Fatal("persisted a device from an error that carries none")
		}
		if acct.Device["deviceId"] != "wb_old" {
			t.Errorf("device mutated: %+v", acct.Device)
		}
	})

	t.Run("identical identity reports no change", func(t *testing.T) {
		acct := &accounts.Account{ID: "a1", Device: map[string]string{"deviceId": "wb_1"}}
		err := &mimo.ErrOTPPending{Device: map[string]string{"deviceId": "wb_1"}}
		if persistDeviceFromError(acct, err) {
			t.Fatal("reported a change for an identical identity — every failed login would rewrite the accounts file")
		}
	})
}
