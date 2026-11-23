package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/yougg/got/internal/locator"
	"github.com/yougg/got/internal/runner"
)

// Executor 负责工具的两阶段编译执行、PATH 注入、信号转发以及内置工具透传。
type Executor struct {
	Runner *runner.Runner
	GoRoot string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// New 创建绑定到当前进程标准 IO 的 Executor。
func New(r *runner.Runner, goRoot string) *Executor {
	return &Executor{
		Runner: r,
		GoRoot: goRoot,
		Stdin:  r.Stdin,
		Stdout: r.Stdout,
		Stderr: r.Stderr,
	}
}

// RunCustomTool 对 got.mod 中声明的工具执行两阶段编译与运行：
// 阶段一：GOWORK=off 调用 `go tool -modfile=<got.mod> -n <targetPkg>` 触发增量编译并获取缓存二进制路径；
// 阶段二：恢复用户原始环境变量与原始工作目录，并在 PATH 最前端注入 $GOROOT/bin 后直接执行该二进制文件。
func (e *Executor) RunCustomTool(ctx context.Context, loc *locator.Context, targetPkg string, toolArgs []string, rawToolArgs []string) (int, error) {
	// 阶段一：隔离环境下编译并获取工具可执行文件路径
	stdout, exitCode, err := e.Runner.RunCaptureStdout(ctx, loc, "tool", []string{"-n", targetPkg})
	if err != nil {
		return 1, err
	}
	if exitCode != 0 {
		return exitCode, nil
	}

	binPath := parseToolBinaryPath(stdout)
	if binPath == "" {
		return 1, fmt.Errorf("go tool -n %s returned empty executable path", targetPkg)
	}

	// 若解析到的二进制位于 $GOROOT/pkg/tool/ 下，按内置工具降级透传处理
	if isUnderGoRootToolDir(e.GoRoot, binPath) {
		slog.Debug("检测到工具位于 GOROOT/pkg/tool，降级为内置工具透传", "binPath", binPath)
		return e.RunPassthrough(ctx, loc.OrigWorkDir, rawToolArgs)
	}

	// 安全校验：确认二进制存在且具备可执行权限
	if err := verifyExecutable(binPath); err != nil {
		return 1, fmt.Errorf("resolved tool binary %q is not executable: %w", binPath, err)
	}

	// 阶段二：恢复原始环境变量与 Cwd，前置 GOROOT/bin 到 PATH 并直接运行工具
	env := PrependGoRootBin(os.Environ(), e.GoRoot)
	slog.Debug("阶段二直接执行工具二进制",
		"bin", binPath,
		"args", toolArgs,
		"dir", loc.OrigWorkDir,
	)

	return e.execWithSignalForwarding(ctx, binPath, toolArgs, loc.OrigWorkDir, env)
}

// RunPassthrough 保留用户原始环境变量与工作目录，将参数直接透传给原生 `go tool`。
func (e *Executor) RunPassthrough(ctx context.Context, workDir string, args []string) (int, error) {
	goArgs := make([]string, 0, 1+len(args))
	goArgs = append(goArgs, "tool")
	goArgs = append(goArgs, args...)

	slog.Debug("原始环境透传执行 go tool", "args", goArgs, "dir", workDir)
	return e.execWithSignalForwarding(ctx, "go", goArgs, workDir, os.Environ())
}

// execWithSignalForwarding 启动子进程、转发 SIGINT/SIGTERM 并透传子进程退出码。
func (e *Executor) execWithSignalForwarding(ctx context.Context, name string, args []string, dir string, env []string) (int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = e.Stdin
	cmd.Stdout = e.Stdout
	cmd.Stderr = e.Stderr

	if err := cmd.Start(); err != nil {
		return 1, fmt.Errorf("failed to start %s: %w", name, err)
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	doneCh := make(chan struct{})

	go func() {
		for {
			select {
			case sig := <-sigCh:
				if cmd.Process != nil {
					_ = cmd.Process.Signal(sig)
				}
			case <-doneCh:
				return
			}
		}
	}()

	waitErr := cmd.Wait()
	signal.Stop(sigCh)
	close(doneCh)

	if waitErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](waitErr); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("failed to wait for %s: %w", name, waitErr)
	}
	return 0, nil
}

// PrependGoRootBin 在环境变量列表的 PATH 最前端注入 <GOROOT>/bin，
// 保持 GOWORK 及其他原始环境变量原样不变。
func PrependGoRootBin(baseEnv []string, goRoot string) []string {
	env := make([]string, 0, len(baseEnv)+1)
	if goRoot == "" {
		env = append(env, baseEnv...)
		return env
	}

	goBin := filepath.Join(goRoot, "bin")
	sep := string(filepath.ListSeparator)
	foundPath := false

	for _, entry := range baseEnv {
		key, val, ok := strings.Cut(entry, "=")
		if !ok {
			env = append(env, entry)
			continue
		}
		if key == "PATH" || (runtime.GOOS == "windows" && strings.EqualFold(key, "PATH")) {
			foundPath = true
			if val == "" {
				env = append(env, key+"="+goBin)
			} else {
				env = append(env, key+"="+goBin+sep+val)
			}
			continue
		}
		env = append(env, entry)
	}

	if !foundPath {
		env = append(env, "PATH="+goBin)
	}
	return env
}

// parseToolBinaryPath 从 `go tool -n <pkg>` 的标准输出中提取可执行文件路径。
func parseToolBinaryPath(stdout string) string {
	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" {
		return ""
	}
	lines := strings.Split(trimmed, "\n")
	var lastLine string
	for _, line := range slices.Backward(lines) {
		line := strings.TrimSpace(line)
		if line != "" {
			lastLine = line
			break
		}
	}
	if lastLine == "" {
		return ""
	}

	// 若原始路径直接存在则直接返回，避免误剥离合法文件名中的引号字符
	if _, err := os.Stat(lastLine); err == nil {
		return lastLine
	}

	// 处理包含空格时 go tool -n 输出的成对单引号或双引号
	if len(lastLine) >= 2 {
		first, last := lastLine[0], lastLine[len(lastLine)-1]
		if (first == '\'' && last == '\'') || (first == '"' && last == '"') {
			unquoted := lastLine[1 : len(lastLine)-1]
			if _, err := os.Stat(unquoted); err == nil {
				return unquoted
			}
		}
	}
	return lastLine
}

// isUnderGoRootToolDir 判断给定二进制路径是否位于 $GOROOT/pkg/tool/ 目录之下。
func isUnderGoRootToolDir(goRoot, binPath string) bool {
	if goRoot == "" || binPath == "" {
		return false
	}
	toolRoot := filepath.Clean(filepath.Join(goRoot, "pkg", "tool"))
	cleanBin := filepath.Clean(binPath)
	rel, err := filepath.Rel(toolRoot, cleanBin)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// verifyExecutable 检查目标路径存在、非目录且具备可执行权限。
func verifyExecutable(binPath string) error {
	info, err := os.Stat(binPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("path is a directory")
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		return fmt.Errorf("missing execute permission (mode %s)", info.Mode())
	}
	return nil
}
