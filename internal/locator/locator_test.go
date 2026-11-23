package locator

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLocateUpwards(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "pkg", "service", "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	gotMod := filepath.Join(root, DefaultModFileName)
	if err := os.WriteFile(gotMod, []byte("module got.local/tools\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write got.mod: %v", err)
	}

	// 无 go.mod 场景
	ctx, err := Locate(sub, "")
	if err != nil {
		t.Fatalf("Locate failed: %v", err)
	}
	if ctx.ModFile != gotMod {
		t.Errorf("ModFile = %q, want %q", ctx.ModFile, gotMod)
	}
	if ctx.RootDir != root {
		t.Errorf("RootDir = %q, want %q", ctx.RootDir, root)
	}
	if ctx.OrigWorkDir != sub {
		t.Errorf("OrigWorkDir = %q, want %q", ctx.OrigWorkDir, sub)
	}
	if ctx.HasGoMod {
		t.Error("HasGoMod = true, want false")
	}

	// 同级添加 go.mod 后应检测到共存状态
	goMod := filepath.Join(root, GoModFileName)
	if err := os.WriteFile(goMod, []byte("module example.com/app\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	ctx, err = Locate(sub, "")
	if err != nil {
		t.Fatalf("Locate with go.mod failed: %v", err)
	}
	if !ctx.HasGoMod {
		t.Error("HasGoMod = false, want true")
	}
}

func TestLocateWithGotFileEnv(t *testing.T) {
	base := t.TempDir()
	workDir := filepath.Join(base, "work")
	toolsDir := filepath.Join(base, "custom-tools")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir work: %v", err)
	}
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatalf("mkdir tools: %v", err)
	}

	customMod := filepath.Join(toolsDir, "custom.mod")
	if err := os.WriteFile(customMod, []byte("module got.local/custom\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write custom.mod: %v", err)
	}

	// 绝对路径 GOTFILE
	ctx, err := Locate(workDir, customMod)
	if err != nil {
		t.Fatalf("Locate absolute GOTFILE failed: %v", err)
	}
	if ctx.ModFile != customMod || ctx.RootDir != toolsDir || ctx.OrigWorkDir != workDir {
		t.Errorf("unexpected context: %+v", ctx)
	}

	// 相对路径 GOTFILE
	relPath := filepath.Join("..", "custom-tools", "custom.mod")
	ctx, err = Locate(workDir, relPath)
	if err != nil {
		t.Fatalf("Locate relative GOTFILE failed: %v", err)
	}
	if ctx.ModFile != customMod {
		t.Errorf("ModFile = %q, want %q", ctx.ModFile, customMod)
	}

	// 不存在的 GOTFILE
	_, err = Locate(workDir, filepath.Join(toolsDir, "missing.mod"))
	if !errors.Is(err, ErrModFileNotFound) {
		t.Fatalf("expected ErrModFileNotFound, got: %v", err)
	}
}

func TestLocateNotFound(t *testing.T) {
	emptyDir := t.TempDir()
	_, err := Locate(emptyDir, "")
	if !errors.Is(err, ErrModFileNotFound) {
		t.Fatalf("expected ErrModFileNotFound, got: %v", err)
	}
}

func TestResolveWorkDir(t *testing.T) {
	tmp := t.TempDir()
	got, err := ResolveWorkDir(tmp)
	if err != nil {
		t.Fatalf("ResolveWorkDir(%q) failed: %v", tmp, err)
	}
	if got != tmp {
		t.Errorf("ResolveWorkDir = %q, want %q", got, tmp)
	}

	filePath := filepath.Join(tmp, "notadir")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if _, err := ResolveWorkDir(filePath); err == nil {
		t.Fatal("expected error when -C points to a file")
	}
}
