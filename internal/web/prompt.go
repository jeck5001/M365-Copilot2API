package web

import (
	"fmt"
	"m365-copilot2api/internal/chathub"
	"strings"
)

func flattenPromptMessages(messages []oaiMsg, attachments []chathub.Attachment) (string, []chathub.Attachment) {
	var systemParts []string
	var rest []oaiMsg
	for _, m := range messages {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "system" || role == "developer" {
			txt, sysFiles := parseContent(m.Content)
			attachments = append(attachments, sysFiles...)
			txt = strings.TrimSpace(txt)
			if txt != "" {
				systemParts = append(systemParts, txt)
			}
		} else {
			rest = append(rest, m)
		}
	}
	var b strings.Builder
	if len(systemParts) > 0 {
		b.WriteString("\n[system]\n")
		b.WriteString(strings.Join(systemParts, "\n"))
		b.WriteString("\n")
	}
	totalRest := len(rest)
	for i, m := range rest {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" {
			role = "user"
		}
		content := m.Content
		if role == "tool" {
			switch v := content.(type) {
			case nil:
				content = ""
			case string:
			default:
				content = mustJSON(v)
			}
		}
		txt, files := parseContent(content)
		attachments = append(attachments, files...)
		txt = strings.TrimSpace(txt)
		if len(m.ToolCalls) > 0 {
			if txt != "" {
				b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
			}
			b.WriteString(fmt.Sprintf("\n[%s tool_calls]\n%s\n", role, mustJSON(m.ToolCalls)))
			continue
		}
		if role == "tool" {
			toolLimit := maxToolResultBytes()
			if i < totalRest-2 {
				toolLimit = 2000
			}
			txt = compactToolResult(txt, toolLimit)
			b.WriteString(fmt.Sprintf("\n[tool result id=%s]\n%s\n", m.ToolCallID, txt))
			continue
		}
		if txt == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("\n[%s]\n%s\n", role, txt))
	}
	res := strings.TrimSpace(b.String())
	const maxTotalPromptChars = 45000
	if len(res) > maxTotalPromptChars {
		head := 15000
		tail := maxTotalPromptChars - head - 120
		res = res[:head] + fmt.Sprintf("\n... [history truncated %d chars] ...\n", len(res)-head-tail) + res[len(res)-tail:]
	}
	return res, attachments
}
