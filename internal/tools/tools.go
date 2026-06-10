package tools

import (
	"context"
	"fmt"
)

// ToolHandler 工具处理函数
type ToolHandler func(ctx context.Context, arguments map[string]interface{}) (string, error)

// ToolRegistry 工具注册表
type ToolRegistry struct {
	tools map[string]*RegisteredTool
}

type RegisteredTool struct {
	Name        string
	Description string
	Handler     ToolHandler
}

func NewRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools: make(map[string]*RegisteredTool),
	}
}

// Register 注册一个新工具
func (r *ToolRegistry) Register(name, description string, handler ToolHandler) {
	r.tools[name] = &RegisteredTool{
		Name:        name,
		Description: description,
		Handler:     handler,
	}
}

// Execute 执行工具
func (r *ToolRegistry) Execute(ctx context.Context, name string, arguments map[string]interface{}) (string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return "", fmt.Errorf("unknown tool: %s", name)
	}
	return tool.Handler(ctx, arguments)
}

// GetTool 获取工具信息
func (r *ToolRegistry) GetTool(name string) (*RegisteredTool, bool) {
	tool, ok := r.tools[name]
	return tool, ok
}

// ListTools 列出所有已注册的工具
func (r *ToolRegistry) ListTools() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

// === 内置工具示例 ===

// RegisterBuiltins 注册内置工具
func RegisterBuiltins(registry *ToolRegistry) {
	// 示例：搜索工具
	registry.Register("search", "Search for information", func(ctx context.Context, args map[string]interface{}) (string, error) {
		query, _ := args["query"].(string)
		return fmt.Sprintf("Search results for: %s", query), nil
	})

	// 示例：代码执行工具
	registry.Register("run_code", "Execute code", func(ctx context.Context, args map[string]interface{}) (string, error) {
		code, _ := args["code"].(string)
		return fmt.Sprintf("Executed: %s", code), nil
	})

	// 示例：文件读取工具
	registry.Register("read_file", "Read a file", func(ctx context.Context, args map[string]interface{}) (string, error) {
		path, _ := args["path"].(string)
		return fmt.Sprintf("File content from: %s", path), nil
	})
}
