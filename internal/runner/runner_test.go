package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yougg/got/internal/locator"
)

func TestSanitizeGoFlags(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty", in: "", want: ""},
		{name: "only mod vendor", in: "-mod=vendor", want: ""},
		{name: "double dash mod readonly", in: "--mod=readonly", want: ""},
		{name: "space separated mod", in: "-mod vendor", want: ""},
		{name: "preserve other flags", in: "-v -mod=vendor -race", want: "-v -race"},
		{name: "preserve space separated mod and others", in: "-tags=dev -mod readonly -trimpath", want: "-tags=dev -trimpath"},
		{name: "preserve quoted flags with spaces", in: `-mod=vendor '-ldflags=-X main.ver=1.0 -s' -v`, want: `'-ldflags=-X main.ver=1.0 -s' -v`},
		{name: "do not touch modfile or modcacherw", in: "-modcacherw -mod=vendor", want: "-modcacherw"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeGoFlags(tc.in); got != tc.want {
				t.Errorf("SanitizeGoFlags(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestBuildGoEnv(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"GOWORK=/repo/go.work",
		"GOOS=windows",
		"GOARCH=arm64",
		"GOFLAGS=-v -mod=vendor -trimpath",
		"HOME=/home/test",
	}

	got := BuildGoEnv(base, "linux", "amd64")
	if !slices.Contains(got, "GOWORK=off") {
		t.Errorf("expected GOWORK=off in env: %v", got)
	}
	if slices.Contains(got, "GOWORK=/repo/go.work") {
		t.Errorf("original GOWORK should be replaced: %v", got)
	}
	if !slices.Contains(got, "GOOS=linux") || slices.Contains(got, "GOOS=windows") {
		t.Errorf("expected GOOS overridden to host linux, got: %v", got)
	}
	if !slices.Contains(got, "GOARCH=amd64") || slices.Contains(got, "GOARCH=arm64") {
		t.Errorf("expected GOARCH overridden to host amd64, got: %v", got)
	}
	if !slices.Contains(got, "GOFLAGS=-v -trimpath") {
		t.Errorf("expected sanitized GOFLAGS in env: %v", got)
	}
	if !slices.Contains(got, "HOME=/home/test") {
		t.Errorf("expected unrelated env preserved: %v", got)
	}
}

func TestBuildGoArgs(t *testing.T) {
	loc := &locator.Context{
		ModFile: "/repo/got.mod",
		RootDir: "/repo",
	}

	// go get
	got := BuildGoArgs(loc, "", "get", []string{"-tool", "example.com/x@v1.0.0"})
	want := []string{"get", "-modfile=/repo/got.mod", "-tool", "example.com/x@v1.0.0"}
	if !slices.Equal(got, want) {
		t.Errorf("get args = %v, want %v", got, want)
	}

	// go mod tidy with overlay：-overlay 与 -modfile 必须在 tidy 之后
	got = BuildGoArgs(loc, "-overlay=/tmp/ov.json", "mod", []string{"tidy", "-v"})
	want = []string{"mod", "tidy", "-overlay=/tmp/ov.json", "-modfile=/repo/got.mod", "-v"}
	if !slices.Equal(got, want) {
		t.Errorf("mod tidy args = %v, want %v", got, want)
	}

	// go mod edit
	got = BuildGoArgs(loc, "", "mod", []string{"edit", "-tool", "example.com/a"})
	want = []string{"mod", "edit", "-modfile=/repo/got.mod", "-tool", "example.com/a"}
	if !slices.Equal(got, want) {
		t.Errorf("mod edit args = %v, want %v", got, want)
	}

	// go list with overlay
	got = BuildGoArgs(loc, "-overlay=/tmp/ov.json", "list", []string{"tool"})
	want = []string{"list", "-overlay=/tmp/ov.json", "-modfile=/repo/got.mod", "tool"}
	if !slices.Equal(got, want) {
		t.Errorf("list args = %v, want %v", got, want)
	}
}

func TestCreateOverlay(t *testing.T) {
	root := t.TempDir()

	// 同级存在 go.mod 时不应生成 overlay
	flagArg, cleanup, err := CreateOverlay(&locator.Context{
		RootDir:  root,
		ModFile:  filepath.Join(root, "got.mod"),
		HasGoMod: true,
	})
	if err != nil {
		t.Fatalf("CreateOverlay(HasGoMod=true) error: %v", err)
	}
	if flagArg != "" {
		t.Errorf("expected empty overlay flag, got %q", flagArg)
	}
	cleanup()

	// 同级不存在 go.mod 时应生成 overlay 并在 cleanup 时彻底销毁
	flagArg, cleanup, err = CreateOverlay(&locator.Context{
		RootDir:  root,
		ModFile:  filepath.Join(root, "got.mod"),
		HasGoMod: false,
	})
	if err != nil {
		t.Fatalf("CreateOverlay(HasGoMod=false) error: %v", err)
	}
	ovPath, ok := strings.CutPrefix(flagArg, "-overlay=")
	if !ok || ovPath == "" {
		t.Fatalf("unexpected overlay flag %q", flagArg)
	}

	data, err := os.ReadFile(ovPath)
	if err != nil {
		t.Fatalf("read overlay.json: %v", err)
	}
	var spec overlaySpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("unmarshal overlay.json: %v", err)
	}
	virtualGoMod := filepath.Join(root, "go.mod")
	emptyPath, exists := spec.Replace[virtualGoMod]
	if !exists {
		t.Fatalf("overlay missing Replace entry for %q: %+v", virtualGoMod, spec.Replace)
	}
	if _, err := os.Stat(emptyPath); err != nil {
		t.Fatalf("empty.mod does not exist at %q: %v", emptyPath, err)
	}

	ovDir := filepath.Dir(ovPath)
	cleanup()
	// 再次调用应幂等安全
	cleanup()

	if _, err := os.Stat(ovDir); !os.IsNotExist(err) {
		t.Errorf("expected overlay dir %q to be deleted after cleanup, stat err = %v", ovDir, err)
	}
}
