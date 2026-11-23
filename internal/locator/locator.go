package locator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// DefaultModFileName 是默认工具清单文件名。
	DefaultModFileName = "got.mod"
	// GoModFileName 是 Go 主模块文件名。
	GoModFileName = "go.mod"
)

// ErrModFileNotFound 表示未能定位到 got.mod 文件。
var ErrModFileNotFound = errors.New("got.mod not found")

// Context 描述定位到的 got.mod 上下文信息。
type Context struct {
	// ModFile 是 got.mod 的绝对路径。
	ModFile string
	// RootDir 是 got.mod 所在目录的绝对路径。
	RootDir string
	// HasGoMod 表示 RootDir 下是否同时存在 go.mod。
	HasGoMod bool
	// OrigWorkDir 表示调用方的原始工作目录（经过 -C 解析后的绝对路径，用于工具执行期保持 Cwd）。
	OrigWorkDir string
}

// ResolveWorkDir 根据可选的 -C 目录参数解析出最终工作目录的绝对路径。
func ResolveWorkDir(chDir string) (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to get working directory: %w", err)
	}
	if chDir == "" {
		return filepath.Abs(wd)
	}

	target := chDir
	if !filepath.IsAbs(target) {
		target = filepath.Join(wd, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", fmt.Errorf("failed to resolve directory %q: %w", chDir, err)
	}

	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("cannot change directory to %q: %w", chDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cannot change directory to %q: not a directory", chDir)
	}
	return target, nil
}

// Locate 定位 got.mod 文件并探测其所在目录的 go.mod 共存状态。
// 若 gotFileEnv 非空，直接使用该路径而不进行向上目录查找。
func Locate(workDir, gotFileEnv string) (*Context, error) {
	origWorkDir, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve working directory: %w", err)
	}

	var modFile string
	if gotFileEnv != "" {
		candidate := gotFileEnv
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(origWorkDir, candidate)
		}
		candidate, err = filepath.Abs(candidate)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve GOTFILE %q: %w", gotFileEnv, err)
		}
		if !fileExists(candidate) {
			return nil, fmt.Errorf("GOTFILE %q does not exist: %w", candidate, ErrModFileNotFound)
		}
		modFile = candidate
	} else {
		found, ok := findUpwards(origWorkDir, DefaultModFileName)
		if !ok {
			return nil, fmt.Errorf("%w (searched upward from %s); run 'got init' to create one", ErrModFileNotFound, origWorkDir)
		}
		modFile = found
	}

	rootDir := filepath.Dir(modFile)
	hasGoMod := fileExists(filepath.Join(rootDir, GoModFileName))

	return &Context{
		ModFile:     modFile,
		RootDir:     rootDir,
		HasGoMod:    hasGoMod,
		OrigWorkDir: origWorkDir,
	}, nil
}

// findUpwards 从 startDir 开始逐级向上查找指定文件名。
func findUpwards(startDir, fileName string) (string, bool) {
	dir := filepath.Clean(startDir)
	for {
		candidate := filepath.Join(dir, fileName)
		if fileExists(candidate) {
			return candidate, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}
