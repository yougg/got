package main_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// probeToolSource 是一个测试用工具源码，能够打印自身版本、当前目录、环境变量、参数，
// 并支持按参数写入相对路径文件或以指定退出码退出。
const probeToolSourceV1 = `package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

type report struct {
	Version   string   ` + "`json:\"version\"`" + `
	Cwd       string   ` + "`json:\"cwd\"`" + `
	EnvGoWork string   ` + "`json:\"env_gowork\"`" + `
	GoEnvWork string   ` + "`json:\"go_env_gowork\"`" + `
	EnvGoOS   string   ` + "`json:\"env_goos\"`" + `
	EnvGoArch string   ` + "`json:\"env_goarch\"`" + `
	GoFlags   string   ` + "`json:\"goflags\"`" + `
	Path      string   ` + "`json:\"path\"`" + `
	Args      []string ` + "`json:\"args\"`" + `
}

func main() {
	writeFile := flag.String("write", "", "write marker file to relative path")
	exitCode := flag.Int("exit", 0, "exit with specified code")
	flag.Parse()

	if *exitCode != 0 {
		os.Exit(*exitCode)
	}

	cwd, _ := os.Getwd()
	if *writeFile != "" {
		content := fmt.Sprintf("generated-by=v1.0.0\ncwd=%s\n", cwd)
		if err := os.WriteFile(*writeFile, []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	out, _ := exec.Command("go", "env", "GOWORK").Output()
	rep := report{
		Version:   "v1.0.0",
		Cwd:       cwd,
		EnvGoWork: os.Getenv("GOWORK"),
		GoEnvWork: strings.TrimSpace(string(out)),
		EnvGoOS:   os.Getenv("GOOS"),
		EnvGoArch: os.Getenv("GOARCH"),
		GoFlags:   os.Getenv("GOFLAGS"),
		Path:      os.Getenv("PATH"),
		Args:      flag.Args(),
	}
	data, _ := json.Marshal(rep)
	fmt.Println(string(data))
}
`

const probeToolSourceV11 = `package main

import "fmt"

func main() {
	fmt.Println("version=v1.1.0")
}
`

const simpleToolSourceTemplate = `package main

import "fmt"

func main() {
	fmt.Println(%q)
}
`

type probeReport struct {
	Version   string   `json:"version"`
	Cwd       string   `json:"cwd"`
	EnvGoWork string   `json:"env_gowork"`
	GoEnvWork string   `json:"go_env_gowork"`
	EnvGoOS   string   `json:"env_goos"`
	EnvGoArch string   `json:"env_goarch"`
	GoFlags   string   `json:"goflags"`
	Path      string   `json:"path"`
	Args      []string `json:"args"`
}

type testHarness struct {
	gotBin     string
	proxyURL   string
	goModCache string
	goCache    string
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	binDir := t.TempDir()
	gotBin := filepath.Join(binDir, "got")
	if runtime.GOOS == "windows" {
		gotBin += ".exe"
	}
	buildCmd := exec.Command("go", "build", "-o", gotBin, ".")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build got binary failed: %v\n%s", err, string(out))
	}

	proxyDir := t.TempDir()
	publishFakeModule(t, proxyDir, "example.com/probe", "v1.0.0", map[string]string{
		"go.mod":                "module example.com/probe\n\ngo 1.24\n",
		"cmd/probetool/main.go": probeToolSourceV1,
	})
	publishFakeModule(t, proxyDir, "example.com/probe", "v1.1.0", map[string]string{
		"go.mod":                "module example.com/probe\n\ngo 1.24\n",
		"cmd/probetool/main.go": probeToolSourceV11,
	})
	publishFakeModule(t, proxyDir, "example.com/conflict", "v1.0.0", map[string]string{
		"go.mod":            "module example.com/conflict\n\ngo 1.24\n",
		"a/cmd/dup/main.go": fmt.Sprintf(simpleToolSourceTemplate, "from-a"),
		"b/cmd/dup/main.go": fmt.Sprintf(simpleToolSourceTemplate, "from-b"),
	})
	publishFakeModule(t, proxyDir, "example.com/helper", "v1.0.0", map[string]string{
		"go.mod":    "module example.com/helper\n\ngo 1.24\n",
		"helper.go": "package helper\n\nconst Version = \"helper-v1.0.0\"\n",
	})
	publishFakeModule(t, proxyDir, "example.com/helper", "v1.1.0", map[string]string{
		"go.mod":    "module example.com/helper\n\ngo 1.24\n",
		"helper.go": "package helper\n\nconst Version = \"helper-v1.1.0\"\n",
	})
	publishFakeModule(t, proxyDir, "example.com/biz", "v1.0.0", map[string]string{
		"go.mod": "module example.com/biz\n\ngo 1.24\n",
		"biz.go": "package biz\n\nconst Name = \"biz\"\n",
	})
	publishFakeModule(t, proxyDir, "example.com/bizfork", "v1.0.0", map[string]string{
		"go.mod": "module example.com/biz\n\ngo 1.24\n",
		"biz.go": "package biz\n\nconst Name = \"biz-fork\"\n",
	})
	// 模拟直接 replace 的工具模块及其传递依赖 replace 模块
	publishFakeModule(t, proxyDir, "example.com/forkdep", "v1.5.1", map[string]string{
		"go.mod":     "module example.com/origdep\n\ngo 1.24\n",
		"ext/ext.go": "package ext\n\nconst Tag = \"forkdep-v1.5.1\"\n",
	})
	publishFakeModule(t, proxyDir, "example.com/forktool", "v1.2.0", map[string]string{
		"go.mod":            "module example.com/origtool\n\ngo 1.24\n\nrequire example.com/origdep v1.5.0\n",
		"cmd/ftool/main.go": "package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/origdep/ext\"\n)\n\nfunc main() { fmt.Println(\"ftool:\" + ext.Tag) }\n",
	})

	// 复用宿主 GOCACHE 以加快集成测试编译速度，但隔离 GOMODCACHE 以防污染宿主机模块缓存
	goCacheBytes, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		t.Fatalf("go env GOCACHE: %v", err)
	}

	goModCache := t.TempDir()
	// Go 模块缓存默认将解压目录设为只读 (0555)，在 TempDir 清理前恢复写权限
	t.Cleanup(func() {
		_ = filepath.WalkDir(goModCache, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				_ = os.Chmod(p, 0o755)
			}
			return nil
		})
	})

	proxySlash := filepath.ToSlash(proxyDir)
	if !strings.HasPrefix(proxySlash, "/") {
		proxySlash = "/" + proxySlash
	}

	return &testHarness{
		gotBin:     gotBin,
		proxyURL:   "file://" + proxySlash,
		goModCache: goModCache,
		goCache:    strings.TrimSpace(string(goCacheBytes)),
	}
}

