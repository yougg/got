package executor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestPrependGoRootBin(t *testing.T) {
	goRoot := filepath.Join("/opt", "go")
	goBin := filepath.Join(goRoot, "bin")
	sep := string(filepath.ListSeparator)

	base := []string{
		"GOWORK=/repo/go.work",
		"PATH=/usr/local/bin" + sep + "/usr/bin",
		"FOO=bar",
	}
	got := PrependGoRootBin(base, goRoot)

	if !slices.Contains(got, "GOWORK=/repo/go.work") {
		t.Errorf("GOWORK must remain untouched: %v", got)
	}
	wantPath := "PATH=" + goBin + sep + "/usr/local/bin" + sep + "/usr/bin"
	if !slices.Contains(got, wantPath) {
		t.Errorf("PATH not prepended properly; got %v, want %q", got, wantPath)
	}

	// 原环境没有 PATH 时应补齐 PATH
	gotNoPath := PrependGoRootBin([]string{"FOO=bar"}, goRoot)
	if !slices.Contains(gotNoPath, "PATH="+goBin) {
		t.Errorf("expected PATH=%s when PATH absent, got %v", goBin, gotNoPath)
	}
}

func TestIsUnderGoRootToolDir(t *testing.T) {
	goRoot := filepath.Join("/usr", "local", "go")
	if !isUnderGoRootToolDir(goRoot, filepath.Join(goRoot, "pkg", "tool", "linux_amd64", "vet")) {
		t.Error("expected vet binary to be detected under GOROOT/pkg/tool")
	}
	if isUnderGoRootToolDir(goRoot, filepath.Join("/home", "user", ".cache", "go-build", "mytool")) {
		t.Error("cached custom tool should not be under GOROOT/pkg/tool")
	}
}

func TestParseToolBinaryPath(t *testing.T) {
	tmp := t.TempDir()
	binFile := filepath.Join(tmp, "mytool")
	if err := os.WriteFile(binFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if got := parseToolBinaryPath(binFile + "\n"); got != binFile {
		t.Errorf("parseToolBinaryPath = %q, want %q", got, binFile)
	}

	// 带引号路径
	if got := parseToolBinaryPath("'" + binFile + "'\n"); got != binFile {
		t.Errorf("parseToolBinaryPath(quoted) = %q, want %q", got, binFile)
	}
}
