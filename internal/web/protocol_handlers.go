package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type pipeResponseWriter struct {
	h      http.Header
	w      *io.PipeWriter
	status int
}

func (p *pipeResponseWriter) Header() http.Header { return p.h }
func (p *pipeResponseWriter) WriteHeader(n int) {
	if p.status == 0 {
		p.status = n
	}
}
func (p *pipeResponseWriter) Write(b []byte) (int, error) {
	if p.status == 0 {
		p.status = 200
	}
	return p.w.Write(b)
}
func (p *pipeResponseWriter) Flush() {}

// streamResponsesAdapter converts the internal OpenAI SSE incrementally instead
// of buffering the entire completion in httptest.ResponseRecorder.
func (s *Server) streamResponsesAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string, startedAt time.Time) {
	o.Stream = true
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	pr, pw := io.Pipe()
	defer pr.Close()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	innerDone := make(chan struct{})
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[responses] inner goroutine panic: %v", rec)
			}
			_ = pw.Close()
			close(innerDone)
		}()
		s.openaiChat(irw, r2)
	}()
	tenant := extractAPIKey(r)
	translateChatStreamToResponsesInternal(w, r, model, o, pr, innerDone, func() int { return irw.status }, s, tenant, startedAt)
}

// translateChatStreamToResponses rewrites the internal OpenAI chat SSE in src as
// Responses events. innerDone closes when the producer is finished and
// innerStatus reports the status it wrote, so transport failures can be told
// apart from a turn that streamed cleanly.
func translateChatStreamToResponses(w http.ResponseWriter, r *http.Request, model string, o oaiReq, src io.Reader, innerDone <-chan struct{}, innerStatus func() int) {
	translateChatStreamToResponsesInternal(w, r, model, o, src, innerDone, innerStatus, nil, "", time.Time{})
}

