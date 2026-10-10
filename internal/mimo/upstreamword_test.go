package mimo

import (
	"errors"
	"strings"
	"testing"
)

// ADR 0033: whatever the platform said about a refusal is preserved verbatim, so a
// caller reporting it can quote the platform instead of paraphrasing it.
func TestUpstreamMessageKeepsThePlatformsOwnWords(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"in-band busy frame", classifyInBandError("服务器繁忙，请稍后再试"), "服务器繁忙，请稍后再试"},
		{"in-band model frame", classifyInBandError("模型名称错误"), "模型名称错误"},
		{"unknown in-band message", classifyInBandError("some new refusal"), "some new refusal"},
		{"content filter", &ErrContentFilter{Reason: "sensitive_query", Message: "topic not allowed"},
			"sensitive_query"},
		{"plain sentinel", ErrUpstreamBusy, ""},
		{"unrelated error", errors.New("boom"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := UpstreamMessage(tc.err)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("UpstreamMessage = %q, want empty", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("UpstreamMessage = %q, want it to carry %q", got, tc.want)
			}
		})
	}
}

// The content filter's own message names the event and the platform's payload, in
// that order, and stays readable when the platform sent no payload at all.
func TestErrContentFilterMessage(t *testing.T) {
	full := (&ErrContentFilter{Reason: "sensitive_title", Message: " 标题不合规 "}).Error()
	if !strings.Contains(full, "sensitive_title") || !strings.Contains(full, "标题不合规") {
		t.Errorf("Error() = %q, want the event name and the payload", full)
	}
	bare := (&ErrContentFilter{Reason: "sensitive_query"}).Error()
	if strings.Contains(bare, ": ") && strings.HasSuffix(bare, ": ") {
		t.Errorf("Error() = %q, want no dangling separator when the payload is empty", bare)
	}
	if !strings.Contains(bare, "sensitive_query") {
		t.Errorf("Error() = %q, want the event name", bare)
	}
}
