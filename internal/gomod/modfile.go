package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

var (
	// ErrToolNotFound 表示工具未在 got.mod 中声明。
	ErrToolNotFound = errors.New("tool not found in got.mod")
	// ErrAmbiguousTool 表示短名匹配到多个不同的工具路径。
	ErrAmbiguousTool = errors.New("ambiguous tool name")
)

// AmbiguousToolError 描述工具短名存在多个冲突路径时的详细错误。
type AmbiguousToolError struct {
	Name    string
	Matches []string
}

func (e *AmbiguousToolError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "tool %q is ambiguous; matches multiple tool paths in got.mod:\n", e.Name)
	for _, m := range e.Matches {
		fmt.Fprintf(&b, "\t%s\n", m)
	}
	b.WriteString("specify the full import path to disambiguate")
	return b.String()
}

func (e *AmbiguousToolError) Unwrap() error {
	return ErrAmbiguousTool
}

// ModEditJSON 对应 `go mod edit -json` 的输出结构。
type ModEditJSON struct {
	Module struct {
		Path string `json:"Path"`
	} `json:"Module"`
	Go      string        `json:"Go"`
	Require []RequireItem `json:"Require"`
	Replace []ReplaceItem `json:"Replace"`
	Tool    []ToolItem    `json:"Tool"`
}

// RequireItem 表示模块清单中的单条 require 依赖。
type RequireItem struct {
	Path     string `json:"Path"`
	Version  string `json:"Version"`
	Indirect bool   `json:"Indirect"`
}

// ReplaceItem 表示模块清单中的单条 replace 规则。
type ReplaceItem struct {
	Old ModuleVersion `json:"Old"`
	New ModuleVersion `json:"New"`
}

// ModuleVersion 表示模块路径与可选版本号。
type ModuleVersion struct {
	Path    string `json:"Path"`
	Version string `json:"Version"`
}

// ToolItem 表示 got.mod 中声明的单条 tool 指令。
type ToolItem struct {
	Path string `json:"Path"`
}

// IsPackageInModule 判断包路径 pkgPath 是否隶属于模块 modPath。
func IsPackageInModule(pkgPath, modPath string) bool {
	if pkgPath == "" || modPath == "" {
		return false
	}
	return pkgPath == modPath || strings.HasPrefix(pkgPath, modPath+"/")
}

// MatchRequireModule 在 require 列表中按最长模块路径前缀匹配 pkgPath 所属模块。
func MatchRequireModule(pkgPath string, requires []RequireItem) (RequireItem, bool) {
	var best RequireItem
	found := false
	for _, req := range requires {
		if !IsPackageInModule(pkgPath, req.Path) {
			continue
		}
		if !found || len(req.Path) > len(best.Path) {
			best = req
			found = true
		}
	}
	return best, found
}

// FormatReplaceArg 将 ReplaceItem 格式化为 `go mod edit -replace=<arg>` 接受的参数形式。
func FormatReplaceArg(rep ReplaceItem) string {
	oldSpec := FormatDropReplaceArg(rep)
	newSpec := rep.New.Path
	if rep.New.Version != "" {
		newSpec += "@" + rep.New.Version
	}
	return oldSpec + "=" + newSpec
}

// FormatDropReplaceArg 将 ReplaceItem 格式化为 `go mod edit -dropreplace=<arg>` 接受的参数形式。
func FormatDropReplaceArg(rep ReplaceItem) string {
	if rep.Old.Version != "" {
		return rep.Old.Path + "@" + rep.Old.Version
	}
	return rep.Old.Path
}

// ParseModFile 调用 `go mod edit -json <modFilePath>` 解析模块文件。
func ParseModFile(ctx context.Context, modFilePath string) (*ModEditJSON, error) {
	cmd := exec.CommandContext(ctx, "go", "mod", "edit", "-json", modFilePath)
	// 设置 GOWORK=off，防止在特定环境下受到 go.work 影响。
	cmd.Env = append(os.Environ(), "GOWORK=off")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Debug("解析模块清单", "args", cmd.Args)
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, fmt.Errorf("failed to parse %s: %s: %w", modFilePath, errMsg, err)
		}
		return nil, fmt.Errorf("failed to parse %s: %w", modFilePath, err)
	}

	var parsed ModEditJSON
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return nil, fmt.Errorf("failed to decode go mod edit -json output for %s: %w", modFilePath, err)
	}
	return &parsed, nil
}