func translateChatStreamToResponsesInternal(w http.ResponseWriter, r *http.Request, model string, o oaiReq, src io.Reader, innerDone <-chan struct{}, innerStatus func() int, server *Server, tenant string, startedAt time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	emit := func(name string, v any) error {
		return writeSSE(r, w, flusher, name, v)
	}
	id := "resp_" + uuid.NewString()
	created := time.Now().Unix()
	emit("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": id, "object": "response", "status": "in_progress", "model": model, "output": []any{}}})

	var text strings.Builder
	messageID := "msg_" + uuid.NewString()
	contentID := "txt_" + uuid.NewString()
	textStarted := false
	type tcState struct {
		ID, Name, Args, Type string
		ItemID               string
	}
	calls := map[int]*tcState{}
	streamErr := ""
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		if r.Context().Err() != nil {
			return
		}
		line := scanner.Text()
		if line == "data: [DONE]" {
			continue
		}
		rawJSON := strings.TrimPrefix(line, "data: ")
		var chunk map[string]any
		if json.Unmarshal([]byte(rawJSON), &chunk) != nil {
			continue
		}
		// A failed upstream turn arrives as an error frame after any deltas it
		// managed to emit. Without capturing it the partial text below is
		// reported as response.completed, so a chat truncated at the timeout
		// looks to the client like a finished answer.
		if errFrame, ok := chunk["error"].(map[string]any); ok {
			streamErr, _ = errFrame["message"].(string)
			if streamErr == "" {
				streamErr = "upstream stream failed"
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			text.WriteString(content)
			if !textStarted {
				textStarted = true
				emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}})
			}
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": content})
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				idxFloat, ok := tc["index"].(float64)
				if !ok {
					continue
				}
				idx := int(idxFloat)
				st := calls[idx]
				typ := "function"
				if v, ok := tc["type"].(string); ok && v == "custom" {
					typ = "custom"
				}
				if st == nil {
					prefix := "fc_"
					item := map[string]any{"type": "function_call", "call_id": "", "name": "", "arguments": "", "status": "in_progress"}
					if typ == "custom" {
						prefix = "ctc_"
						item = map[string]any{"type": "custom_tool_call", "call_id": "", "name": "", "input": "", "status": "in_progress"}
					}
					st = &tcState{ItemID: prefix + uuid.NewString(), Type: typ}
					calls[idx] = st
					item["id"] = st.ItemID
					emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": idx, "item": item})
				}
				if v, ok := tc["id"].(string); ok {
					st.ID = v
				}
				fn, _ := tc["function"].(map[string]any)
				if v, ok := fn["name"].(string); ok {
					st.Name += v
				}
				if v, ok := fn["arguments"].(string); ok {
					st.Args += v
					if st.Type != "custom" {
						emit("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": idx, "item_id": st.ItemID, "delta": v})
					}
				}
			}
		}
	}
	<-innerDone
	if scanner.Err() != nil || innerStatus() >= http.StatusBadRequest {
		status := innerStatus()
		if status == 0 {
			status = http.StatusBadGateway
		}
		errMsg := streamErr
		if errMsg == "" {
			errMsg = "inner chat request failed"
		}
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": status, "message": errMsg},
			},
		})
		return
	}
	if streamErr != "" {
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": "upstream_stream_failed", "message": streamErr},
			},
		})
		return
	}
	if len(calls) == 0 && strings.TrimSpace(text.String()) == "" {
		// Never leave a Responses stream after response.created without a
		// terminal event: clients otherwise render this as a successful blank
		// answer and may reuse an incomplete response on the next turn.
		emit("response.failed", map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"id": id, "object": "response", "status": "failed", "model": model,
				"error": map[string]any{"code": "empty_upstream_response", "message": "ChatHub returned no text or tool call"},
			},
		})
		return
	}
	output := []any{}
	var keys []int
	if len(calls) > 0 {
		keys = make([]int, 0, len(calls))
		for k := range calls {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		for _, i := range keys {
			st := calls[i]
			if st == nil {
				continue
			}
			if st.Type == "custom" {
				input := customToolInput(st.Args)
				item := map[string]any{"type": "custom_tool_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "input": input, "status": "completed"}
				output = append(output, item)
				emit("response.custom_tool_call_input.delta", map[string]any{"type": "response.custom_tool_call_input.delta", "output_index": i, "item_id": item["id"], "delta": input})
				emit("response.custom_tool_call_input.done", map[string]any{"type": "response.custom_tool_call_input.done", "output_index": i, "item_id": item["id"], "input": input})
				emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
				continue
			}
			item := map[string]any{"type": "function_call", "id": st.ItemID, "call_id": st.ID, "name": st.Name, "arguments": st.Args, "status": "completed"}
			output = append(output, item)
			emit("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "output_index": i, "item_id": st.ItemID, "arguments": st.Args})
			emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
	} else {
		item := map[string]any{"type": "message", "id": messageID, "role": "assistant", "status": "in_progress", "content": []any{map[string]any{"type": "output_text", "id": contentID, "text": "", "annotations": []any{}}}}
		output = append(output, item)
		if !textStarted {
			emit("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
			emit("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": messageID, "delta": text.String()})
		}
		emit("response.output_text.done", map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": messageID, "text": text.String()})
		item["status"] = "completed"
		item["content"] = []any{map[string]any{"type": "output_text", "id": contentID, "text": text.String(), "annotations": []any{}}}
		emit("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	}
	usageOutput := text.String()
	for _, call := range calls {
		usageOutput += call.Name + call.Args
	}
	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, usageOutput)
	resp := map[string]any{"id": id, "object": "response", "created_at": created, "status": "completed", "model": model, "output": output, "usage": estimate.Values, "m365": localUsageMetadata(estimate.Source)}
	emit("response.completed", map[string]any{"type": "response.completed", "response": resp})

	if server != nil && len(output) > 0 {
		stored := append([]oaiMsg(nil), o.Messages...)
		if len(calls) > 0 {
			converted := make([]map[string]any, 0, len(calls))
			for _, i := range keys {
				st := calls[i]
				if st != nil {
					typ := "function"
					if st.Type == "custom" {
						typ = "custom"
					}
					converted = append(converted, map[string]any{
						"id":       st.ID,
						"type":     typ,
						"function": map[string]any{"name": st.Name, "arguments": st.Args},
					})
				}
			}
			stored = append(stored, oaiMsg{Role: "assistant", ToolCalls: converted})
		} else if text.Len() > 0 {
			stored = append(stored, oaiMsg{Role: "assistant", Content: text.String()})
		}
		server.saveResponseHistory(tenant, id, stored)
		if server.usage != nil {
			server.usage.record(UsageRecord{
				Time:         time.Now(),
				APIKeyPrefix: tenant,
				Model:        model,
				Endpoint:     "/v1/responses",
				InputTokens:  int64(estimate.Values["input_tokens"].(int)),
				OutputTokens: int64(estimate.Values["output_tokens"].(int)),
				DurationMs:   time.Since(startedAt).Milliseconds(),
				Status:       200,
			})
		}
	}
}

func (s *Server) saveResponseHistory(tenant string, publicID string, messages []oaiMsg) {
	if s == nil || publicID == "" {
		return
	}
	s.responseMu.Lock()
	defer s.responseMu.Unlock()
	bucket := s.responseMessages[tenant]
	if bucket == nil {
		bucket = map[string]respHistory{}
		s.responseMessages[tenant] = bucket
	}
	for k, h := range bucket {
		if time.Since(h.At) > time.Hour {
			delete(bucket, k)
		}
	}
	if len(bucket) >= maxResponsesPerTenant {
		var oldestKey string
		var oldestAt time.Time
		for k, h := range bucket {
			if oldestKey == "" || h.At.Before(oldestAt) {
				oldestKey, oldestAt = k, h.At
			}
		}
		delete(bucket, oldestKey)
	}
	bucket[publicID] = respHistory{At: time.Now(), Messages: messages}
}

func (s *Server) runOpenAIAdapter(r *http.Request, o oaiReq) (map[string]any, []byte, int, error) {
	o.Stream = false
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	rr := httptest.NewRecorder()
	s.openaiChat(rr, r2)
	var out map[string]any
	err := json.Unmarshal(rr.Body.Bytes(), &out)
	return out, rr.Body.Bytes(), rr.Code, err
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeResponsesError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body responsesRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeResponsesError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeResponsesError(w, 400, "invalid_request_error", err.Error())
		return
	}
	tenant := extractAPIKey(r)
	if body.PreviousResponseID != "" {
		s.responseMu.Lock()
		prior, ok := s.responseMessages[tenant][body.PreviousResponseID]
		messages := append([]oaiMsg(nil), prior.Messages...)
		s.responseMu.Unlock()
		if !ok || len(messages) == 0 {
			writeResponsesError(w, 400, "invalid_request_error", "unknown previous_response_id")
			return
		}
		o.Messages = append(messages, o.Messages...)
	}
	if body.Stream {
		s.streamResponsesAdapter(w, r, o, firstNonEmpty(o.Model, "m365-copilot"), startedAt)
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeResponsesError(w, status, "upstream_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "upstream protocol error: "+err.Error())
		return
	}
	if !responsesOutputHasContent(out) {
		writeResponsesError(w, http.StatusBadGateway, "upstream_error", "ChatHub returned an empty response; no reusable message was created")
		return
	}
	msg, _ := openAIChoice(out)
	outputForUsage := ""
	if msg != nil {
		outputForUsage = fmt.Sprint(msg["content"])
		if calls, ok := msg["tool_calls"].([]any); ok {
			outputForUsage += fmt.Sprint(calls)
		}
	}
	estimate := estimateResponsesUsage(firstNonEmpty(o.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, outputForUsage)
	out["usage"] = estimate.Values
	out["m365_usage_source"] = estimate.Source
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        firstNonEmpty(o.Model, "m365-copilot"),
		Endpoint:     "/v1/responses",
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	// Retain the normalized history so a subsequent previous_response_id can
	// validate its function_call_output against the original tool call.
	if _, ok := out["id"].(string); ok {
		// Use the same public response id that writeResponsesResult exposes.
		publicID := "resp_" + uuid.NewString()
		out["m365_response_id"] = publicID
		stored := append([]oaiMsg(nil), o.Messages...)
		if msg, _ := openAIChoice(out); msg != nil {
			if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
				converted := make([]map[string]any, 0, len(calls))
				for _, call := range calls {
					if m, ok := call.(map[string]any); ok {
						converted = append(converted, m)
					}
				}
				stored = append(stored, oaiMsg{Role: "assistant", ToolCalls: converted})
			} else {
				if text, _ := msg["content"].(string); text != "" {
					stored = append(stored, oaiMsg{Role: "assistant", Content: text})
				}
			}
		}
		s.saveResponseHistory(tenant, publicID, stored)
	}
	writeResponsesResult(w, firstNonEmpty(o.Model, "m365-copilot"), body.Stream, out)
}

func responsesOutputHasContent(src map[string]any) bool {
	msg, _ := openAIChoice(src)
	if msg == nil {
		return false
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	text, _ := msg["content"].(string)
	return strings.TrimSpace(text) != ""
}

func (s *Server) streamAnthropicAdapter(w http.ResponseWriter, r *http.Request, o oaiReq, model string, startedAt time.Time) {
	o.Stream = true
	b, _ := json.Marshal(o)
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	pr, pw := io.Pipe()
	defer pr.Close()
	irw := &pipeResponseWriter{h: make(http.Header), w: pw}
	innerDone := make(chan struct{})
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[anthropic-stream] inner goroutine panic: %v", rec)
			}
			_ = pw.Close()
			close(innerDone)
		}()
		s.openaiChat(irw, r2)
	}()
	translateChatStreamToAnthropic(w, r, model, o, pr, innerDone, func() int { return irw.status }, s.usage, startedAt)
}

func translateChatStreamToAnthropic(w http.ResponseWriter, r *http.Request, model string, o oaiReq, src io.Reader, innerDone <-chan struct{}, innerStatus func() int, usage *usageLog, startedAt time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)
	emit := func(name string, v any) error {
		return writeSSE(r, w, flusher, name, v)
	}
	id := "msg_" + uuid.NewString()

	estimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, "")
	inputTokens := int64(estimate.Values["input_tokens"].(int))

	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":                inputTokens,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})

	var text strings.Builder
	var reasoning strings.Builder
	textStarted := false
	reasoningStarted := false
	textBlockIndex := 0
	reasoningBlockIndex := 0
	nextBlockIndex := 0

	type anthropicToolCallState struct {
		BlockIndex int
		ID         string
		Name       string
		Args       strings.Builder
		Started    bool
	}
	calls := map[int]*anthropicToolCallState{}
	streamErr := ""
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 4096), 2<<20)

	for scanner.Scan() {
		if r.Context().Err() != nil {
			return
		}
		line := scanner.Text()
		if line == "data: [DONE]" {
			continue
		}
		rawJSON := strings.TrimPrefix(line, "data: ")
		var chunk map[string]any
		if json.Unmarshal([]byte(rawJSON), &chunk) != nil {
			continue
		}
		if errFrame, ok := chunk["error"].(map[string]any); ok {
			streamErr, _ = errFrame["message"].(string)
			if streamErr == "" {
				streamErr = "upstream stream failed"
			}
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		delta, _ := choice["delta"].(map[string]any)

		if reasonContent, ok := delta["reasoning_content"].(string); ok && reasonContent != "" {
			reasoning.WriteString(reasonContent)
			if !reasoningStarted {
				reasoningStarted = true
				reasoningBlockIndex = nextBlockIndex
				nextBlockIndex++
				emit("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         reasoningBlockIndex,
					"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": reasoningBlockIndex,
				"delta": map[string]any{"type": "thinking_delta", "thinking": reasonContent},
			})
		}

		if content, ok := delta["content"].(string); ok && content != "" {
			text.WriteString(content)
			if !textStarted {
				if reasoningStarted {
					emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": reasoningBlockIndex})
				}
				textStarted = true
				textBlockIndex = nextBlockIndex
				nextBlockIndex++
				emit("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         textBlockIndex,
					"content_block": map[string]any{"type": "text", "text": ""},
				})
			}
			emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": textBlockIndex,
				"delta": map[string]any{"type": "text_delta", "text": content},
			})
		}

		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for _, raw := range rawCalls {
				tc, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				idxFloat, ok := tc["index"].(float64)
				if !ok {
					continue
				}
				idx := int(idxFloat)
				st := calls[idx]
				if st == nil {
					st = &anthropicToolCallState{BlockIndex: nextBlockIndex}
					nextBlockIndex++
					calls[idx] = st
				}
				if v, ok := tc["id"].(string); ok && v != "" {
					st.ID = v
				}
				fn, _ := tc["function"].(map[string]any)
				if fn != nil {
					if v, ok := fn["name"].(string); ok && v != "" {
						st.Name += v
					}
					if v, ok := fn["arguments"].(string); ok {
						st.Args.WriteString(v)
						if !st.Started {
							st.Started = true
							if st.ID == "" {
								st.ID = "call_" + uuid.NewString()
							}
							emit("content_block_start", map[string]any{
								"type":          "content_block_start",
								"index":         st.BlockIndex,
								"content_block": map[string]any{"type": "tool_use", "id": st.ID, "name": st.Name, "input": map[string]any{}},
							})
						}
						emit("content_block_delta", map[string]any{
							"type":  "content_block_delta",
							"index": st.BlockIndex,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": v},
						})
					}
				}
			}
		}
	}

	<-innerDone
	if scanner.Err() != nil || innerStatus() >= http.StatusBadRequest {
		status := innerStatus()
		if status == 0 {
			status = http.StatusBadGateway
		}
		errMsg := streamErr
		if errMsg == "" {
			errMsg = "inner chat request failed"
		}
		emit("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": errMsg},
		})
		return
	}
	if streamErr != "" {
		emit("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": streamErr},
		})
		return
	}

	if reasoningStarted && !textStarted && len(calls) == 0 {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": reasoningBlockIndex})
	}
	if textStarted {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": textBlockIndex})
	}

	toolKeys := make([]int, 0, len(calls))
	for k := range calls {
		toolKeys = append(toolKeys, k)
	}
	sort.Ints(toolKeys)
	for _, k := range toolKeys {
		st := calls[k]
		if st == nil {
			continue
		}
		if !st.Started {
			if st.ID == "" {
				st.ID = "call_" + uuid.NewString()
			}
			emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         st.BlockIndex,
				"content_block": map[string]any{"type": "tool_use", "id": st.ID, "name": st.Name, "input": map[string]any{}},
			})
			if st.Args.Len() > 0 {
				emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": st.BlockIndex,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": st.Args.String()},
				})
			}
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": st.BlockIndex})
	}

	stopReason := "end_turn"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}

	outputContent := text.String()
	for _, k := range toolKeys {
		if st := calls[k]; st != nil {
			outputContent += st.Name + st.Args.String()
		}
	}
	outEstimate := estimateResponsesUsage(model, o.Messages, o.Tools, o.ToolChoice, outputContent)
	outputTokens := int64(outEstimate.Values["output_tokens"].(int))

	emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": outputTokens,
		},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})

	if usage != nil {
		usage.record(UsageRecord{
			Time:         time.Now(),
			APIKeyPrefix: extractAPIKey(r),
			Model:        model,
			Endpoint:     "/v1/messages",
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			DurationMs:   time.Since(startedAt).Milliseconds(),
			Status:       200,
		})
	}
}

