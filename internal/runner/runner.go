package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"unicode"

	"github.com/yougg/got/internal/locator"
)

// Runner 封装底层 go 工具链调用，负责环境变量净化、宿主机平台锁定、-modfile 注入与 -overlay 生命周期管理。
type Runner struct {
	// Stdin、Stdout、Stderr 用于流式子命令执行的标准 IO 绑定。
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// HostOS、HostArch 指定底层工具编译所使用的宿主机平台（默认回退为 runtime.GOOS / runtime.GOARCH），
	// 避免外部交叉编译环境变量 GOOS/GOARCH 导致工具二进制出现 exec format error。
	HostOS   string
	HostArch string
}

// New 创建绑定到当前进程标准输入输出及当前宿主机平台的 Runner。
func New() *Runner {
	return &Runner{
		Stdin:    os.Stdin,
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		HostOS:   runtime.GOOS,
		HostArch: runtime.GOARCH,
	}
}

// RunStreaming 以流式透传 stdio 的方式执行隔离后的 go 子命令，并返回子进程退出码。
func (r *Runner) RunStreaming(ctx context.Context, loc *locator.Context, subcmd string, args []string) (int, error) {
	overlayFlag, cleanup, err := CreateOverlay(loc)
	if err != nil {
		return 1, err
	}
	defer cleanup()

	goArgs := BuildGoArgs(loc, overlayFlag, subcmd, args)
	env := BuildGoEnv(os.Environ(), r.HostOS, r.HostArch)

	cmd := exec.CommandContext(ctx, "go", goArgs...)
	cmd.Dir = loc.RootDir
	cmd.Env = env
	cmd.Stdin = r.Stdin
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr

	slog.Debug("执行底层 go 命令",
		"args", cmd.Args,
		"dir", cmd.Dir,
		"hasGoMod", loc.HasGoMod,
		"overlay", overlayFlag != "",
	)

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("failed to execute go %s: %w", subcmd, err)
	}
	return 0, nil
}

// RunCaptureStdout 执行隔离后的 go 子命令，实时透传 stderr 并捕获 stdout 输出。
// 适用于阶段一 `go tool -n` 等需要读取命令输出同时向用户实时展示编译/下载信息的场景。
func (r *Runner) RunCaptureStdout(ctx context.Context, loc *locator.Context, subcmd string, args []string) (string, int, error) {
	overlayFlag, cleanup, err := CreateOverlay(loc)
	if err != nil {
		return "", 1, err
	}
	defer cleanup()

	goArgs := BuildGoArgs(loc, overlayFlag, subcmd, args)
	env := BuildGoEnv(os.Environ(), r.HostOS, r.HostArch)

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "go", goArgs...)
	cmd.Dir = loc.RootDir
	cmd.Env = env
	cmd.Stdin = r.Stdin
	cmd.Stdout = &stdout
	cmd.Stderr = r.Stderr

	slog.Debug("执行底层 go 命令并捕获输出",
		"args", cmd.Args,
		"dir", cmd.Dir,
		"hasGoMod", loc.HasGoMod,
		"overlay", overlayFlag != "",
	)

	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return stdout.String(), exitErr.ExitCode(), nil
		}
		return stdout.String(), 1, fmt.Errorf("failed to execute go %s: %w", subcmd, err)
	}
	return stdout.String(), 0, nil
}

// BuildGoArgs 按 Go CLI 规范构造包含 -overlay 与 -modfile 的参数列表。
// 对于 `go mod <subcmd>`，-overlay 与 -modfile 必须置于 <subcmd> 之后。
func BuildGoArgs(loc *locator.Context, overlayFlag, subcmd string, args []string) []string {
	var modFlags []string
	if overlayFlag != "" {
		modFlags = append(modFlags, overlayFlag)
	}
	if loc != nil && loc.ModFile != "" {
		modFlags = append(modFlags, "-modfile="+loc.ModFile)
	}

	if subcmd == "mod" {
		if len(args) == 0 || strings.HasPrefix(args[0], "-") {
			out := make([]string, 0, 1+len(args))
			out = append(out, "mod")
			out = append(out, args...)
			return out
		}
		modSub := args[0]
		rest := args[1:]
		out := make([]string, 0, 2+len(modFlags)+len(rest))
		out = append(out, "mod", modSub)
		// `go mod init` 不接受 -modfile/-overlay 标志
		if modSub != "init" {
			out = append(out, modFlags...)
		}
		out = append(out, rest...)
		return out
	}

	out := make([]string, 0, 1+len(modFlags)+len(args))
	out = append(out, subcmd)
	out = append(out, modFlags...)
	out = append(out, args...)
	return out
}

