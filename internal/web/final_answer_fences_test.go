package web

import "testing"

// summaryQuotingCommands is the shape of a final report: prose that documents
// which commands to run, in fenced blocks, with no intent to run anything.
const summaryQuotingCommands = "## 测试状态\n\n" +
	"`internal/web` 的失败不是断言失败。\n\n" +
	"验收标准:\n\n" +
	"```bash\ngo test ./internal/web -count=10\ngo test ./... -count=3\n```\n\n" +
	"### P1: 验收当前未提交改动\n"

// The parser cannot tell a documented command from a requested one: once the
// client declares a shell tool, a bash fence always becomes a call, even in a
// summary that is only quoting the command. That is the whole reason the caller
// has to decide by turn, so this pins the behaviour the guard depends on.
//
// Undeclared fences are a separate matter: since issue #12 they are left as
// text (see TestFencedBashNotConvertedWhenUndeclared), so the guard only has
// to earn its keep for clients that did declare the tool.
func TestFencedBashInProseBecomesAToolCall(t *testing.T) {
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "bash"}}}
	calls := fencedToolCalls(summaryQuotingCommands, tools, "auto")
	if len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("got %d calls %#v, want one bash call", len(calls), calls)
	}
}

// The guard's input. A conversation that already carries a tool result is on
// its reporting turn, so the handler stops parsing fences there. Before any
// tool has run the ledger is empty and fenced parsing still bootstraps a call.
func TestLedgerCompletedMarksTheReportingTurn(t *testing.T) {
	fresh := buildAgentLedger([]oaiMsg{{Role: "user", Content: "分析下当前项目"}})
	if len(fresh.Completed) != 0 {
		t.Fatalf("no tool has run yet, got Completed=%d", len(fresh.Completed))
	}

	afterTool := buildAgentLedger([]oaiMsg{
		{Role: "user", Content: "分析下当前项目"},
		{Role: "assistant", ToolCalls: []map[string]any{{
			"id": "c1", "type": "function",
			"function": map[string]any{"name": "bash", "arguments": `{"command":"go test ./..."}`},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "FAIL internal/web"},
	})
	if len(afterTool.Completed) == 0 {
		t.Fatal("a completed tool call must mark the turn as reporting")
	}
}
