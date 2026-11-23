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
