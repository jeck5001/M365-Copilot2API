package web

import "testing"

func fileTools(names ...string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": name}})
	}
	return out
}

func TestFileToolPolicyKeepsModelChoiceWhenEditExists(t *testing.T) {
	got, _ := applyFileToolPolicy(fileTools("write_file", "edit_file"), "auto")
	if len(got) != 2 {
		t.Fatalf("expected both tools, got %#v", got)
	}
}

func TestFileToolPolicyForcesWriteSetWithoutEdit(t *testing.T) {
	got, _ := applyFileToolPolicy(fileTools("write_file", "read_file"), "auto")
	if len(got) != 1 || toolMapFunctionName(got[0]) != "write_file" {
		t.Fatalf("expected write only, got %#v", got)
	}
}

func TestFileToolPolicyLeavesNonFileToolsUntouched(t *testing.T) {
	got, _ := applyFileToolPolicy(fileTools("get_weather"), "auto")
	if len(got) != 1 || toolMapFunctionName(got[0]) != "get_weather" {
		t.Fatalf("unexpected tools %#v", got)
	}
}
