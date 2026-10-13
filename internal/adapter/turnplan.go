package adapter

import (
	"fmt"
	"strings"
)

// TurnPlan is the sequence of user queries to send upstream for one client
// request (SDD §5.3). There is only one mode: full chain replay — one upstream
// call per user message, chained via previousDialogueId. System messages
// splice into the first user query as a preamble (API-017); they are never
// dropped. Assistant history is not re-sent (accepted trade-off, SDD §5.3B).
type TurnPlan struct {
	Queries []string
	// Media[i] are the image URLs that belong to Queries[i] (IMG-006): an image
	// travels with the turn whose message carried it and is uploaded on that turn
	// only, because the channel attaches files to a single query.
	Media [][]MediaRef
}

// DefaultImageOnlyQuery is the query sent for a user message that carries images
// but no text. The channel has no way to send an attachment on its own ("query"
// is required), so a synthesis is unavoidable; it is deliberately neutral, and
// documented in SDD §5.2 rather than hidden — the alternative is refusing a
// request OpenAI clients consider legal.
const DefaultImageOnlyQuery = "请描述这张图片。"

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
	type userTurn struct {
		content string
		media   []MediaRef
	}
	var userMsgs []userTurn
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if t := strings.TrimSpace(m.Content); t != "" {
				systemParts = append(systemParts, t)
			}
		case "user":
			// A message with images but no text is a legal request (IMG-007):
			// it enters the plan with the neutral query below, not a 400.
			if m.Content != "" || len(m.Media) > 0 {
				userMsgs = append(userMsgs, userTurn{content: m.Content, media: m.Media})
			}
		}
	}
	if len(userMsgs) == 0 {
		return nil, ErrNoUserMessage{}
	}
	if maxTurns > 0 && len(userMsgs) > maxTurns {
		return nil, &ErrTooManyTurns{Turns: len(userMsgs), Cap: maxTurns}
	}
	plan := &TurnPlan{
		Queries: make([]string, 0, len(userMsgs)),
		Media:   make([][]MediaRef, 0, len(userMsgs)),
	}
	for i, t := range userMsgs {
		q := t.content
		if len(systemParts) > 0 && i == 0 {
			q = strings.Join(systemParts, "\n\n") + "\n\n---\n\n" + q
		}
		if q == "" && len(t.media) > 0 {
			q = DefaultImageOnlyQuery
		}
		plan.Queries = append(plan.Queries, q)
		plan.Media = append(plan.Media, t.media)
	}
	return plan, nil
}
