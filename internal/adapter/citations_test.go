package adapter_test

// Citation shape mapping (M19): citations must appear as OpenAI
// `annotations` on the message plus the gateway's own top-level `citations`
// array, and must be absent entirely when there are none — an empty array
// would tell a client "there were sources, we just lost them".

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yg/mimo2api/internal/adapter"
)

var twoCits = []adapter.Citation{
	{Index: 0, Title: "One", URL: "https://one.test", DisplayURL: "one.test", Snippet: "first", Used: true},
	{Index: 1, Title: "Two", URL: "https://two.test"},
}

// CITE-001: annotations preserve order and carry url/title/snippet, with no
// invented character spans.
func TestCite001Annotations(t *testing.T) {
	if got := adapter.Annotations(nil); got != nil {
		t.Errorf("Annotations(nil) = %v, want nil (field omitted)", got)
	}
	anns := adapter.Annotations(twoCits)
	if len(anns) != 2 {
		t.Fatalf("annotations = %d, want 2", len(anns))
	}
	if anns[0].Type != "url_citation" || anns[0].URLCitation.URL != "https://one.test" {
		t.Errorf("annotation[0] = %+v, want url_citation https://one.test", anns[0])
	}
	if anns[1].URLCitation.Title != "Two" {
		t.Errorf("annotation[1].Title = %q, want Two (order preserved)", anns[1].URLCitation.Title)
	}
	for i, a := range anns {
		if a.URLCitation.StartIndex != nil || a.URLCitation.EndIndex != nil {
			t.Errorf("annotation[%d] has an invented span: %+v", i, a.URLCitation)
		}
	}
}

// CITE-002: the non-streaming body carries both the annotations and the
// top-level citations array.
func TestCite002CompletionCarriesCitations(t *testing.T) {
	var usage adapter.Usage
	comp := adapter.ShapeCompletion("id-1", "mimo-v2.6-flash", 1, "answer", "", usage, twoCits...)
	raw, err := json.Marshal(comp)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"annotations":[{"type":"url_citation"`) {
		t.Errorf("body = %s, want message annotations", body)
	}
	if !strings.Contains(body, `"citations":[{"index":0`) {
		t.Errorf("body = %s, want top-level citations", body)
	}
	if !strings.Contains(body, `"display_url":"one.test"`) {
		t.Errorf("body = %s, want display_url preserved", body)
	}
	// Without citations both fields disappear.
	plain, _ := json.Marshal(adapter.ShapeCompletion("id-1", "mimo-v2.6-flash", 1, "answer", "", usage))
	if strings.Contains(string(plain), "annotations") || strings.Contains(string(plain), "citations") {
		t.Errorf("body = %s, want no citation fields when there are none", plain)
	}
}

// CITE-003: the streaming citations chunk is a legal OpenAI chunk — empty
// choices, so a standard client ignores it — and it carries the sources.
func TestCite003CitationsChunk(t *testing.T) {
	ch := adapter.ShapeCitationsChunk("id-1", "mimo-v2.6-flash", 1, twoCits)
	if ch.Object != "chat.completion.chunk" {
		t.Errorf("object = %q, want chat.completion.chunk", ch.Object)
	}
	if ch.Choices == nil || len(ch.Choices) != 0 {
		t.Errorf("choices = %v, want an empty (non-nil) array", ch.Choices)
	}
	encoded := adapter.EncodeChunk(ch)
	if !strings.Contains(encoded, `"choices":[]`) || !strings.Contains(encoded, `"citations":[`) {
		t.Errorf("chunk = %s, want choices:[] plus citations", encoded)
	}
	if strings.Contains(encoded, "annotations") {
		t.Errorf("chunk = %s, want citations only (annotations belong to the message)", encoded)
	}
}
