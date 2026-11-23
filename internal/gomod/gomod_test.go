package gomod

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseGoVersion(t *testing.T) {
	tests := []struct {
		input     string
		wantMajor int
		wantMinor int
		wantOK    bool
	}{
		{input: "go1.24", wantMajor: 1, wantMinor: 24, wantOK: true},
		{input: "go1.24.0", wantMajor: 1, wantMinor: 24, wantOK: true},
		{input: "go1.27.4", wantMajor: 1, wantMinor: 27, wantOK: true},
		{input: "go1.24rc1", wantMajor: 1, wantMinor: 24, wantOK: true},
		{input: "devel go1.25-abcdef1234", wantMajor: 1, wantMinor: 25, wantOK: true},
		{input: "go2.0.0", wantMajor: 2, wantMinor: 0, wantOK: true},
		{input: "go1.23.9", wantMajor: 1, wantMinor: 23, wantOK: true},
		{input: "", wantOK: false},
		{input: "1.24.0", wantOK: false},
		{input: "go1", wantOK: false},
	}

	for _, tc := range tests {
		maj, minVal, ok := ParseGoVersion(tc.input)
		if ok != tc.wantOK || maj != tc.wantMajor || minVal != tc.wantMinor {
			t.Errorf("ParseGoVersion(%q) = (%d, %d, %v), want (%d, %d, %v)",
				tc.input, maj, minVal, ok, tc.wantMajor, tc.wantMinor, tc.wantOK)
		}
	}
}

func TestValidateGoVersion(t *testing.T) {
	if err := ValidateGoVersion("go1.24.0"); err != nil {
		t.Fatalf("unexpected error for go1.24.0: %v", err)
	}
	if err := ValidateGoVersion("go1.27.4"); err != nil {
		t.Fatalf("unexpected error for go1.27.4: %v", err)
	}
	if err := ValidateGoVersion("devel go1.25-abc"); err != nil {
		t.Fatalf("unexpected error for devel go1.25-abc: %v", err)
	}

	err := ValidateGoVersion("go1.23.8")
	if err == nil {
		t.Fatal("expected error for go1.23.8, got nil")
	}
	if want := "got requires Go 1.24 or later (found go1.23.8)"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestFormatGoModVersion(t *testing.T) {
	if got := FormatGoModVersion("go1.27.4"); got != "1.27" {
		t.Errorf("FormatGoModVersion(go1.27.4) = %q, want %q", got, "1.27")
	}
	if got := FormatGoModVersion("devel go1.25-xyz"); got != "1.25" {
		t.Errorf("FormatGoModVersion(devel go1.25-xyz) = %q, want %q", got, "1.25")
	}
}

func TestToolShortName(t *testing.T) {
	tests := []struct {
		pkg  string
		want string
	}{
		{pkg: "golang.org/x/tools/cmd/stringer", want: "stringer"},
		{pkg: "github.com/golangci/golangci-lint/v2/cmd/golangci-lint", want: "golangci-lint"},
		{pkg: "example.com/mytool/v2", want: "mytool"},
		{pkg: "example.com/mytool/v10", want: "mytool"},
		{pkg: "example.com/mytool/v1", want: "v1"},
		{pkg: "example.com/mytool/v02", want: "v02"},
		{pkg: "example.com/v2", want: "example.com"},
		{pkg: "simple", want: "simple"},
	}

	for _, tc := range tests {
		if got := ToolShortName(tc.pkg); got != tc.want {
			t.Errorf("ToolShortName(%q) = %q, want %q", tc.pkg, got, tc.want)
		}
	}
}

func TestMatchTool(t *testing.T) {
	tools := []ToolItem{
		{Path: "golang.org/x/tools/cmd/stringer"},
		{Path: "example.com/foo/v2"},
		{Path: "example.com/a/cmd/gen"},
		{Path: "example.com/b/cmd/gen"},
	}

	// 短名唯一匹配
	got, err := MatchTool(tools, "stringer")
	if err != nil || got != "golang.org/x/tools/cmd/stringer" {
		t.Fatalf("MatchTool(stringer) = (%q, %v), want stringer path", got, err)
	}

	// 主版本后缀剥离匹配
	got, err = MatchTool(tools, "foo")
	if err != nil || got != "example.com/foo/v2" {
		t.Fatalf("MatchTool(foo) = (%q, %v), want example.com/foo/v2", got, err)
	}

	// 全路径精确匹配
	got, err = MatchTool(tools, "example.com/b/cmd/gen")
	if err != nil || got != "example.com/b/cmd/gen" {
		t.Fatalf("MatchTool(full path) = (%q, %v), want example.com/b/cmd/gen", got, err)
	}

	// 短名歧义错误
	_, err = MatchTool(tools, "gen")
	if !errors.Is(err, ErrAmbiguousTool) {
		t.Fatalf("MatchTool(gen) err = %v, want ErrAmbiguousTool", err)
	}
	var ambErr *AmbiguousToolError
	if !errors.As(err, &ambErr) {
		t.Fatalf("expected *AmbiguousToolError, got %T", err)
	}
	wantMatches := []string{"example.com/a/cmd/gen", "example.com/b/cmd/gen"}
	if !slices.Equal(ambErr.Matches, wantMatches) {
		t.Errorf("ambiguous matches = %v, want %v", ambErr.Matches, wantMatches)
	}
	if !strings.Contains(err.Error(), "example.com/a/cmd/gen") || !strings.Contains(err.Error(), "example.com/b/cmd/gen") {
		t.Errorf("error message should list conflicting paths, got: %s", err.Error())
	}

	// 未找到
	_, err = MatchTool(tools, "nonexistent")
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("MatchTool(nonexistent) err = %v, want ErrToolNotFound", err)
	}
}

