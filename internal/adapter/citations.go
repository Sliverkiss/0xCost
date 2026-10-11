package adapter

// Web-search citations (M19): the upstream returns search results inline in the
// stream (`event:web_search` with up to twelve entries) and used to have them
// dropped silently. They surface now as OpenAI `annotations` on the message plus
// a gateway extension `citations` array, so a client can render sources without
// parsing a vendor-specific event.
//
// The upstream also sends each result's full page text (`text`, tens of KB).
// That is deliberately NOT carried here: it would blow up any client's context
// budget, so it stays upstream and is only summarised in the debug log.

// Citation is one web-search result.
type Citation struct {
	Index      int    `json:"index"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url"`
	DisplayURL string `json:"display_url,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	// Used mirrors the upstream's usedContent flag: the model actually read
	// this page rather than just being shown the title.
	Used bool `json:"used,omitempty"`
}

// Annotation is the OpenAI annotation object attached to a message.
type Annotation struct {
	Type        string      `json:"type"`
	URLCitation URLCitation `json:"url_citation"`
}

// URLCitation is the url_citation body. The index fields are pointers so they
// are omitted when the upstream gives us no position — an invented span would
// be worse than an absent one.
type URLCitation struct {
	URL        string `json:"url"`
	Title      string `json:"title,omitempty"`
	Snippet    string `json:"snippet,omitempty"`
	StartIndex *int   `json:"start_index,omitempty"`
	EndIndex   *int   `json:"end_index,omitempty"`
}

// Annotations converts citations into OpenAI annotations, preserving order.
// Returns nil for an empty input so callers can omit the field entirely.
func Annotations(cits []Citation) []Annotation {
	if len(cits) == 0 {
		return nil
	}
	out := make([]Annotation, 0, len(cits))
	for _, c := range cits {
		out = append(out, Annotation{
			Type: "url_citation",
			URLCitation: URLCitation{
				URL:     c.URL,
				Title:   c.Title,
				Snippet: c.Snippet,
			},
		})
	}
	return out
}