func sameRealPath(a, b string) bool {
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	if err1 == nil && err2 == nil {
		return filepath.Clean(ra) == filepath.Clean(rb)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func (h *testHarness) runGot(t *testing.T, dir string, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()

	cmd := exec.Command(h.gotBin, args...)
	cmd.Dir = dir

	baseEnv := make([]string, 0, len(os.Environ())+len(extraEnv)+4)
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		switch key {
		case "GOPROXY", "GOSUMDB", "GOMODCACHE", "GOCACHE", "GOWORK", "GOFLAGS", "GOTFILE", "GOOS", "GOARCH":
			continue
		default:
			baseEnv = append(baseEnv, kv)
		}
	}
	baseEnv = append(baseEnv,
		"GOPROXY="+h.proxyURL,
		"GOSUMDB=off",
		"GOMODCACHE="+h.goModCache,
		"GOCACHE="+h.goCache,
	)
	baseEnv = append(baseEnv, extraEnv...)
	cmd.Env = baseEnv

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return outBuf.String(), errBuf.String(), exitErr.ExitCode()
		}
		t.Fatalf("exec got %v failed: %v", args, err)
	}
	return outBuf.String(), errBuf.String(), 0
}

func publishFakeModule(t *testing.T, proxyDir, modPath, version string, files map[string]string) {
	t.Helper()

	vDir := filepath.Join(proxyDir, filepath.FromSlash(modPath), "@v")
	if err := os.MkdirAll(vDir, 0o755); err != nil {
		t.Fatalf("mkdir proxy dir: %v", err)
	}

	// 追加版本到 @v/list
	listPath := filepath.Join(vDir, "list")
	var existing string
	if data, err := os.ReadFile(listPath); err == nil {
		existing = string(data)
	}
	if err := os.WriteFile(listPath, []byte(existing+version+"\n"), 0o644); err != nil {
		t.Fatalf("write list: %v", err)
	}

	infoContent := fmt.Sprintf(`{"Version":%q,"Time":"2026-01-01T00:00:00Z"}`, version)
	if err := os.WriteFile(filepath.Join(vDir, version+".info"), []byte(infoContent), 0o644); err != nil {
		t.Fatalf("write info: %v", err)
	}

	goModContent, ok := files["go.mod"]
	if !ok {
		t.Fatalf("module %s@%s missing go.mod", modPath, version)
	}
	if err := os.WriteFile(filepath.Join(vDir, version+".mod"), []byte(goModContent), 0o644); err != nil {
		t.Fatalf("write mod: %v", err)
	}

	zipPath := filepath.Join(vDir, version+".zip")
	f, err := os.Create(zipPath)
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	prefix := modPath + "@" + version + "/"
	for name, content := range files {
		w, err := zw.Create(prefix + name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}
}

// 场景 1：标准单模块项目全流程（init / get -tool / list / tool -n / tool / get -u / rm）及主项目 go.mod 零修改。
func TestE2E_StandardSingleModule(t *testing.T) {
	h := newTestHarness(t)
	proj := t.TempDir()

	mainGoModPath := filepath.Join(proj, "go.mod")
	mainGoModOrig := "module example.com/mainapp\n\ngo 1.24\n"
	if err := os.WriteFile(mainGoModPath, []byte(mainGoModOrig), 0o644); err != nil {
		t.Fatalf("write main go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// got version
	stdout, stderr, code := h.runGot(t, proj, nil, "version")
	if code != 0 || !strings.Contains(stdout, "got version") {
		t.Fatalf("got version failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// got init
	_, stderr, code = h.runGot(t, proj, nil, "init")
	if code != 0 {
		t.Fatalf("got init failed: code=%d stderr=%q", code, stderr)
	}

	// 重复 got init 必须报错
	_, _, code = h.runGot(t, proj, nil, "init")
	if code == 0 {
		t.Fatal("second got init should fail when got.mod already exists")
	}

	// got get -tool
	_, stderr, code = h.runGot(t, proj, nil, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
	if code != 0 {
		t.Fatalf("got get -tool failed: code=%d stderr=%q", code, stderr)
	}

	// got list tool
	stdout, stderr, code = h.runGot(t, proj, nil, "list", "tool")
	if code != 0 || !strings.Contains(stdout, "example.com/probe/cmd/probetool") {
		t.Fatalf("got list tool failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// got tool (列表模式)
	stdout, stderr, code = h.runGot(t, proj, nil, "tool")
	if code != 0 || !strings.Contains(stdout, "example.com/probe/cmd/probetool") {
		t.Fatalf("got tool list failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// got tool -n
	stdout, stderr, code = h.runGot(t, proj, nil, "tool", "-n", "probetool", "arg1")
	if code != 0 || !strings.Contains(stdout, "arg1") {
		t.Fatalf("got tool -n failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// got tool 短名执行
	stdout, stderr, code = h.runGot(t, proj, nil, "tool", "probetool", "hello", "world")
	if code != 0 {
		t.Fatalf("got tool probetool failed: code=%d stderr=%q", code, stderr)
	}
	var rep probeReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
		t.Fatalf("unmarshal report %q: %v", stdout, err)
	}
	if rep.Version != "v1.0.0" || !sameRealPath(rep.Cwd, proj) {
		t.Errorf("unexpected report: %+v (want cwd=%q)", rep, proj)
	}
	if len(rep.Args) != 2 || rep.Args[0] != "hello" || rep.Args[1] != "world" {
		t.Errorf("unexpected args: %v", rep.Args)
	}

	// 退出码非 0 静默透传（不输出 got: wrapper 错误）
	_, stderr, code = h.runGot(t, proj, nil, "tool", "probetool", "-exit=42")
	if code != 42 {
		t.Fatalf("expected exit code 42, got %d (stderr=%q)", code, stderr)
	}
	if strings.Contains(stderr, "got:") {
		t.Errorf("non-zero tool exit should not print wrapper error, got stderr=%q", stderr)
	}

	// got get -u（无包参数，自动补充 tool 升级至 v1.1.0）
	_, stderr, code = h.runGot(t, proj, nil, "get", "-u")
	if code != 0 {
		t.Fatalf("got get -u failed: code=%d stderr=%q", code, stderr)
	}
	stdout, stderr, code = h.runGot(t, proj, nil, "tool", "probetool")
	if code != 0 || strings.TrimSpace(stdout) != "version=v1.1.0" {
		t.Fatalf("expected upgraded tool v1.1.0, got code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// got rm probetool
	_, stderr, code = h.runGot(t, proj, nil, "rm", "probetool")
	if code != 0 {
		t.Fatalf("got rm failed: code=%d stderr=%q", code, stderr)
	}
	gotModAfter, err := os.ReadFile(filepath.Join(proj, "got.mod"))
	if err != nil {
		t.Fatalf("read got.mod: %v", err)
	}
	if strings.Contains(string(gotModAfter), "example.com/probe") {
		t.Errorf("expected probe tool removed from got.mod, got:\n%s", string(gotModAfter))
	}

	// 验证主项目 go.mod 零修改且未生成 go.sum
	mainGoModAfter, err := os.ReadFile(mainGoModPath)
	if err != nil {
		t.Fatalf("read main go.mod: %v", err)
	}
	if string(mainGoModAfter) != mainGoModOrig {
		t.Errorf("main go.mod was modified!\nwant:\n%s\ngot:\n%s", mainGoModOrig, string(mainGoModAfter))
	}
	if _, err := os.Stat(filepath.Join(proj, "go.sum")); !os.IsNotExist(err) {
		t.Errorf("main go.sum should not be created")
	}
}

// 场景 2：go.work Workspace 项目，验证编译不报错且工具子进程实际读取原始 GOWORK。
func TestE2E_WorkspaceGoWorkPreserved(t *testing.T) {
	h := newTestHarness(t)
	repo := t.TempDir()
	modA := filepath.Join(repo, "modA")
	if err := os.MkdirAll(modA, 0o755); err != nil {
		t.Fatalf("mkdir modA: %v", err)
	}

	goWorkPath := filepath.Join(repo, "go.work")
	if err := os.WriteFile(goWorkPath, []byte("go 1.24\n\nuse ./modA\n"), 0o644); err != nil {
		t.Fatalf("write go.work: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modA, "go.mod"), []byte("module example.com/moda\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write modA/go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(modA, "a.go"), []byte("package moda\n"), 0o644); err != nil {
		t.Fatalf("write modA/a.go: %v", err)
	}

	// 在无 go.mod 的 workspace 根目录初始化 got.mod 并安装工具
	_, stderr, code := h.runGot(t, repo, nil, "init")
	if code != 0 {
		t.Fatalf("got init in workspace failed: code=%d stderr=%q", code, stderr)
	}
	_, stderr, code = h.runGot(t, repo, nil, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
	if code != 0 {
		t.Fatalf("got get -tool in workspace failed: code=%d stderr=%q", code, stderr)
	}

	// 在子模块 modA 目录下执行 got tool，同时显式设置 GOWORK 环境变量
	stdout, stderr, code := h.runGot(t, modA, []string{"GOWORK=" + goWorkPath}, "tool", "probetool")
	if code != 0 {
		t.Fatalf("got tool in workspace failed: code=%d stderr=%q", code, stderr)
	}

	var rep probeReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
		t.Fatalf("unmarshal report %q: %v", stdout, err)
	}
	if rep.EnvGoWork != goWorkPath {
		t.Errorf("child process GOWORK env = %q, want %q", rep.EnvGoWork, goWorkPath)
	}
	if rep.GoEnvWork != goWorkPath {
		t.Errorf("child process `go env GOWORK` = %q, want %q", rep.GoEnvWork, goWorkPath)
	}
	if !sameRealPath(rep.Cwd, modA) {
		t.Errorf("child process Cwd = %q, want %q", rep.Cwd, modA)
	}
}

// 场景 3：无 go.mod 纯目录，验证 -overlay 自动占位与无残留文件。
func TestE2E_NoGoModOverlay(t *testing.T) {
	h := newTestHarness(t)
	dir := t.TempDir()

	// 未初始化 got.mod 时，got get 应默认自动初始化并完成工具拉取
	_, stderr, code := h.runGot(t, dir, nil, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
	if code != 0 {
		t.Fatalf("got get auto-init failed: code=%d stderr=%q", code, stderr)
	}

	// 已取消 --init 参数，传入应报错
	_, _, initFlagCode := h.runGot(t, t.TempDir(), nil, "get", "--init", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
	if initFlagCode == 0 {
		t.Fatal("expected 'got get --init' to fail after removing --init flag")
	}

	stdout, stderr, code := h.runGot(t, dir, nil, "tool", "probetool")
	if code != 0 {
		t.Fatalf("got tool in no-go.mod dir failed: code=%d stderr=%q", code, stderr)
	}
	var rep probeReport
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
		t.Fatalf("unmarshal report %q: %v", stdout, err)
	}
	if rep.Version != "v1.0.0" {
		t.Errorf("version = %q, want v1.0.0", rep.Version)
	}

	// got mod verify 同样借助 overlay 正常工作
	_, stderr, code = h.runGot(t, dir, nil, "mod", "verify")
	if code != 0 {
		t.Fatalf("got mod verify failed: code=%d stderr=%q", code, stderr)
	}

	// 确认目录内不存在 go.mod 残留
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); !os.IsNotExist(err) {
		t.Errorf("no go.mod file should be left in directory")
	}
}

// 场景 4：//go:generate 子目录调用，验证子包 Cwd 保持与相对路径输出。
func TestE2E_GoGenerateSubdir(t *testing.T) {
	h := newTestHarness(t)
	proj := t.TempDir()
	subPkg := filepath.Join(proj, "pkg", "gen")
	if err := os.MkdirAll(subPkg, 0o755); err != nil {
		t.Fatalf("mkdir subpkg: %v", err)
	}

	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/genapp\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	genGoSrc := "package gen\n\n//go:generate got tool probetool -write=generated.txt\n"
	if err := os.WriteFile(filepath.Join(subPkg, "gen.go"), []byte(genGoSrc), 0o644); err != nil {
		t.Fatalf("write gen.go: %v", err)
	}

	_, stderr, code := h.runGot(t, proj, nil, "init")
	if code != 0 {
		t.Fatalf("got init: code=%d stderr=%q", code, stderr)
	}
	_, stderr, code = h.runGot(t, proj, nil, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
	if code != 0 {
		t.Fatalf("got get: code=%d stderr=%q", code, stderr)
	}

	// 将编译出的 got 二进制所在目录放到 PATH 前方后运行 go generate ./...
	genCmd := exec.Command("go", "generate", "./...")
	genCmd.Dir = proj
	genEnv := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		key, val, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(key, "PATH") {
			genEnv = append(genEnv, key+"="+filepath.Dir(h.gotBin)+string(filepath.ListSeparator)+val)
			continue
		}
		switch strings.ToUpper(key) {
		case "GOPROXY", "GOSUMDB", "GOMODCACHE", "GOCACHE", "GOWORK", "GOFLAGS", "GOTFILE", "GOOS", "GOARCH":
			continue
		default:
			genEnv = append(genEnv, kv)
		}
	}
	genEnv = append(genEnv,
		"GOPROXY="+h.proxyURL,
		"GOSUMDB=off",
		"GOMODCACHE="+h.goModCache,
		"GOCACHE="+h.goCache,
	)
	genCmd.Env = genEnv

	if out, err := genCmd.CombinedOutput(); err != nil {
		t.Fatalf("go generate ./... failed: %v\n%s", err, string(out))
	}

	generatedPath := filepath.Join(subPkg, "generated.txt")
	data, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatalf("expected generated file at %s: %v", generatedPath, err)
	}
	var gotCwd string
	for line := range strings.SplitSeq(string(data), "\n") {
		if val, ok := strings.CutPrefix(strings.TrimSpace(line), "cwd="); ok {
			gotCwd = val
			break
		}
	}
	if !sameRealPath(gotCwd, subPkg) {
		t.Errorf("generated file has unexpected cwd %q (want %q):\n%s", gotCwd, subPkg, string(data))
	}
}

// 场景 5：Go 内置工具透传（含无 got.mod 与有 got.mod 两种情况）。
func TestE2E_BuiltinToolPassthrough(t *testing.T) {
	h := newTestHarness(t)
	proj := t.TempDir()

	if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/vetapp\n\ngo 1.24\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	// 无 got.mod 时，内置工具 vet 应直接透传成功
	_, stderr, code := h.runGot(t, proj, nil, "tool", "vet", "-help")
	if code != 0 && !strings.Contains(stderr, "Usage") && !strings.Contains(stderr, "vet") {
		t.Fatalf("got tool vet -help without got.mod failed unexpectedly: code=%d stderr=%q", code, stderr)
	}

	// 无 got.mod 时，非内置工具应提示未找到 got.mod
	_, stderr, code = h.runGot(t, proj, nil, "tool", "custom-missing-tool")
	if code == 0 || !strings.Contains(stderr, "got.mod not found") {
		t.Fatalf("expected got.mod not found error for custom tool, got code=%d stderr=%q", code, stderr)
	}

	// 初始化 got.mod 后，内置工具同样正常透传
	_, stderr, code = h.runGot(t, proj, nil, "init")
	if code != 0 {
		t.Fatalf("got init failed: code=%d stderr=%q", code, stderr)
	}
	_, stderr, code = h.runGot(t, proj, nil, "tool", "vet", "-help")
	if code != 0 && !strings.Contains(stderr, "Usage") && !strings.Contains(stderr, "vet") {
		t.Fatalf("got tool vet -help with got.mod failed unexpectedly: code=%d stderr=%q", code, stderr)
	}
}

// 场景 6：默认复用主模块路径与本地工具、显式自定义模块路径、GOFLAGS=-mod=vendor 清洗、歧义检测、-C 与 GOTFILE。
func TestE2E_AdvancedEdgeCases(t *testing.T) {
	h := newTestHarness(t)

	t.Run("init module path resolution and local tool", func(t *testing.T) {
		// 1. 存在 go.mod 且未指定 module-path 时，默认复用 go.mod 的模块路径（直接支持仓库本地工具）
		proj := t.TempDir()
		if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/myrepo\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		localToolDir := filepath.Join(proj, "cmd", "localgen")
		if err := os.MkdirAll(localToolDir, 0o755); err != nil {
			t.Fatalf("mkdir cmd/localgen: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localToolDir, "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"localgen-ok\") }\n"), 0o644); err != nil {
			t.Fatalf("write localgen: %v", err)
		}

		_, stderr, code := h.runGot(t, proj, nil, "init")
		if code != 0 {
			t.Fatalf("got init failed: code=%d stderr=%q", code, stderr)
		}
		gotModBytes, err := os.ReadFile(filepath.Join(proj, "got.mod"))
		if err != nil {
			t.Fatalf("read got.mod: %v", err)
		}
		if !strings.Contains(string(gotModBytes), "module example.com/myrepo\n") {
			t.Errorf("expected got.mod to default to sibling go.mod module path, got:\n%s", string(gotModBytes))
		}
		_, stderr, code = h.runGot(t, proj, nil, "mod", "edit", "-tool", "example.com/myrepo/cmd/localgen")
		if code != 0 {
			t.Fatalf("got mod edit -tool failed: code=%d stderr=%q", code, stderr)
		}
		stdout, stderr, code := h.runGot(t, proj, nil, "tool", "localgen")
		if code != 0 || strings.TrimSpace(stdout) != "localgen-ok" {
			t.Fatalf("got tool localgen failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}

		// 2. 存在 go.mod 但显式指定了不同的 module-path 时，应使用指定值
		customProj := t.TempDir()
		if err := os.WriteFile(filepath.Join(customProj, "go.mod"), []byte("module example.com/origapp\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		_, stderr, code = h.runGot(t, customProj, nil, "init", "custom.local/mytools")
		if code != 0 {
			t.Fatalf("got init custom.local/mytools failed: code=%d stderr=%q", code, stderr)
		}
		customModBytes, err := os.ReadFile(filepath.Join(customProj, "got.mod"))
		if err != nil {
			t.Fatalf("read custom got.mod: %v", err)
		}
		if !strings.Contains(string(customModBytes), "module custom.local/mytools\n") {
			t.Errorf("expected got.mod to use explicitly specified module path, got:\n%s", string(customModBytes))
		}

		// 3. 无 go.mod 且未指定 module-path 时，应使用默认值 got.local/tools
		noGoModDir := t.TempDir()
		_, stderr, code = h.runGot(t, noGoModDir, nil, "init")
		if code != 0 {
			t.Fatalf("got init in no-go.mod dir failed: code=%d stderr=%q", code, stderr)
		}
		defaultModBytes, err := os.ReadFile(filepath.Join(noGoModDir, "got.mod"))
		if err != nil {
			t.Fatalf("read default got.mod: %v", err)
		}
		if !strings.Contains(string(defaultModBytes), "module got.local/tools\n") {
			t.Errorf("expected got.mod to fall back to got.local/tools, got:\n%s", string(defaultModBytes))
		}
	})

	t.Run("GOFLAGS mod=vendor sanitized and preserved for child", func(t *testing.T) {
		proj := t.TempDir()
		if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/vendapp\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		_, stderr, code := h.runGot(t, proj, []string{"GOFLAGS=-mod=vendor"}, "init")
		if code != 0 {
			t.Fatalf("got init failed: code=%d stderr=%q", code, stderr)
		}
		_, stderr, code = h.runGot(t, proj, []string{"GOFLAGS=-mod=vendor"}, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
		if code != 0 {
			t.Fatalf("got get under GOFLAGS=-mod=vendor failed: code=%d stderr=%q", code, stderr)
		}
		stdout, stderr, code := h.runGot(t, proj, []string{"GOFLAGS=-mod=vendor"}, "tool", "probetool")
		if code != 0 {
			t.Fatalf("got tool under GOFLAGS=-mod=vendor failed: code=%d stderr=%q", code, stderr)
		}
		var rep probeReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
			t.Fatalf("unmarshal report %q: %v", stdout, err)
		}
		// 阶段二应保留用户原始 GOFLAGS 给工具进程
		if rep.GoFlags != "-mod=vendor" {
			t.Errorf("child GOFLAGS = %q, want -mod=vendor", rep.GoFlags)
		}
	})

	t.Run("ambiguous tool names and full path disambiguation", func(t *testing.T) {
		proj := t.TempDir()
		_, stderr, code := h.runGot(t, proj, nil, "init")
		if code != 0 {
			t.Fatalf("got init: code=%d stderr=%q", code, stderr)
		}
		_, stderr, code = h.runGot(t, proj, nil, "get", "-tool", "example.com/conflict/a/cmd/dup@v1.0.0", "example.com/conflict/b/cmd/dup@v1.0.0")
		if code != 0 {
			t.Fatalf("got get conflict tools: code=%d stderr=%q", code, stderr)
		}

		// 短名调用必须报歧义错误并列出所有冲突路径
		_, stderr, code = h.runGot(t, proj, nil, "tool", "dup")
		if code == 0 {
			t.Fatal("expected ambiguity error for 'got tool dup'")
		}
		if !strings.Contains(stderr, "example.com/conflict/a/cmd/dup") || !strings.Contains(stderr, "example.com/conflict/b/cmd/dup") {
			t.Errorf("ambiguity error should list both paths, got:\n%s", stderr)
		}

		// 全路径调用必须精确命中
		stdout, stderr, code := h.runGot(t, proj, nil, "tool", "example.com/conflict/b/cmd/dup")
		if code != 0 || strings.TrimSpace(stdout) != "from-b" {
			t.Fatalf("full path invocation failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("-C flag and GOTFILE override", func(t *testing.T) {
		base := t.TempDir()
		proj := filepath.Join(base, "proj")
		other := filepath.Join(base, "other")
		if err := os.MkdirAll(proj, 0o755); err != nil {
			t.Fatalf("mkdir proj: %v", err)
		}
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatalf("mkdir other: %v", err)
		}

		_, stderr, code := h.runGot(t, other, nil, "-C", proj, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
		if code != 0 {
			t.Fatalf("got -C proj get failed: code=%d stderr=%q", code, stderr)
		}

		// 从 other 目录通过 -C proj 执行，Cwd 应为 proj
		stdout, stderr, code := h.runGot(t, other, nil, "-C", proj, "tool", "probetool")
		if code != 0 {
			t.Fatalf("got -C proj tool failed: code=%d stderr=%q", code, stderr)
		}
		var rep probeReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
			t.Fatalf("unmarshal report: %v", err)
		}
		if !sameRealPath(rep.Cwd, proj) {
			t.Errorf("Cwd with -C = %q, want %q", rep.Cwd, proj)
		}

		// 从 other 目录通过 GOTFILE 执行，Cwd 应保持为 other
		gotModPath := filepath.Join(proj, "got.mod")
		stdout, stderr, code = h.runGot(t, other, []string{"GOTFILE=" + gotModPath}, "tool", "probetool")
		if code != 0 {
			t.Fatalf("got with GOTFILE failed: code=%d stderr=%q", code, stderr)
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
			t.Fatalf("unmarshal report: %v", err)
		}
		if !sameRealPath(rep.Cwd, other) {
			t.Errorf("Cwd with GOTFILE = %q, want %q", rep.Cwd, other)
		}
	})

	t.Run("exe alias for tool", func(t *testing.T) {
		proj := t.TempDir()
		other := t.TempDir()

		_, stderr, code := h.runGot(t, proj, nil, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
		if code != 0 {
			t.Fatalf("got get failed: code=%d stderr=%q", code, stderr)
		}

		// got exe 列表模式
		stdout, stderr, code := h.runGot(t, proj, nil, "exe")
		if code != 0 || !strings.Contains(stdout, "example.com/probe/cmd/probetool") {
			t.Fatalf("got exe list failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}

		// got exe -n
		stdout, stderr, code = h.runGot(t, proj, nil, "exe", "-n", "probetool", "arg1")
		if code != 0 || !strings.Contains(stdout, "arg1") {
			t.Fatalf("got exe -n failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}

		// got exe 执行工具（兼容子命令后置 -C）
		stdout, stderr, code = h.runGot(t, other, nil, "exe", "-C", proj, "probetool", "foo", "bar")
		if code != 0 {
			t.Fatalf("got exe -C proj probetool failed: code=%d stderr=%q", code, stderr)
		}
		var rep probeReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
			t.Fatalf("unmarshal report %q: %v", stdout, err)
		}
		if rep.Version != "v1.0.0" || !sameRealPath(rep.Cwd, proj) || len(rep.Args) != 2 || rep.Args[0] != "foo" || rep.Args[1] != "bar" {
			t.Errorf("unexpected report from got exe: %+v (want cwd=%q)", rep, proj)
		}

		// got exe 透传 Go 内置工具
		_, stderr, code = h.runGot(t, other, nil, "exe", "vet", "-help")
		if code != 0 && !strings.Contains(stderr, "Usage") && !strings.Contains(stderr, "vet") {
			t.Fatalf("got exe vet -help failed unexpectedly: code=%d stderr=%q", code, stderr)
		}
	})

	t.Run("cross-compile GOOS/GOARCH builds host binary and preserves child env", func(t *testing.T) {
		proj := t.TempDir()
		if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/crossapp\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}

		// 构造与当前宿主机必然不同的 GOOS 与 GOARCH 组合
		targetOS := "windows"
		if runtime.GOOS == "windows" {
			targetOS = "linux"
		}
		targetArch := "arm64"
		if runtime.GOARCH == "arm64" {
			targetArch = "amd64"
		}
		crossEnv := []string{
			"GOOS=" + targetOS,
			"GOARCH=" + targetArch,
		}

		_, stderr, code := h.runGot(t, proj, crossEnv, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
		if code != 0 {
			t.Fatalf("got get under cross-compile env failed: code=%d stderr=%q", code, stderr)
		}

		// 阶段一必须按宿主机 GOHOSTOS/GOHOSTARCH 编译（不会出现 exec format error），
		// 阶段二执行工具时仍须保留用户原始的 GOOS/GOARCH。
		for _, sub := range []string{"tool", "exe"} {
			stdout, stderr, code := h.runGot(t, proj, crossEnv, sub, "probetool")
			if code != 0 {
				t.Fatalf("got %s probetool under GOOS=%s GOARCH=%s failed: code=%d stderr=%q",
					sub, targetOS, targetArch, code, stderr)
			}
			var rep probeReport
			if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
				t.Fatalf("unmarshal report %q: %v", stdout, err)
			}
			if rep.Version != "v1.0.0" {
				t.Errorf("got %s version = %q, want v1.0.0", sub, rep.Version)
			}
			if rep.EnvGoOS != targetOS || rep.EnvGoArch != targetArch {
				t.Errorf("got %s child env GOOS/GOARCH = (%q, %q), want (%q, %q)",
					sub, rep.EnvGoOS, rep.EnvGoArch, targetOS, targetArch)
			}
		}
	})

	t.Run("init migrates external tools from go.mod with exact versions", func(t *testing.T) {
		proj := t.TempDir()
		mainGoMod := strings.Join([]string{
			"module example.com/migapp",
			"",
			"go 1.24",
			"",
			"tool (",
			"\texample.com/probe/cmd/probetool",
			"\texample.com/conflict/a/cmd/dup",
			")",
			"",
			"require (",
			"\texample.com/biz v1.0.0",
			"\texample.com/conflict v1.0.0 // indirect",
			"\texample.com/probe v1.0.0 // indirect",
			")",
			"",
		}, "\n")
		mainGoModPath := filepath.Join(proj, "go.mod")
		if err := os.WriteFile(mainGoModPath, []byte(mainGoMod), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\n\nimport _ \"example.com/biz\"\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatalf("write main.go: %v", err)
		}

		// 执行 got init，应自动复制 go.mod 中的 tool 列表并按 v1.0.0 精确拉取（而非升级到 v1.1.0）
		_, stderr, code := h.runGot(t, proj, nil, "init")
		if code != 0 {
			t.Fatalf("got init failed: code=%d stderr=%q", code, stderr)
		}

		gotModBytes, err := os.ReadFile(filepath.Join(proj, "got.mod"))
		if err != nil {
			t.Fatalf("read got.mod: %v", err)
		}
		gotModStr := string(gotModBytes)
		if !strings.Contains(gotModStr, "example.com/probe/cmd/probetool") || !strings.Contains(gotModStr, "example.com/conflict/a/cmd/dup") {
			t.Errorf("got.mod missing migrated tools:\n%s", gotModStr)
		}
		if !strings.Contains(gotModStr, "example.com/probe v1.0.0") {
			t.Errorf("expected exact version example.com/probe v1.0.0 in got.mod, got:\n%s", gotModStr)
		}
		if strings.Contains(gotModStr, "example.com/biz") {
			t.Errorf("business dependency example.com/biz must not leak into got.mod:\n%s", gotModStr)
		}
		if _, err := os.Stat(filepath.Join(proj, "got.sum")); err != nil {
			t.Errorf("expected got.sum to be initialized: %v", err)
		}

		// 验证主项目 go.mod 未被修改
		mainAfter, err := os.ReadFile(mainGoModPath)
		if err != nil {
			t.Fatalf("read main go.mod: %v", err)
		}
		if string(mainAfter) != mainGoMod {
			t.Errorf("main go.mod was modified during got init")
		}

		// 验证 got mod verify 与直接执行两个工具均正常
		_, stderr, code = h.runGot(t, proj, nil, "mod", "verify")
		if code != 0 {
			t.Fatalf("got mod verify failed: code=%d stderr=%q", code, stderr)
		}

		stdout, stderr, code := h.runGot(t, proj, nil, "tool", "probetool")
		if code != 0 {
			t.Fatalf("got tool probetool failed: code=%d stderr=%q", code, stderr)
		}
		var rep probeReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
			t.Fatalf("unmarshal report %q: %v", stdout, err)
		}
		if rep.Version != "v1.0.0" {
			t.Errorf("migrated probetool version = %q, want v1.0.0", rep.Version)
		}

		stdout, stderr, code = h.runGot(t, proj, nil, "exe", "dup")
		if code != 0 || strings.TrimSpace(stdout) != "from-a" {
			t.Fatalf("got exe dup failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("init migrates mixed local and external tools from go.mod", func(t *testing.T) {
		proj := t.TempDir()
		mainGoMod := strings.Join([]string{
			"module example.com/mixapp",
			"",
			"go 1.24",
			"",
			"tool (",
			"\texample.com/mixapp/cmd/localgen",
			"\texample.com/probe/cmd/probetool",
			")",
			"",
			"require (",
			"\texample.com/biz v1.0.0",
			"\texample.com/helper v1.0.0",
			"\texample.com/probe v1.0.0 // indirect",
			")",
			"",
		}, "\n")
		if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte(mainGoMod), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\n\nimport _ \"example.com/biz\"\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatalf("write main.go: %v", err)
		}
		localToolDir := filepath.Join(proj, "cmd", "localgen")
		if err := os.MkdirAll(localToolDir, 0o755); err != nil {
			t.Fatalf("mkdir cmd/localgen: %v", err)
		}
		localToolSrc := "package main\n\nimport (\n\t\"fmt\"\n\t\"example.com/helper\"\n)\n\nfunc main() { fmt.Println(helper.Version) }\n"
		if err := os.WriteFile(filepath.Join(localToolDir, "main.go"), []byte(localToolSrc), 0o644); err != nil {
			t.Fatalf("write cmd/localgen/main.go: %v", err)
		}

		// 先在主项目中生成完整 go.sum，确保主项目 go list -deps 正常解析
		tidyCmd := exec.Command("go", "mod", "tidy")
		tidyCmd.Dir = proj
		tidyCmd.Env = append(os.Environ(),
			"GOPROXY="+h.proxyURL,
			"GOSUMDB=off",
			"GOMODCACHE="+h.goModCache,
			"GOCACHE="+h.goCache,
			"GOWORK=off",
		)
		if out, err := tidyCmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare main go mod tidy failed: %v\n%s", err, string(out))
		}

		_, stderr, code := h.runGot(t, proj, nil, "init")
		if code != 0 {
			t.Fatalf("got init failed: code=%d stderr=%q", code, stderr)
		}

		gotModBytes, err := os.ReadFile(filepath.Join(proj, "got.mod"))
		if err != nil {
			t.Fatalf("read got.mod: %v", err)
		}
		gotModStr := string(gotModBytes)
		if !strings.Contains(gotModStr, "module example.com/mixapp") {
			t.Errorf("expected got.mod to adopt main module path for local tool:\n%s", gotModStr)
		}
		if !strings.Contains(gotModStr, "example.com/helper v1.0.0") {
			t.Errorf("expected exact helper v1.0.0 in got.mod:\n%s", gotModStr)
		}
		if strings.Contains(gotModStr, "example.com/biz") {
			t.Errorf("business dependency example.com/biz must not leak into got.mod:\n%s", gotModStr)
		}

		stdout, stderr, code := h.runGot(t, proj, nil, "tool", "localgen")
		if code != 0 || strings.TrimSpace(stdout) != "helper-v1.0.0" {
			t.Fatalf("got tool localgen failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("init migrates direct and transitive replaces for tools without leaking business replaces", func(t *testing.T) {
		proj := t.TempDir()
		mainGoMod := strings.Join([]string{
			"module example.com/repapp",
			"",
			"go 1.24",
			"",
			"tool example.com/origtool/cmd/ftool",
			"",
			"require (",
			"\texample.com/biz v1.0.0",
			"\texample.com/origdep v1.5.0 // indirect",
			"\texample.com/origtool v0.0.0-00010101000000-000000000000 // indirect",
			")",
			"",
			"replace (",
			"\texample.com/biz => example.com/bizfork v1.0.0",
			"\texample.com/origdep => example.com/forkdep v1.5.1",
			"\texample.com/origtool => example.com/forktool v1.2.0",
			")",
			"",
		}, "\n")
		if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte(mainGoMod), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\n\nimport _ \"example.com/biz\"\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatalf("write main.go: %v", err)
		}

		// 先为测试项目补齐主项目 go.sum
		tidyCmd := exec.Command("go", "mod", "tidy")
		tidyCmd.Dir = proj
		tidyCmd.Env = append(os.Environ(),
			"GOPROXY="+h.proxyURL,
			"GOSUMDB=off",
			"GOMODCACHE="+h.goModCache,
			"GOCACHE="+h.goCache,
			"GOWORK=off",
		)
		if out, err := tidyCmd.CombinedOutput(); err != nil {
			t.Fatalf("prepare main go mod tidy failed: %v\n%s", err, string(out))
		}

		_, stderr, code := h.runGot(t, proj, nil, "init")
		if code != 0 {
			t.Fatalf("got init with direct+transitive replace failed: code=%d stderr=%q", code, stderr)
		}

		gotModBytes, err := os.ReadFile(filepath.Join(proj, "got.mod"))
		if err != nil {
			t.Fatalf("read got.mod: %v", err)
		}
		gotModStr := string(gotModBytes)
		if !strings.Contains(gotModStr, "example.com/origtool => example.com/forktool v1.2.0") {
			t.Errorf("missing direct tool replace in got.mod:\n%s", gotModStr)
		}
		if !strings.Contains(gotModStr, "example.com/origdep => example.com/forkdep v1.5.1") {
			t.Errorf("missing transitive dep replace in got.mod:\n%s", gotModStr)
		}
		if strings.Contains(gotModStr, "example.com/bizfork") || strings.Contains(gotModStr, "example.com/biz") {
			t.Errorf("business replace must not leak into got.mod:\n%s", gotModStr)
		}

		stdout, stderr, code := h.runGot(t, proj, nil, "tool", "ftool")
		if code != 0 || strings.TrimSpace(stdout) != "ftool:forkdep-v1.5.1" {
			t.Fatalf("got tool ftool failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})

	t.Run("mod tidy and rm run in sandbox without scanning main project source files", func(t *testing.T) {
		proj := t.TempDir()
		if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/bizapp\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		// 构造一段引用无法在 got.mod 中解析的业务代码；若 got mod tidy 扫描了项目目录必然报错
		brokenDir := filepath.Join(proj, "pkg", "biz")
		if err := os.MkdirAll(brokenDir, 0o755); err != nil {
			t.Fatalf("mkdir pkg/biz: %v", err)
		}
		brokenSrc := "package biz\n\nimport _ \"example.com/unreachable-biz-only/subpkg\"\n"
		if err := os.WriteFile(filepath.Join(brokenDir, "biz.go"), []byte(brokenSrc), 0o644); err != nil {
			t.Fatalf("write pkg/biz/biz.go: %v", err)
		}

		// 同时准备一个本地相对路径 replace 模块，验证沙箱 tidy 保持相对路径 replace
		localRepDir := filepath.Join(proj, "localrep")
		if err := os.MkdirAll(filepath.Join(localRepDir, "cmd", "ltool"), 0o755); err != nil {
			t.Fatalf("mkdir localrep: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRepDir, "go.mod"), []byte("module example.com/localrep\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write localrep/go.mod: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRepDir, "cmd", "ltool", "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"ltool-ok\") }\n"), 0o644); err != nil {
			t.Fatalf("write localrep main.go: %v", err)
		}

		gotModInitial := strings.Join([]string{
			"module got.local/tools",
			"",
			"go 1.24",
			"",
			"tool (",
			"\texample.com/probe/cmd/probetool",
			"\texample.com/localrep/cmd/ltool",
			")",
			"",
			"require (",
			"\texample.com/biz v1.0.0 // indirect",
			"\texample.com/localrep v0.0.0-00010101000000-000000000000 // indirect",
			"\texample.com/probe v1.0.0 // indirect",
			")",
			"",
			"replace example.com/localrep => ./localrep",
			"",
		}, "\n")
		gotModPath := filepath.Join(proj, "got.mod")
		if err := os.WriteFile(gotModPath, []byte(gotModInitial), 0o644); err != nil {
			t.Fatalf("write got.mod: %v", err)
		}

		// -diff 只读模式存在差异时应返回非 0 退出码、打印 diff 且不修改 got.mod
		diffOut, stderr, code := h.runGot(t, proj, nil, "mod", "tidy", "-diff")
		if code == 0 || !strings.Contains(diffOut, "example.com/biz") {
			t.Fatalf("got mod tidy -diff unexpected result: code=%d stdout=%q stderr=%q", code, diffOut, stderr)
		}
		afterDiff, err := os.ReadFile(gotModPath)
		if err != nil {
			t.Fatalf("read got.mod after -diff: %v", err)
		}
		if string(afterDiff) != gotModInitial {
			t.Errorf("got mod tidy -diff should not modify got.mod")
		}

		// 正式执行 got mod tidy：应清理冗余 example.com/biz、保留相对路径 replace、且不扫描 pkg/biz/biz.go
		_, stderr, code = h.runGot(t, proj, nil, "mod", "tidy")
		if code != 0 {
			t.Fatalf("got mod tidy failed: code=%d stderr=%q", code, stderr)
		}
		// tidy 完成后再次 -diff 应返回 0
		_, stderr, code = h.runGot(t, proj, nil, "mod", "tidy", "-diff")
		if code != 0 {
			t.Fatalf("got mod tidy -diff after tidy failed: code=%d stderr=%q", code, stderr)
		}
		afterTidyBytes, err := os.ReadFile(gotModPath)
		if err != nil {
			t.Fatalf("read got.mod after tidy: %v", err)
		}
		afterTidy := string(afterTidyBytes)
		if strings.Contains(afterTidy, "example.com/biz") || strings.Contains(afterTidy, "unreachable-biz-only") {
			t.Errorf("unexpected business dependency in tidied got.mod:\n%s", afterTidy)
		}
		if !strings.Contains(afterTidy, "replace example.com/localrep => ./localrep") {
			t.Errorf("relative replace should be preserved after tidy:\n%s", afterTidy)
		}

		// 验证相对路径 replace 工具可正常执行
		stdout, stderr, code := h.runGot(t, proj, nil, "tool", "ltool")
		if code != 0 || strings.TrimSpace(stdout) != "ltool-ok" {
			t.Fatalf("got tool ltool failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}

		// 验证 got rm（内部会调用 tidy）同样不会扫描 pkg/biz/biz.go
		_, stderr, code = h.runGot(t, proj, nil, "rm", "probetool")
		if code != 0 {
			t.Fatalf("got rm probetool failed: code=%d stderr=%q", code, stderr)
		}
		afterRmBytes, err := os.ReadFile(gotModPath)
		if err != nil {
			t.Fatalf("read got.mod after rm: %v", err)
		}
		if strings.Contains(string(afterRmBytes), "example.com/probe") {
			t.Errorf("expected probe removed after got rm:\n%s", string(afterRmBytes))
		}
	})

	t.Run("symlink directory without go.mod works with -C overlay", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation requires elevated privileges on Windows")
		}
		realProj := t.TempDir()
		linkParent := t.TempDir()
		linkProj := filepath.Join(linkParent, "proj-link")
		if err := os.Symlink(realProj, linkProj); err != nil {
			t.Fatalf("create symlink: %v", err)
		}
		other := t.TempDir()

		_, stderr, code := h.runGot(t, other, nil, "-C", linkProj, "get", "-tool", "example.com/probe/cmd/probetool@v1.0.0")
		if code != 0 {
			t.Fatalf("got -C symlink get failed: code=%d stderr=%q", code, stderr)
		}

		stdout, stderr, code := h.runGot(t, other, nil, "exe", "-C", linkProj, "probetool")
		if code != 0 {
			t.Fatalf("got exe -C symlink probetool failed: code=%d stderr=%q", code, stderr)
		}
		var rep probeReport
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &rep); err != nil {
			t.Fatalf("unmarshal report %q: %v", stdout, err)
		}
		if rep.Version != "v1.0.0" || !sameRealPath(rep.Cwd, linkProj) {
			t.Errorf("unexpected report from symlink dir: %+v (want cwd=%q)", rep, linkProj)
		}
	})

	t.Run("mod tidy resolves non-canonical replace version in got.mod", func(t *testing.T) {
		proj := t.TempDir()
		if err := os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module example.com/app\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
		// 放置一段引用无效业务包的源码，确认沙箱 tidy 不扫描主项目业务代码
		if err := os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\n\nimport _ \"example.com/nonexistent-biz/pkg\"\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatalf("write main.go: %v", err)
		}
		// 同时放置一个相对路径本地 replace 模块，验证与非规范化版本 replace 共存时也能正常 tidy
		localRepDir := filepath.Join(proj, "localrep")
		if err := os.MkdirAll(filepath.Join(localRepDir, "cmd", "ltool"), 0o755); err != nil {
			t.Fatalf("mkdir localrep: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRepDir, "go.mod"), []byte("module example.com/localrep\n\ngo 1.24\n"), 0o644); err != nil {
			t.Fatalf("write localrep/go.mod: %v", err)
		}
		if err := os.WriteFile(filepath.Join(localRepDir, "cmd", "ltool", "main.go"), []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"ltool-ok\") }\n"), 0o644); err != nil {
			t.Fatalf("write localrep main.go: %v", err)
		}

		// 手动在 replace 中填写非规范版本 "latest"（类似于手填 commit hash/分支名，go mod edit -json 会报错但 go mod tidy 支持解析）
		gotModRaw := strings.Join([]string{
			"module example.com/app",
			"",
			"go 1.24",
			"",
			"replace (",
			"\texample.com/origtool => example.com/forktool latest",
			"\texample.com/origdep => example.com/forkdep v1.5.1",
			"\texample.com/localrep => ./localrep",
			")",
			"",
			"tool (",
			"\texample.com/origtool/cmd/ftool",
			"\texample.com/localrep/cmd/ltool",
			")",
			"",
			"require (",
			"\texample.com/localrep v0.0.0-00010101000000-000000000000 // indirect",
			"\texample.com/origtool v0.0.0-00010101000000-000000000000 // indirect",
			")",
			"",
		}, "\n")
		gotModPath := filepath.Join(proj, "got.mod")
		if err := os.WriteFile(gotModPath, []byte(gotModRaw), 0o644); err != nil {
			t.Fatalf("write got.mod: %v", err)
		}

		_, stderr, code := h.runGot(t, proj, nil, "mod", "tidy")
		if code != 0 {
			t.Fatalf("got mod tidy with non-canonical replace version failed: code=%d stderr=%q", code, stderr)
		}

		tidiedBytes, err := os.ReadFile(gotModPath)
		if err != nil {
			t.Fatalf("read tidied got.mod: %v", err)
		}
		tidied := string(tidiedBytes)
		if !strings.Contains(tidied, "example.com/origtool => example.com/forktool v1.2.0") {
			t.Errorf("expected non-canonical replace version resolved to v1.2.0, got:\n%s", tidied)
		}
		if !strings.Contains(tidied, "example.com/localrep => ./localrep") {
			t.Errorf("expected relative replace preserved after tidy, got:\n%s", tidied)
		}

		stdout, stderr, code := h.runGot(t, proj, nil, "tool", "ftool")
		if code != 0 || strings.TrimSpace(stdout) != "ftool:forkdep-v1.5.1" {
			t.Fatalf("got tool ftool after tidy failed: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})
}
