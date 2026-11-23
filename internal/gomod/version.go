package gomod

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

const (
	// MinGoMajor 是 got 要求的最低 Go 主版本号。
	MinGoMajor = 1
	// MinGoMinor 是 got 要求的最低 Go 次版本号（Go 1.24 引入 tool 指令）。
	MinGoMinor = 24
)

// EnvInfo 保存底层 Go 工具链的环境信息。
type EnvInfo struct {
	// GoVersion 为 go env GOVERSION 的值，例如 "go1.27.4"。
	GoVersion string `json:"GOVERSION"`
	// GoRoot 为 go env GOROOT 的值。
	GoRoot string `json:"GOROOT"`
	// GoHostOS 为当前宿主机操作系统（go env GOHOSTOS），用于确保工具按宿主机平台编译。
	GoHostOS string `json:"GOHOSTOS"`
	// GoHostArch 为当前宿主机架构（go env GOHOSTARCH），用于确保工具按宿主机平台编译。
	GoHostArch string `json:"GOHOSTARCH"`
}

// InspectGoEnv 查询当前系统的 Go 版本、GOROOT 及宿主机平台信息，并校验版本是否满足 >= 1.24。
func InspectGoEnv(ctx context.Context) (*EnvInfo, error) {
	info, err := QueryGoEnv(ctx)
	if err != nil {
		return nil, err
	}
	if err := ValidateGoVersion(info.GoVersion); err != nil {
		return nil, err
	}
	return info, nil
}

// QueryGoEnv 执行 `go env -json GOVERSION GOROOT GOHOSTOS GOHOSTARCH` 获取 Go 环境信息。
func QueryGoEnv(ctx context.Context) (*EnvInfo, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "-json", "GOVERSION", "GOROOT", "GOHOSTOS", "GOHOSTARCH")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Debug("查询 Go 环境信息", "args", cmd.Args)
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, fmt.Errorf("failed to run go env: %s: %w", errMsg, err)
		}
		return nil, fmt.Errorf("failed to run go env: %w", err)
	}

	var info EnvInfo
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		return nil, fmt.Errorf("failed to parse go env output: %w", err)
	}
	if info.GoVersion == "" {
		return nil, fmt.Errorf("empty GOVERSION from go env")
	}
	if info.GoHostOS == "" {
		info.GoHostOS = runtime.GOOS
	}
	if info.GoHostArch == "" {
		info.GoHostArch = runtime.GOARCH
	}
	return &info, nil
}

// ValidateGoVersion 校验 GOVERSION 字符串是否满足 Go 1.24 及以上要求。
func ValidateGoVersion(goVersion string) error {
	major, minor, ok := ParseGoVersion(goVersion)
	if !ok {
		return fmt.Errorf("unable to parse Go version %q", goVersion)
	}
	if major < MinGoMajor || (major == MinGoMajor && minor < MinGoMinor) {
		return fmt.Errorf("got requires Go %d.%d or later (found %s)", MinGoMajor, MinGoMinor, goVersion)
	}
	return nil
}

// ParseGoVersion 解析形如 "go1.24.0"、"go1.27.4"、"go1.24rc1"、"devel go1.25-abc" 的版本串。
func ParseGoVersion(v string) (major, minor int, ok bool) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, 0, false
	}

	// 处理开发版前缀，例如 "devel go1.25-abcdef..."
	if after, found := strings.CutPrefix(s, "devel "); found {
		s = strings.TrimSpace(after)
	}

	// 找到 "go" 前缀
	after, found := strings.CutPrefix(s, "go")
	if !found {
		return 0, 0, false
	}
	s = after

	// 提取主版本号
	majStr, rest := takeDigits(s)
	if majStr == "" || !strings.HasPrefix(rest, ".") {
		return 0, 0, false
	}
	rest = rest[1:]

	// 提取次版本号
	minStr, _ := takeDigits(rest)
	if minStr == "" {
		return 0, 0, false
	}

	maj, err1 := strconv.Atoi(majStr)
	minVal, err2 := strconv.Atoi(minStr)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, minVal, true
}

// FormatGoModVersion 将 GOVERSION 格式化为 got.mod 中的 "X.Y" 版本号。
func FormatGoModVersion(goVersion string) string {
	major, minor, ok := ParseGoVersion(goVersion)
	if !ok {
		return fmt.Sprintf("%d.%d", MinGoMajor, MinGoMinor)
	}
	return fmt.Sprintf("%d.%d", major, minor)
}

func takeDigits(s string) (digits, rest string) {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i], s[i:]
}