func (s *Server) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	startedAt := time.Now()
	if r.Method != http.MethodPost {
		writeAnthropicError(w, 405, "invalid_request_error", "method not allowed")
		return
	}
	var body anthropicRequest
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		writeAnthropicError(w, 400, "invalid_request_error", "bad json")
		return
	}
	o, err := body.openAI()
	if err != nil {
		writeAnthropicError(w, 400, "invalid_request_error", err.Error())
		return
	}
	if body.Stream {
		s.streamAnthropicAdapter(w, r, o, firstNonEmpty(o.Model, "m365-copilot"), startedAt)
		return
	}
	out, raw, status, err := s.runOpenAIAdapter(r, o)
	if status >= 400 {
		writeAnthropicError(w, status, "api_error", errorMessage(raw, "upstream protocol error"))
		return
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream protocol error: "+err.Error())
		return
	}
	estimate := estimateResponsesUsage(firstNonEmpty(o.Model, "m365-copilot"), o.Messages, o.Tools, o.ToolChoice, "")
	s.usage.record(UsageRecord{
		Time:         time.Now(),
		APIKeyPrefix: extractAPIKey(r),
		Model:        firstNonEmpty(o.Model, "m365-copilot"),
		Endpoint:     "/v1/messages",
		InputTokens:  int64(estimate.Values["input_tokens"].(int)),
		OutputTokens: int64(estimate.Values["output_tokens"].(int)),
		DurationMs:   time.Since(startedAt).Milliseconds(),
		Status:       200,
	})
	writeAnthropicResult(w, firstNonEmpty(o.Model, "m365-copilot"), body.Stream, out)
}
