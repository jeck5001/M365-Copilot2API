package web

import (
	"encoding/json"
	"fmt"
	"strings"
)

func modelToolRouterPrompt(prompt string, tools []map[string]any, choice any) string {
	if idx := strings.Index(prompt, "[user]"); idx >= 0 {
		prompt = prompt[idx:]
	} else if idx := strings.Index(prompt, "\n[user]"); idx >= 0 {
		prompt = prompt[idx+1:]
	}
	prompt = compactToolResult(prompt, 6000)
	defs, _ := json.Marshal(tools)
	mode := normalizedToolChoiceMode(choice)
	rules := `- If a tool is needed (such as inspecting files, executing commands, checking directories or running tests), respond with: CALL_TOOL: tool_name({"arg1":"value1"})
- If all information is collected and you are ready to answer the user, respond with: NO_TOOL_NEEDED
- Only use tools from the available list above
- Validate all arguments against the tool's schema
- Do not invent tools that are not in the list
- For exploration, analysis, or inspection tasks, continue calling tools until you have gathered all necessary information and files. Do NOT output a plan or intention to check something next without calling the tool; call the tool immediately using CALL_TOOL:.
- Never output explanatory text before CALL_TOOL: or NO_TOOL_NEEDED`
	if strings.Contains(prompt, "tool_calls:") || strings.Contains(prompt, "tool[call_") || strings.Contains(prompt, "EVIDENCE_SUMMARY") {
		rules += `
- Completed evidence must not be repeated: completed tool calls are already executed, never re-invoke identical calls
- Only start a new tool call when fresh unfinished work remains on the current request
- If you still need to inspect more files or verify more details to fulfill the request, call the tool now`
	}
	return fmt.Sprintf(`You are a tool execution planner. Based on the user request, decide which tool to call next.

Available tools: %s

MODE: %s

Rules:
%s

User request and context:
%s`, defs, mode, rules, prompt)
}

func parseModelToolDecision(text string, tools []map[string]any, choice any) ([]detectedToolCall, bool) {
	text = strings.TrimSpace(text)
	upper := strings.ToUpper(text)
	if idx := strings.Index(upper, "CALL_TOOL:"); idx >= 0 {
		rest := strings.TrimSpace(text[idx+len("CALL_TOOL:"):])
		start := strings.Index(rest, "(")
		end := strings.LastIndex(rest, ")")
		if start > 0 && end > start {
			name := strings.TrimSpace(rest[:start])
			argsStr := rest[start+1 : end]
			var args map[string]any
			if json.Unmarshal([]byte(argsStr), &args) == nil && toolChoiceAllows(choice, name) {
				fn := toolFunction(name, tools)
				if fn != nil && schemaValid(args, fn) == nil {
					b, _ := json.Marshal(args)
					return []detectedToolCall{{ID: callID(name, string(b), 0), Type: toolType(name, tools), Name: name, Arguments: b}}, true
				}
			}
		}
	}
	if strings.Contains(text, "NO_TOOL_NEEDED") || strings.Contains(text, "no_tool_needed") {
		return nil, true
	}
	if fenced := fencedToolCalls(text, tools, choice); len(fenced) > 0 {
		return fenced, true
	}
	// Fallback: try the old JSON format
	if i := strings.Index(text, "```"); i >= 0 {
		text = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(text[i+3:], "```"), "json"))
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return nil, false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal([]byte(text[start:end+1]), &probe) != nil {
		return nil, false
	}
	if _, ok := probe["calls"]; !ok {
		return nil, false
	}
	var envelope struct {
		Calls []struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"calls"`
	}
	if json.Unmarshal([]byte(text[start:end+1]), &envelope) != nil {
		return nil, false
	}
	out := make([]detectedToolCall, 0, len(envelope.Calls))
	for i, c := range envelope.Calls {
		fn := toolFunction(c.Name, tools)
		if fn == nil || c.Arguments == nil || !toolChoiceAllows(choice, c.Name) || schemaValid(c.Arguments, fn) != nil {
			continue
		}
		b, _ := json.Marshal(c.Arguments)
		out = append(out, detectedToolCall{ID: callID(c.Name, string(b), i), Type: toolType(c.Name, tools), Name: c.Name, Arguments: b})
	}
	return out, true
}
