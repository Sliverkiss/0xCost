package api

// GET /v1/admin/pool — the authenticated pool view (SDD §4.9, spec §6).
//
// The aggregate /healthz contract stays frozen: the Docker healthcheck parses it
// (REQ-13). Everything an operator needs to reason about a pool — why an account
// is cooling, which class keeps failing, what a risk-control signal looked like,
// which egress carries how many accounts — lives here instead, behind the same
// auth as the other admin endpoints.
//
// Nothing credential-bearing crosses this boundary: ids are masked with a stable
// one-way hash, egress appears as its LABEL (never an endpoint or its
// credentials), and a risk event carries a body digest rather than a body
// (REQ-14, INV-4).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"
)

// maskID renders an account id as a stable, non-reversible handle: the same
// account keeps the same handle across reads and restarts (so an operator can
// line two readings up), while the id itself does not appear.
func maskID(id string) string {
	sum := sha256.Sum256([]byte("mimo2api/pool/" + id))
	return "acct-" + hex.EncodeToString(sum[:4])
}

type poolLastErrorView struct {
	Status int       `json:"status,omitempty"`
	At     time.Time `json:"at"`
}

// poolSummaryView is the same aggregates /healthz exposes — reported here too
// so an operator reading the detail view does not have to fetch both.
type poolSummaryView struct {
	Total     int `json:"total"`
	Available int `json:"available"`
	Disabled  int `json:"disabled"`
}

type poolAccountView struct {
	ID            string             `json:"id"`
	Egress        string             `json:"egress"`
	State         string             `json:"state"`
	Permanent     bool               `json:"permanent,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	InFlight      int                `json:"in_flight"`
	MaxInFlight   int                `json:"max_in_flight"`
	EWMALatencyMS int64              `json:"ewma_latency_ms"`
	LastError     *poolLastErrorView `json:"last_error,omitempty"`
	// CredentialAgeS is how long ago the account's session was fetched. A stale
	// session is the usual explanation for a run of auth-class failures.
	CredentialAgeS int64 `json:"credential_age_s"`
}

type poolRiskEventView struct {
	AccountID  string    `json:"account_id"`
	Egress     string    `json:"egress"`
	Model      string    `json:"model,omitempty"`
	Channel    string    `json:"channel,omitempty"`
	Status     int       `json:"status,omitempty"`
	BodyDigest string    `json:"body_digest,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	At         time.Time `json:"at"`
}

func (s *Server) handlePoolView(w http.ResponseWriter, r *http.Request) {
	sum := s.pool.Summarize()

	ages := map[string]int64{}
	now := time.Now().Unix()
	for _, a := range s.store.Accounts() {
		if a.Session != nil && a.Session.FetchedAt > 0 {
			ages[a.ID] = now - a.Session.FetchedAt
		}
	}

	views := make([]poolAccountView, 0, sum.Total)
	for _, snap := range s.pool.Snapshot() {
		v := poolAccountView{
			ID:             maskID(snap.ID),
			Egress:         snap.Egress,
			State:          string(snap.State),
			Permanent:      snap.Permanent,
			Reason:         snap.Reason,
			InFlight:       snap.InFlight,
			MaxInFlight:    snap.MaxInFlight,
			EWMALatencyMS:  snap.EWMALatencyMS,
			CredentialAgeS: ages[snap.ID],
		}
		if !snap.LastErrorAt.IsZero() {
			v.LastError = &poolLastErrorView{
				Status: snap.LastErrorStatus, At: snap.LastErrorAt,
			}
		}
		views = append(views, v)
	}

	events := s.pool.RiskEvents()
	evViews := make([]poolRiskEventView, 0, len(events))
	signalCounts := map[string]int{}
	seenPerEgress := map[string]map[string]bool{}
	for _, ev := range events {
		evViews = append(evViews, poolRiskEventView{
			AccountID:  maskID(ev.AccountID),
			Egress:     ev.Egress,
			Model:      ev.Model,
			Channel:    ev.Channel,
			Status:     ev.Status,
			BodyDigest: ev.BodyDigest,
			Reason:     ev.Reason,
			At:         ev.At,
		})
		if seenPerEgress[ev.Egress] == nil {
			seenPerEgress[ev.Egress] = map[string]bool{}
		}
		seenPerEgress[ev.Egress][ev.AccountID] = true
	}
	for egress, accts := range seenPerEgress {
		signalCounts[egress] = len(accts)
	}

	// The accounting invariant (INV-1) is reported rather than assumed: an
	// operator reading this endpoint should never have to add up states by hand
	// to trust the counts.
	invariantOK := sum.Available+sum.Disabled == sum.Total

	response := struct {
		Accounts    poolSummaryView     `json:"accounts"`
		InvariantOK bool                `json:"invariant_ok"`
		Pool        []poolAccountView   `json:"pool"`
		RiskEvents  []poolRiskEventView `json:"risk_events"`
		// EgressSignals: distinct accounts per egress label with a recorded risk
		// signal (SDD §4.5.2). One is inconclusive; several point at the egress.
		EgressSignals map[string]int `json:"egress_signals"`
	}{
		Accounts: poolSummaryView{
			Total:     sum.Total,
			Available: sum.Available,
			Disabled:  sum.Disabled,
		},
		InvariantOK:   invariantOK,
		Pool:          views,
		RiskEvents:    evViews,
		EgressSignals: signalCounts,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
