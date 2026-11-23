package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yougg/got/internal/gomod"
	"github.com/yougg/got/internal/locator"
)

// RunModTidy 执行 `got mod tidy`（及其内部触发的 tidy）。
// 当 got.mod 中不包含仓库本地工具时，在干净的临时沙箱目录中执行 `go mod tidy` 并回写结果，
// 防止扫描主项目目录中的业务 Go 源码文件，避免业务依赖渗入或因主项目业务 replace 冲突导致报错。
func (r *Runner) RunModTidy(ctx context.Context, loc *locator.Context, tidyArgs []string) (int, error) {
	fullArgs := make([]string, 0, 1+len(tidyArgs))
	fullArgs = append(fullArgs, "tidy")
	fullArgs = append(fullArgs, tidyArgs...)

	for _, arg := range tidyArgs {
		if arg == "-h" || arg == "-help" || arg == "--help" {
			return r.RunStreaming(ctx, loc, "mod", fullArgs)
		}
	}

	mod, err := gomod.ParseModFile(ctx, loc.ModFile)
	if err != nil {
		return 1, err
	}

	// 若 got.mod 声明了当前模块内的仓库本地工具，需要在仓库根目录上下文执行 tidy 以读取本地工具源码
	if hasLocalModuleTools(mod) {
		slog.Debug("检测到 got.mod 包含仓库本地工具，在模块根目录执行原生 go mod tidy", "modFile", loc.ModFile)
		return r.RunStreaming(ctx, loc, "mod", fullArgs)
	}

	return r.runSandboxedTidy(ctx, loc, mod, tidyArgs)
}

func hasLocalModuleTools(mod *gomod.ModEditJSON) bool {
	if mod == nil || mod.Module.Path == "" {
		return false
	}
	for _, item := range mod.Tool {
		if gomod.IsPackageInModule(item.Path, mod.Module.Path) {
			return true
		}
	}
	return false
}

type relReplaceRewrite struct {
	orig gomod.ReplaceItem
	abs  gomod.ReplaceItem
}

