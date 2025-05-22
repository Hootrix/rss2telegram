package rss

//formatMessage时支持的模版操作符高级特性

import (
	"fmt"
	"log"
	"regexp"
	"strings"
)

// Operation 定义模板操作接口
type Operation interface {
	Process(content string, params string) string
}

// OperationRegistry 操作注册表
type OperationRegistry struct {
	operations map[string]Operation
}

// TemplateProcessor 模板处理器
type TemplateProcessor struct {
	registry *OperationRegistry
}

// NewTemplateProcessor 创建新的模板处理器
func NewTemplateProcessor() *TemplateProcessor {
	registry := &OperationRegistry{
		operations: make(map[string]Operation),
	}

	// 注册基础操作 操作符
	registry.Register("extract", &ExtractOperation{})
	registry.Register("extract-all", &ExtractAllOperation{})
	registry.Register("replace", &ReplaceOperation{})
	registry.Register("default", &DefaultOperation{})
	registry.Register("prefix", &PrefixOperation{})

	return &TemplateProcessor{registry: registry}
}

// Register 注册新的操作
func (r *OperationRegistry) Register(name string, op Operation) {
	r.operations[name] = op
}

// ExtractOperation 提取操作
type ExtractOperation struct{}

func (op *ExtractOperation) Process(content string, params string) string {
	pattern := params
	re, err := regexp.Compile(pattern)
	if err != nil {
		log.Printf("Invalid regex pattern: %v", err)
		return content
	}
	matches := re.FindStringSubmatch(content)
	if len(matches) > 1 {
		// 如果有捕获组，返回第一个捕获组
		return matches[1]
	}
	if len(matches) == 1 {
		// 如果没有捕获组但有匹配，返回整个匹配
		return matches[0]
	}
	// 如果没有匹配，返回空字符串
	return ""
}

// ExtractAllOperation 提取所有匹配操作
const ExtractAllOperationGap = "<||4623456fdb0d55bc037afa5c25f08cd7||>"

type ExtractAllOperation struct{}

func (op *ExtractAllOperation) Process(content string, params string) string {
	// 只处理正则表达式，前缀由 prefix 操作符处理
	pattern := params

	re, err := regexp.Compile(pattern)
	if err != nil {
		log.Printf("Invalid regex pattern: %v", err)
		return content
	}

	// 提取所有匹配结果
	var allMatches []string

	// 使用FindAllStringSubmatch获取所有匹配
	matches := re.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return ""
	}

	for _, match := range matches {
		if len(match) > 1 {
			// 如果有捕获组，添加第一个捕获组
			allMatches = append(allMatches, match[1])
		} else {
			// 如果没有捕获组，添加整个匹配
			allMatches = append(allMatches, match[0])
		}
	}
	return strings.Join(allMatches, ExtractAllOperationGap)
}

// PrefixOperation 前缀操作
type PrefixOperation struct{}

func (op *PrefixOperation) Process(content string, prefix string) string {
	// 如果内容为空，直接返回空字符串
	if content == "" {
		return ""
	}

	// 如果内容包含多个项（由两个空格分隔）
	items := strings.Split(content, ExtractAllOperationGap)
	for i, item := range items {
		items[i] = prefix + item
	}

	// 重新用两个空格连接
	return strings.Join(items, "  ")
}

// ReplaceOperation 替换操作
type ReplaceOperation struct{}

func (op *ReplaceOperation) Process(content string, params string) string {
	parts := strings.SplitN(params, ":", 2)
	if len(parts) != 2 {
		return content
	}

	// pattern := unescapeParams(parts[0])
	pattern := parts[0]
	replacement := parts[1] // 不对替换部分进行unescape，因为可能包含 $1 这样的引用

	re, err := regexp.Compile(pattern)
	if err != nil {
		log.Printf("Invalid regex pattern: %v", err)
		return content
	}
	result := re.ReplaceAllString(content, replacement)
	return result
}

// DefaultOperation 默认值操作
type DefaultOperation struct{}

func (op *DefaultOperation) Process(content string, defaultValue string) string {
	if content == "" {
		return defaultValue
	}
	return content
}

// ProcessField 处理模板字段
func (p *TemplateProcessor) ProcessField(field, content string) string {
	operations := splitEscaped(field, '|')
	if len(operations) == 1 {
		return content
	}

	result := content
	// 第一个是字段名，从第二个开始是操作
	for _, op := range operations[1:] {
		parts := strings.SplitN(op, ":", 2)
		if len(parts) < 2 {
			continue
		}

		opName := strings.TrimSpace(parts[0])
		params := parts[1] // 保留原始空格，因为在正则表达式中可能有意义

		if operation, exists := p.registry.operations[opName]; exists {
			result = operation.Process(result, params)
		}
	}

	return result
}

// splitEscaped 分割字符串，处理转义字符, 之后还原转义字符
func splitEscaped(s string, sep byte) []string {
	interSymbol := fmt.Sprintf("<===%%%X%%===>", sep)
	str := strings.ReplaceAll(s, `\`+string(sep), interSymbol)

	result := strings.Split(str, string(sep))
	for i, str := range result {
		result[i] = strings.ReplaceAll(str, interSymbol, `\`+string(sep))
	}
	return result
}
