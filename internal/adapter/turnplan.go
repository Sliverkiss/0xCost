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

// PlanDelta builds the sticky-hit turn plan: only the user messages after the
// stored frontier (storedMsgCount = non-system message count at the time the
// entry was stored). System messages appearing in the delta tail splice into
// the first delta user query. Returns ErrNoUserMessage when there is nothing
// new to send.
func PlanDelta(msgs []Msg, storedMsgCount int) (*TurnPlan, error) {
	var non []Msg
	for _, m := range msgs {
		if m.Role != "system" {
			non = append(non, m)
		}
	}
	if storedMsgCount < 0 || storedMsgCount >= len(non) {
		return nil, ErrNoUserMessage{}
	}
	tail := non[storedMsgCount:]
	var systemParts []string
	var queries []string
	for _, m := range tail {
		if m.Role == "system" {
			if t := strings.TrimSpace(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
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