func (r *Runner) runSandboxedTidy(ctx context.Context, loc *locator.Context, mod *gomod.ModEditJSON, tidyArgs []string) (int, error) {
	origModInfo, err := os.Stat(loc.ModFile)
	if err != nil {
		return 1, fmt.Errorf("failed to stat %s: %w", loc.ModFile, err)
	}
	origModData, err := os.ReadFile(loc.ModFile)
	if err != nil {
		return 1, fmt.Errorf("failed to read %s: %w", loc.ModFile, err)
	}

	sumPath := modSumPath(loc.ModFile)
	var origSumData []byte
	var origSumPerm os.FileMode = 0o644
	sumExisted := false
	if sumPath != "" {
		if sumInfo, statErr := os.Stat(sumPath); statErr == nil && !sumInfo.IsDir() {
			sumExisted = true
			origSumPerm = sumInfo.Mode().Perm()
			origSumData, err = os.ReadFile(sumPath)
			if err != nil {
				return 1, fmt.Errorf("failed to read %s: %w", sumPath, err)
			}
		}
	}

	tmpDir, err := os.MkdirTemp("", "got-tidy-*")
	if err != nil {
		return 1, fmt.Errorf("failed to create temporary tidy directory: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			slog.Debug("清理临时 tidy 沙箱目录失败", "dir", tmpDir, "err", rmErr)
		}
	}()

	tmpMod := filepath.Join(tmpDir, "go.mod")
	tmpSum := filepath.Join(tmpDir, "go.sum")
	if err := os.WriteFile(tmpMod, origModData, 0o600); err != nil {
		return 1, fmt.Errorf("failed to prepare sandbox go.mod: %w", err)
	}
	if sumExisted {
		if err := os.WriteFile(tmpSum, origSumData, 0o600); err != nil {
			return 1, fmt.Errorf("failed to prepare sandbox go.sum: %w", err)
		}
	}

	env := BuildGoEnv(os.Environ(), r.HostOS, r.HostArch)

	// 将相对本地路径的 replace 临时重映射为基于 loc.RootDir 的绝对路径，保证沙箱内可正常解析
	var rewrites []relReplaceRewrite
	for _, rep := range mod.Replace {
		if rep.New.Version == "" && rep.New.Path != "" && !filepath.IsAbs(rep.New.Path) {
			absRep := rep
			absRep.New.Path = filepath.Clean(filepath.Join(loc.RootDir, rep.New.Path))
			rewrites = append(rewrites, relReplaceRewrite{
				orig: rep,
				abs:  absRep,
			})
		}
	}
	if len(rewrites) > 0 {
		if err := rewriteSandboxReplaces(ctx, tmpDir, tmpMod, env, rewrites, false); err != nil {
			return 1, err
		}
	}

	goArgs := make([]string, 0, 2+len(tidyArgs))
	goArgs = append(goArgs, "mod", "tidy")
	goArgs = append(goArgs, tidyArgs...)

	cmd := exec.CommandContext(ctx, "go", goArgs...)
	cmd.Dir = tmpDir
	cmd.Env = env
	cmd.Stdin = r.Stdin
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr

	slog.Debug("在隔离沙箱中执行 go mod tidy",
		"args", cmd.Args,
		"sandboxDir", tmpDir,
		"modFile", loc.ModFile,
	)

	if runErr := cmd.Run(); runErr != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](runErr); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("failed to execute sandboxed go mod tidy: %w", runErr)
	}

	// -diff 为只读预览模式，不回写原文件
	if isTidyDiffMode(tidyArgs) {
		return 0, nil
	}

	// 还原相对本地路径 replace
	if len(rewrites) > 0 {
		if err := rewriteSandboxReplaces(ctx, tmpDir, tmpMod, env, rewrites, true); err != nil {
			return 1, err
		}
	}

	newModData, err := os.ReadFile(tmpMod)
	if err != nil {
		return 1, fmt.Errorf("failed to read tidied go.mod: %w", err)
	}
	if !bytes.Equal(origModData, newModData) {
		if err := os.WriteFile(loc.ModFile, newModData, origModInfo.Mode().Perm()); err != nil {
			return 1, fmt.Errorf("failed to write tidied %s: %w", loc.ModFile, err)
		}
	}

	if sumPath != "" {
		newSumData, sumReadErr := os.ReadFile(tmpSum)
		switch {
		case sumReadErr == nil && len(newSumData) > 0:
			if !sumExisted || !bytes.Equal(origSumData, newSumData) {
				if err := os.WriteFile(sumPath, newSumData, origSumPerm); err != nil {
					return 1, fmt.Errorf("failed to write tidied %s: %w", sumPath, err)
				}
			}
		case sumReadErr == nil && len(newSumData) == 0, errors.Is(sumReadErr, os.ErrNotExist):
			if sumExisted {
				if rmErr := os.Remove(sumPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
					return 1, fmt.Errorf("failed to remove empty %s: %w", sumPath, rmErr)
				}
			}
		default:
			return 1, fmt.Errorf("failed to read tidied go.sum: %w", sumReadErr)
		}
	}

	return 0, nil
}

func rewriteSandboxReplaces(ctx context.Context, tmpDir, tmpMod string, env []string, rewrites []relReplaceRewrite, restore bool) error {
	args := make([]string, 0, 2+len(rewrites)+1)
	args = append(args, "mod", "edit")
	for _, rw := range rewrites {
		target := rw.abs
		if restore {
			target = rw.orig
		}
		args = append(args, "-replace="+gomod.FormatReplaceArg(target))
	}
	args = append(args, tmpMod)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = tmpDir
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return fmt.Errorf("failed to rewrite relative replace in sandbox: %s: %w", errMsg, err)
		}
		return fmt.Errorf("failed to rewrite relative replace in sandbox: %w", err)
	}
	return nil
}

func modSumPath(modFile string) string {
	if base, ok := strings.CutSuffix(modFile, ".mod"); ok {
		return base + ".sum"
	}
	return ""
}

func isTidyDiffMode(args []string) bool {
	return slices.ContainsFunc(args, func(arg string) bool {
		return arg == "-diff" || arg == "--diff" || arg == "-diff=true" || arg == "--diff=true"
	})
}