func TestParseModFileAndIsBuiltinTool(t *testing.T) {
	ctx := context.Background()
	info, err := InspectGoEnv(ctx)
	if err != nil {
		t.Fatalf("InspectGoEnv failed: %v", err)
	}

	if info.GoHostOS == "" || info.GoHostArch == "" {
		t.Fatalf("expected non-empty GoHostOS/GoHostArch, got %+v", info)
	}
	if !IsBuiltinTool(info.GoRoot, info.GoHostOS, info.GoHostArch, "vet") {
		t.Errorf("expected 'vet' to be recognized as a builtin Go tool under %s", info.GoRoot)
	}
	if IsBuiltinTool(info.GoRoot, info.GoHostOS, info.GoHostArch, "definitely-not-a-go-builtin-tool") {
		t.Error("unexpected builtin recognition for fake tool name")
	}

	tmpDir := t.TempDir()
	modPath := filepath.Join(tmpDir, "got.mod")
	content := "module got.local/tools\n\ngo 1.24\n\ntool (\n\texample.com/a/cmd/alpha\n\texample.com/b/beta/v2\n)\n"
	if err := os.WriteFile(modPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write got.mod: %v", err)
	}

	parsed, err := ParseModFile(ctx, modPath)
	if err != nil {
		t.Fatalf("ParseModFile failed: %v", err)
	}
	if parsed.Module.Path != "got.local/tools" {
		t.Errorf("Module.Path = %q, want got.local/tools", parsed.Module.Path)
	}
	if len(parsed.Tool) != 2 {
		t.Fatalf("len(Tool) = %d, want 2", len(parsed.Tool))
	}
}

func TestIsPackageInModuleAndMatchRequire(t *testing.T) {
	if !IsPackageInModule("example.com/a/cmd/tool", "example.com/a") {
		t.Error("expected subpackage to match module")
	}
	if !IsPackageInModule("example.com/a", "example.com/a") {
		t.Error("expected root package to match module")
	}
	if IsPackageInModule("example.com/ab/cmd/tool", "example.com/a") {
		t.Error("prefix without slash boundary must not match")
	}

	requires := []RequireItem{
		{Path: "example.com/foo", Version: "v1.0.0"},
		{Path: "example.com/foo/sub", Version: "v2.3.4"},
		{Path: "example.com/other", Version: "v0.1.0"},
	}

	// 应优先匹配更长的嵌套子模块路径
	req, ok := MatchRequireModule("example.com/foo/sub/cmd/gen", requires)
	if !ok || req.Path != "example.com/foo/sub" || req.Version != "v2.3.4" {
		t.Fatalf("MatchRequireModule nested = (%+v, %v), want example.com/foo/sub@v2.3.4", req, ok)
	}

	req, ok = MatchRequireModule("example.com/foo/cmd/gen", requires)
	if !ok || req.Path != "example.com/foo" || req.Version != "v1.0.0" {
		t.Fatalf("MatchRequireModule parent = (%+v, %v), want example.com/foo@v1.0.0", req, ok)
	}

	if _, ok := MatchRequireModule("example.com/missing/cmd/x", requires); ok {
		t.Error("expected no match for missing module")
	}
}

func TestFormatReplaceArg(t *testing.T) {
	got := FormatReplaceArg(ReplaceItem{
		Old: ModuleVersion{Path: "example.com/old"},
		New: ModuleVersion{Path: "./local/old"},
	})
	if got != "example.com/old=./local/old" {
		t.Errorf("FormatReplaceArg = %q, want example.com/old=./local/old", got)
	}

	got = FormatReplaceArg(ReplaceItem{
		Old: ModuleVersion{Path: "example.com/old", Version: "v1.0.0"},
		New: ModuleVersion{Path: "example.com/new", Version: "v1.2.3"},
	})
	if got != "example.com/old@v1.0.0=example.com/new@v1.2.3" {
		t.Errorf("FormatReplaceArg = %q, want example.com/old@v1.0.0=example.com/new@v1.2.3", got)
	}
}
