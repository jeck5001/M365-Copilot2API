package web

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// replayInnerStream drives the Responses translation loop over a canned inner
// chat SSE stream, standing in for a live ChatHub turn.
func replayInnerStream(t *testing.T, w *httptest.ResponseRecorder, inner string) {
	t.Helper()
	done := make(chan struct{})
	close(done)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader("{}"))
	translateChatStreamToResponses(w, req, "gpt-5", oaiReq{}, strings.NewReader(inner), done, func() int { return 200 })
}

// sseEvents parses an SSE body into (eventName, payload) pairs.
func sseEvents(t *testing.T, body string) []struct {
	Name    string
	Payload map[string]any
} {
	t.Helper()
	var out []struct {
		Name    string
		Payload map[string]any
	}
	name := ""
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			var p map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &p) != nil {
				continue
			}
			out = append(out, struct {
				Name    string
				Payload map[string]any
			}{name, p})
		}
	}
	return out
}

// A ChatHub turn that dies partway emits its deltas and then an error frame.
// Reporting the partial text as response.completed made a truncated answer look
// finished, which is how a cut-off mid-sentence reply reached the client.
func TestResponsesStreamReportsUpstreamErrorAfterPartialText(t *testing.T) {
	inner := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"这是一句话判断"}}]}`,
		`data: {"error":{"message":"upstream request failed"}}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	rr := httptest.NewRecorder()
	replayInnerStream(t, rr, inner)

	events := sseEvents(t, rr.Body.String())
	var names []string
	for _, e := range events {
		names = append(names, e.Name)
	}
	for _, n := range names {
		if n == "response.completed" {
			t.Fatalf("truncated turn reported as completed: %v", names)
		}
	}
	last := events[len(events)-1]
	if last.Name != "response.failed" {
		t.Fatalf("last event = %q, want response.failed (%v)", last.Name, names)
	}
	resp, _ := last.Payload["response"].(map[string]any)
	errObj, _ := resp["error"].(map[string]any)
	if errObj["message"] != "upstream request failed" {
		t.Fatalf("error payload = %#v", errObj)
	}
}

// A healthy stream must still complete normally.
func TestResponsesStreamCompletesWithoutErrorFrame(t *testing.T) {
	inner := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant","content":"hello"}}]}`,
		`data: [DONE]`,
		"",
	}, "\n\n")

	rr := httptest.NewRecorder()
	replayInnerStream(t, rr, inner)

	events := sseEvents(t, rr.Body.String())
	if len(events) == 0 || events[len(events)-1].Name != "response.completed" {
		var names []string
		for _, e := range events {
			names = append(names, e.Name)
		}
		t.Fatalf("events=%v want trailing response.completed", names)
	}
}