// ParseModFileLax 以宽松语法直接读取并解析模块清单中的 module、go、tool、require 与 replace 指令。
// 适用于包含手工填写的 commit hash 或分支名（尚未经 go mod tidy 规范化为伪版本）的 got.mod/go.mod。
func ParseModFileLax(modFilePath string) (*ModEditJSON, error) {
	data, err := os.ReadFile(modFilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s: %w", modFilePath, err)
	}
	return ParseModBytesLax(data), nil
}

// ParseModBytesLax 对模块清单字节内容执行宽松解析。
func ParseModBytesLax(data []byte) *ModEditJSON {
	var parsed ModEditJSON
	block := ""

	for rawLine := range strings.SplitSeq(string(data), "\n") {
		tokens := tokenizeModLine(rawLine)
		if len(tokens) == 0 {
			continue
		}

		if block != "" {
			if tokens[0] == ")" {
				block = ""
				continue
			}
			parseModBlockEntry(&parsed, block, tokens)
			continue
		}

		kw := tokens[0]
		rest := tokens[1:]
		if len(rest) == 0 {
			continue
		}

		if rest[0] == "(" {
			switch kw {
			case "tool", "require", "replace":
				block = kw
			}
			// 兼容单行括号写法，如 `tool ( example.com/a )`
			if len(rest) > 1 && rest[len(rest)-1] == ")" {
				inner := rest[1 : len(rest)-1]
				if len(inner) > 0 && block != "" {
					parseModBlockEntry(&parsed, block, inner)
				}
				block = ""
			}
			continue
		}

		switch kw {
		case "module":
			parsed.Module.Path = rest[0]
		case "go":
			parsed.Go = rest[0]
		case "tool", "require", "replace":
			parseModBlockEntry(&parsed, kw, rest)
		}
	}

	return &parsed
}

func parseModBlockEntry(parsed *ModEditJSON, kind string, tokens []string) {
	switch kind {
	case "tool":
		if len(tokens) >= 1 && tokens[0] != "" {
			parsed.Tool = append(parsed.Tool, ToolItem{Path: tokens[0]})
		}
	case "require":
		if len(tokens) >= 2 {
			parsed.Require = append(parsed.Require, RequireItem{
				Path:    tokens[0],
				Version: tokens[1],
			})
		}
	case "replace":
		arrowIdx := slices.Index(tokens, "=>")
		if arrowIdx < 1 || arrowIdx+1 >= len(tokens) {
			return
		}
		left := tokens[:arrowIdx]
		right := tokens[arrowIdx+1:]
		rep := ReplaceItem{
			Old: ModuleVersion{Path: left[0]},
			New: ModuleVersion{Path: right[0]},
		}
		if len(left) >= 2 {
			rep.Old.Version = left[1]
		}
		if len(right) >= 2 {
			rep.New.Version = right[1]
		}
		parsed.Replace = append(parsed.Replace, rep)
	}
}

