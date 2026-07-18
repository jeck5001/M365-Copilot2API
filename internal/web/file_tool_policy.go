package web

import "strings"

// applyFileToolPolicy keeps the model in charge of choosing edit vs full write.
// If no edit-capable tool is exposed, only full-write tools remain available.
func applyFileToolPolicy(tools []map[string]any, choice any) ([]map[string]any, any) {
	var hasEdit, hasWrite bool
	for _, t := range tools {
		name := toolMapFunctionName(t)
		hasEdit = hasEdit || isFileEditTool(name)
		hasWrite = hasWrite || isFileWriteTool(name)
	}
	if !hasWrite || hasEdit {
		return tools, choice
	}
	filtered := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if isFileWriteTool(toolMapFunctionName(t)) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		return tools, choice
	}
	return filtered, choice
}

func toolMapFunctionName(t map[string]any) string {
	f, _ := t["function"].(map[string]any)
	name, _ := f["name"].(string)
	return strings.ToLower(strings.TrimSpace(name))
}

func isFileEditTool(name string) bool {
	for _, x := range []string{"edit_file", "apply_patch", "patch_file", "file_edit", "astrbot_file_edit_tool"} {
		if name == x || strings.Contains(name, x) {
			return true
		}
	}
	return false
}

func isFileWriteTool(name string) bool {
	for _, x := range []string{"write_file", "create_file", "file_write", "astrbot_file_write_tool"} {
		if name == x || strings.Contains(name, x) {
			return true
		}
	}
	return false
}
