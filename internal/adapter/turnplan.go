package adapter

import (
	"fmt"
	"strings"
)

// TurnPlan is the sequence of user queries to send upstream for one client
// request, per SDD §5.3:
//   - Sticky hit: only the delta messages after the stored frontier.
//   - Miss / non-streaming: full chain replay, one upstream call per user
//     message, chained via previousDialogueId. System messages splice into the
//     first user query as a preamble (API-017); they are never dropped.
//     Assistant history is not re-sent (accepted trade-off, SDD §5.3B).
type TurnPlan struct {
	Queries []string
}

// ErrNoUserMessage is returned when the request has no sendable user turn.
type ErrNoUserMessage struct{}

func (ErrNoUserMessage) Error() string { return "messages contains no sendable user message" }

// ErrTooManyTurns is returned when the replay chain exceeds the cap (SDD
// §5.3B: cap 20, configurable).
type ErrTooManyTurns struct {
	Turns int
	Cap   int
}

func (e *ErrTooManyTurns) Error() string {
	return fmt.Sprintf("conversation has %d user turns, exceeding the replay cap of %d; start a new conversation or trim history", e.Turns, e.Cap)
}

// PlanFullReplay builds the miss-path turn plan: one query per user message,
// in order. All system messages concatenate and splice into the first user
// query as "[systems]\n\n---\n\n<first user message>" (API-017).
func PlanFullReplay(msgs []Msg, maxTurns int) (*TurnPlan, error) {
	var systemParts []string
	var userMsgs []string
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if t := strings.TrimSpace(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			if m.Content != "" {
				userMsgs = append(userMsgs, m.Content)
			}
		}
	}
	if len(userMsgs) == 0 {
		return nil, ErrNoUserMessage{}
	}
	if maxTurns > 0 && len(userMsgs) > maxTurns {
		return nil, &ErrTooManyTurns{Turns: len(userMsgs), Cap: maxTurns}
	}
	plan := &TurnPlan{Queries: make([]string, 0, len(userMsgs))}
	for i, q := range userMsgs {
		if i == 0 && len(systemParts) > 0 {
			q = strings.Join(systemParts, "\n\n") + "\n\n---\n\n" + q
		}
		plan.Queries = append(plan.Queries, q)
	}
	return plan, nil
}

// ComposedFirstQueryLen returns the length of the query the planner would send
// first, plus how much of it is preamble rather than the client's own text.
//
// The platform's ceiling applies to the query text it receives, and the planner
// splices every system message into the first user query (API-017) — so a tool
// layer that injects a protocol prompt, or a client with a large system prompt,
// spends the same 102,400-char budget the user's message needs (ADR 0015). This
// is the worst case for the first turn, and the exact text a miss-path replay
// sends.
func ComposedFirstQueryLen(msgs []Msg) (composed, overhead int) {
	var systemParts []string
	first := ""
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if t := strings.TrimSpace(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			if first == "" && m.Content != "" {
				first = m.Content
			}
		}
	}
	composed = len([]rune(first))
	if len(systemParts) > 0 {
		preamble := strings.Join(systemParts, "\n\n") + "\n\n---\n\n"
		overhead = len([]rune(preamble))
		composed += overhead
	}
	return composed, overhead
}

// PlanDelta builds the sticky-hit turn plan: only the user messages after the
// stored frontier (storedMsgCount = non-system message count at the time the
// entry was stored). System messages appearing in the delta tail splice into
// the first delta user query. Returns ErrNoUserMessage when there is nothing
// new to send.
func PlanDelta(msgs []Msg, storedMsgCount int) (*TurnPlan, error) {
	if storedMsgCount < 0 {
		return nil, ErrNoUserMessage{}
	}
	// Walk the FULL list (not a system-filtered copy) so that a system message
	// the client appends after the stored frontier is carried into the delta:
	// filtering system messages out first made the tail system-free by
	// construction, silently dropping them and making the splice below dead
	// code. Non-system messages still advance the frontier, so the frontier
	// arithmetic the caller stores (CountNonSystem) is unchanged.
	var systemParts []string
	var queries []string
	seen := 0
	for _, m := range msgs {
		if m.Role == "system" {
			if seen >= storedMsgCount {
				if t := strings.TrimSpace(m.Content); t != "" {
					systemParts = append(systemParts, t)
				}
			}
			continue
		}
		seen++
		if seen <= storedMsgCount {
			continue
		}
		if m.Role != "user" || m.Content == "" {
			continue
		}
		q := m.Content
		if len(queries) == 0 && len(systemParts) > 0 {
			q = strings.Join(systemParts, "\n\n") + "\n\n---\n\n" + q
		}
		queries = append(queries, q)
	}
	if len(queries) == 0 {
		return nil, ErrNoUserMessage{}
	}
	return &TurnPlan{Queries: queries}, nil
}