// BuildGoEnv 基于给定环境变量列表构造隔离的 Go 工具链执行环境：
// 1. 强制设置 GOWORK=off，避免 workspace 模式与 -modfile 冲突；
// 2. 强制将 GOOS/GOARCH 覆写为宿主机 GOHOSTOS/GOHOSTARCH，避免交叉编译环境变量导致编译出异构二进制（exec format error）；
// 3. 清洗 GOFLAGS 中的 -mod=... / -mod <val>，避免主项目 -mod=vendor 等设置干扰工具构建。
func BuildGoEnv(baseEnv []string, hostOS, hostArch string) []string {
	if hostOS == "" {
		hostOS = runtime.GOOS
	}
	if hostArch == "" {
		hostArch = runtime.GOARCH
	}

	env := make([]string, 0, len(baseEnv)+3)
	for _, entry := range baseEnv {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		switch key {
		case "GOWORK":
			// 稍后统一追加 GOWORK=off
			continue
		case "GOOS":
			if val != hostOS {
				slog.Debug("已将编译期 GOOS 重置为宿主机 GOHOSTOS", "before", val, "after", hostOS)
			}
			continue
		case "GOARCH":
			if val != hostArch {
				slog.Debug("已将编译期 GOARCH 重置为宿主机 GOHOSTARCH", "before", val, "after", hostArch)
			}
			continue
		case "GOFLAGS":
			cleaned := SanitizeGoFlags(val)
			if cleaned != val {
				slog.Debug("已清理 GOFLAGS 中的 -mod 标志", "before", val, "after", cleaned)
			}
			env = append(env, "GOFLAGS="+cleaned)
		default:
			env = append(env, entry)
		}
	}
	env = append(env,
		"GOWORK=off",
		"GOOS="+hostOS,
		"GOARCH="+hostArch,
	)
	return env
}

// SanitizeGoFlags 解析 GOFLAGS 字符串并移除其中的 -mod=...、--mod=...、-mod <val>、--mod <val>。
func SanitizeGoFlags(raw string) string {
	fields, err := splitQuotedFields(raw)
	if err != nil {
		// 若原始 GOFLAGS 引号不合法，回退为空白分割做尽力清理
		fields = strings.Fields(raw)
	}

	var kept []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "-mod" || f == "--mod" {
			// 跳过紧随其后的参数值
			if i+1 < len(fields) {
				i++
			}
			continue
		}
		if strings.HasPrefix(f, "-mod=") || strings.HasPrefix(f, "--mod=") {
			continue
		}
		kept = append(kept, f)
	}
	return joinQuotedFields(kept)
}

// splitQuotedFields 依照 Go 工具链 GOFLAGS 规范拆分带单引号或双引号的空格分隔参数。
func splitQuotedFields(s string) ([]string, error) {
	var fields []string
	var cur strings.Builder
	inToken := false
	var quote rune

	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inToken = true
		case unicode.IsSpace(r):
			if inToken {
				fields = append(fields, cur.String())
				cur.Reset()
				inToken = false
			}
		default:
			cur.WriteRune(r)
			inToken = true
		}
	}

	if quote != 0 {
		return nil, fmt.Errorf("unterminated quote %q in GOFLAGS", quote)
	}
	if inToken {
		fields = append(fields, cur.String())
	}
	return fields, nil
}

// joinQuotedFields 将参数切片重新编码为合法的 GOFLAGS 字符串。
func joinQuotedFields(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	encoded := make([]string, 0, len(fields))
	for _, f := range fields {
		encoded = append(encoded, quoteField(f))
	}
	return strings.Join(encoded, " ")
}

func quoteField(f string) string {
	if f == "" {
		return "''"
	}
	needsQuote := strings.ContainsFunc(f, func(r rune) bool {
		return unicode.IsSpace(r) || r == '\'' || r == '"'
	})
	if !needsQuote {
		return f
	}
	if !strings.ContainsRune(f, '\'') {
		return "'" + f + "'"
	}
	if !strings.ContainsRune(f, '"') {
		return `"` + f + `"`
	}
	// 同时包含单引号与双引号时，采用相邻引号拼接方式
	return "'" + strings.ReplaceAll(f, "'", `'"'"'`) + "'"
}
