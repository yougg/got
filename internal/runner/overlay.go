package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/yougg/got/internal/locator"
)

// overlaySpec 对应 `go -overlay=<file>` 接受的 JSON 格式。
type overlaySpec struct {
	Replace map[string]string `json:"Replace"`
}

// CreateOverlay 在 got.mod 同级不存在 go.mod 时生成临时 -overlay 占位文件。
// 返回值为可直接传入 go 命令的 "-overlay=<path>" 参数以及清理回调函数。
// 若同级已存在 go.mod，返回空参数与空清理函数，不在磁盘上创建任何文件。
func CreateOverlay(loc *locator.Context) (flagArg string, cleanup func(), err error) {
	noop := func() {}
	if loc == nil || loc.HasGoMod {
		return "", noop, nil
	}

	tmpDir, err := os.MkdirTemp("", "got-overlay-*")
	if err != nil {
		return "", noop, fmt.Errorf("failed to create temporary overlay directory: %w", err)
	}

	cleanupOnErr := func() {
		_ = os.RemoveAll(tmpDir)
	}

	emptyModPath := filepath.Join(tmpDir, "empty.mod")
	if err := os.WriteFile(emptyModPath, nil, 0o600); err != nil {
		cleanupOnErr()
		return "", noop, fmt.Errorf("failed to create virtual go.mod file: %w", err)
	}

	virtualGoMod := filepath.Join(loc.RootDir, locator.GoModFileName)
	spec := overlaySpec{
		Replace: map[string]string{
			virtualGoMod: emptyModPath,
		},
	}

	data, err := json.Marshal(spec)
	if err != nil {
		cleanupOnErr()
		return "", noop, fmt.Errorf("failed to encode overlay spec: %w", err)
	}

	overlayPath := filepath.Join(tmpDir, "overlay.json")
	if err := os.WriteFile(overlayPath, data, 0o600); err != nil {
		cleanupOnErr()
		return "", noop, fmt.Errorf("failed to write overlay spec: %w", err)
	}

	slog.Debug("已创建临时 overlay 占位文件",
		"overlay", overlayPath,
		"virtualGoMod", virtualGoMod,
		"emptyMod", emptyModPath,
	)

	var once sync.Once
	clean := func() {
		once.Do(func() {
			if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
				slog.Debug("清理临时 overlay 目录失败", "dir", tmpDir, "err", rmErr)
				return
			}
			slog.Debug("已清理临时 overlay 目录", "dir", tmpDir)
		})
	}

	return "-overlay=" + overlayPath, clean, nil
}