// tokenizeModLine 剥离行尾 `//` 注释并按 go.mod 语法切分为 token 切片（支持双引号与反引号及 `(` `)` 分隔）。
func tokenizeModLine(line string) []string {
	var tokens []string
	i := 0
	n := len(line)

	for i < n {
		// 跳过空白字符
		for i < n && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r') {
			i++
		}
		if i >= n {
			break
		}
		// 遇到注释直接结束本行
		if line[i] == '/' && i+1 < n && line[i+1] == '/' {
			break
		}
		// 独立括号 token
		if line[i] == '(' || line[i] == ')' {
			tokens = append(tokens, line[i:i+1])
			i++
			continue
		}
		// 双引号字符串
		if line[i] == '"' {
			start := i
			i++
			for i < n {
				if line[i] == '\\' && i+1 < n {
					i += 2
					continue
				}
				if line[i] == '"' {
					i++
					break
				}
				i++
			}
			raw := line[start:i]
			if unq, err := strconv.Unquote(raw); err == nil {
				tokens = append(tokens, unq)
			} else {
				tokens = append(tokens, strings.Trim(raw, `"`))
			}
			continue
		}
		// 反引号原始字符串
		if line[i] == '`' {
			start := i + 1
			i++
			for i < n && line[i] != '`' {
				i++
			}
			tokens = append(tokens, line[start:i])
			if i < n {
				i++
			}
			continue
		}

		// 普通无引号 token
		start := i
		for i < n {
			c := line[i]
			if c == ' ' || c == '\t' || c == '\r' || c == '(' || c == ')' {
				break
			}
			if c == '/' && i+1 < n && line[i+1] == '/' {
				break
			}
			i++
		}
		if i > start {
			tokens = append(tokens, line[start:i])
		}
	}
	return tokens
}

// ToolShortName 根据 Go 官方规范从工具包路径推导默认短名。
// 若路径末段形如主版本后缀 vN (N >= 2)，则剥离该版本段后取前一段名称。
func ToolShortName(pkgPath string) string {
	p := strings.TrimRight(pkgPath, "/")
	if p == "" {
		return ""
	}
	base := path.Base(p)
	if isMajorVersionSuffix(base) {
		dir := path.Dir(p)
		if dir != "." && dir != "/" {
			return path.Base(dir)
		}
	}
	return base
}

// isMajorVersionSuffix 判断路径段是否为 Go 模块主版本后缀（如 v2、v3 等，N >= 2 且无前导零）。
func isMajorVersionSuffix(seg string) bool {
	if len(seg) < 2 || seg[0] != 'v' {
		return false
	}
	numPart := seg[1:]
	if numPart[0] == '0' {
		return false
	}
	n, err := strconv.Atoi(numPart)
	return err == nil && n >= 2
}

// MatchTool 在 got.mod 声明的 tool 列表中查找目标工具并返回完整包路径。
// 若 name 包含 "/"，执行完整路径精确匹配；否则按短名匹配并检测歧义。
func MatchTool(tools []ToolItem, name string) (string, error) {
	if name == "" {
		return "", ErrToolNotFound
	}

	if strings.Contains(name, "/") {
		for _, item := range tools {
			if item.Path == name {
				return item.Path, nil
			}
		}
		return "", ErrToolNotFound
	}

	var matches []string
	for _, item := range tools {
		if item.Path == name || ToolShortName(item.Path) == name {
			if !slices.Contains(matches, item.Path) {
				matches = append(matches, item.Path)
			}
		}
	}

	switch len(matches) {
	case 0:
		return "", ErrToolNotFound
	case 1:
		return matches[0], nil
	default:
		slices.Sort(matches)
		return "", &AmbiguousToolError{
			Name:    name,
			Matches: matches,
		}
	}
}

// IsBuiltinTool 检查名称是否为当前 Go SDK 内置工具（位于 $GOROOT/pkg/tool/<GOHOSTOS>_<GOHOSTARCH>/）。
func IsBuiltinTool(goRoot, hostOS, hostArch, name string) bool {
	if goRoot == "" || name == "" || strings.Contains(name, "/") || strings.Contains(name, string(filepath.Separator)) {
		return false
	}
	if hostOS == "" {
		hostOS = runtime.GOOS
	}
	if hostArch == "" {
		hostArch = runtime.GOARCH
	}
	toolDir := filepath.Join(goRoot, "pkg", "tool", hostOS+"_"+hostArch)
	binName := name
	if hostOS == "windows" && !strings.HasSuffix(strings.ToLower(binName), ".exe") {
		binName += ".exe"
	}
	info, err := os.Stat(filepath.Join(toolDir, binName))
	if err != nil {
		return false
	}
	return !info.IsDir()
}
