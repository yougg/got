package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/yougg/got/internal/executor"
	"github.com/yougg/got/internal/gomod"
	"github.com/yougg/got/internal/locator"
	"github.com/yougg/got/internal/runner"
)

const (
	// DefaultToolModulePath 是 `got init` 默认生成的模块路径。
	DefaultToolModulePath = "got.local/tools"
)

// Version 可在编译时通过 -ldflags "-X github.com/yougg/got/internal/cli.Version=vX.Y.Z" 注入。
var Version = "dev"

// App 封装 got CLI 的子命令路由与执行依赖。
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// NewApp 创建默认绑定到当前进程标准 IO 的 App 实例。
func NewApp() *App {
	return &App{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

// Run 解析命令行参数、执行对应子命令并返回进程退出码。
// 当子进程以非 0 退出码结束时，返回 (exitCode, nil) 以便静默透传退出码。
func (a *App) Run(ctx context.Context, args []string) (int, error) {
	chDir, rest, err := ParseGlobalFlags(args)
	if err != nil {
		return 2, err
	}

	if len(rest) == 0 {
		a.printUsage(a.Stderr)
		return 2, nil
	}

	subcmd := rest[0]
	subArgs := rest[1:]

	switch subcmd {
	case "help", "-h", "--help":
		a.printUsage(a.Stdout)
		return 0, nil
	}

	// 启动即校验 Go 工具链版本 >= 1.24
	envInfo, err := gomod.InspectGoEnv(ctx)
	if err != nil {
		return 1, err
	}

	// 兼容 `go <subcmd> -C <dir>` 写法：若全局未传 -C 且子命令首个标志为 -C，一并提取
	if chDir == "" && len(subArgs) > 0 && (subcmd == "init" || subcmd == "get" || subcmd == "rm" || subcmd == "remove" || subcmd == "tool" || subcmd == "exe" || subcmd == "mod" || subcmd == "list") {
		subChDir, trimmed, parseErr := ParseGlobalFlags(subArgs)
		if parseErr != nil {
			return 2, parseErr
		}
		if subChDir != "" {
			chDir = subChDir
			subArgs = trimmed
		}
	}

	workDir, err := locator.ResolveWorkDir(chDir)
	if err != nil {
		return 1, err
	}

	gotFileEnv := os.Getenv("GOTFILE")
	r := &runner.Runner{
		Stdin:    a.Stdin,
		Stdout:   a.Stdout,
		Stderr:   a.Stderr,
		HostOS:   envInfo.GoHostOS,
		HostArch: envInfo.GoHostArch,
	}
	execEngine := executor.New(r, envInfo.GoRoot)

	slog.Debug("分发子命令",
		"subcmd", subcmd,
		"args", subArgs,
		"workDir", workDir,
		"gotFileEnv", gotFileEnv,
		"goVersion", envInfo.GoVersion,
	)

	switch subcmd {
	case "init":
		return a.runInit(ctx, r, workDir, gotFileEnv, envInfo, subArgs)
	case "get":
		return a.runGet(ctx, r, workDir, gotFileEnv, envInfo, subArgs)
	case "rm", "remove":
		return a.runRemove(ctx, r, workDir, gotFileEnv, subArgs)
	case "tool", "exe":
		return a.runTool(ctx, r, execEngine, workDir, gotFileEnv, envInfo, subArgs)
	case "mod":
		return a.runMod(ctx, r, workDir, gotFileEnv, subArgs)
	case "list":
		return a.runList(ctx, r, workDir, gotFileEnv, subArgs)
	case "version":
		return a.runVersion(envInfo)
	default:
		return 2, fmt.Errorf("unknown command %q\nRun 'got help' for usage", subcmd)
	}
}

// ParseGlobalFlags 解析子命令前的全局标志（目前支持 -C <dir> / -C=<dir>）。
func ParseGlobalFlags(args []string) (chDir string, rest []string, err error) {
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "-C" || arg == "--C" {
			if i+1 >= len(args) {
				return "", nil, fmt.Errorf("flag %s requires a directory argument", arg)
			}
			chDir = args[i+1]
			i += 2
			continue
		}
		if val, ok := strings.CutPrefix(arg, "-C="); ok {
			if val == "" {
				return "", nil, fmt.Errorf("flag -C requires a non-empty directory argument")
			}
			chDir = val
			i++
			continue
		}
		if val, ok := strings.CutPrefix(arg, "--C="); ok {
			if val == "" {
				return "", nil, fmt.Errorf("flag --C requires a non-empty directory argument")
			}
			chDir = val
			i++
			continue
		}
		break
	}
	return chDir, args[i:], nil
}

func (a *App) runInit(ctx context.Context, r *runner.Runner, workDir, gotFileEnv string, envInfo *gomod.EnvInfo, args []string) (int, error) {
	sameModule := false
	var positional []string

	for _, arg := range args {
		switch arg {
		case "--same-module", "-same-module":
			sameModule = true
		case "-h", "--help":
			fmt.Fprintln(a.Stdout, "Usage: got init [--same-module | module-path]")
			return 0, nil
		default:
			if strings.HasPrefix(arg, "-") {
				return 2, fmt.Errorf("unknown flag %q for 'got init'", arg)
			}
			positional = append(positional, arg)
		}
	}

	if sameModule && len(positional) > 0 {
		return 2, fmt.Errorf("cannot specify both --same-module and an explicit module path")
	}
	if len(positional) > 1 {
		return 2, fmt.Errorf("got init accepts at most one module path argument")
	}

	targetPath := filepath.Join(workDir, locator.DefaultModFileName)
	if gotFileEnv != "" {
		if filepath.IsAbs(gotFileEnv) {
			targetPath = filepath.Clean(gotFileEnv)
		} else {
			targetPath = filepath.Join(workDir, gotFileEnv)
		}
	}

	// 若同级存在 go.mod，预先解析以便 --same-module 及自动迁移其中的 tool 列表
	rootDir := filepath.Dir(targetPath)
	mainGoMod := filepath.Join(rootDir, locator.GoModFileName)
	var mainMod *gomod.ModEditJSON
	if info, statErr := os.Stat(mainGoMod); statErr == nil && !info.IsDir() {
		parsed, parseErr := gomod.ParseModFile(ctx, mainGoMod)
		if parseErr != nil {
			if sameModule {
				return 1, fmt.Errorf("failed to read main module path from %s: %w", mainGoMod, parseErr)
			}
			slog.Debug("解析同级 go.mod 失败，跳过自动迁移 tool 列表", "path", mainGoMod, "err", parseErr)
		} else {
			mainMod = parsed
		}
	} else if sameModule {
		return 1, fmt.Errorf("--same-module requires %s to exist: %w", mainGoMod, statErr)
	}

	var localTools []string
	var externalTools []string
	if mainMod != nil && len(mainMod.Tool) > 0 {
		for _, item := range mainMod.Tool {
			if item.Path == "" {
				continue
			}
			if gomod.IsPackageInModule(item.Path, mainMod.Module.Path) {
				if !slices.Contains(localTools, item.Path) {
					localTools = append(localTools, item.Path)
				}
			} else {
				if !slices.Contains(externalTools, item.Path) {
					externalTools = append(externalTools, item.Path)
				}
			}
		}
	}

	var modulePath string
	switch {
	case sameModule:
		if mainMod == nil || mainMod.Module.Path == "" {
			return 1, fmt.Errorf("main module path in %s is empty", mainGoMod)
		}
		modulePath = mainMod.Module.Path
	case len(positional) == 1:
		modulePath = positional[0]
	case len(localTools) > 0 && mainMod != nil && mainMod.Module.Path != "":
		// 存在仓库本地工具时，自动与主模块路径保持一致以便 Go 解析本地工具包
		modulePath = mainMod.Module.Path
	default:
		modulePath = DefaultToolModulePath
	}

	if err := createGotMod(targetPath, modulePath, envInfo.GoVersion); err != nil {
		return 1, err
	}

	fmt.Fprintf(a.Stderr, "got: creating new %s: module %s\n", filepath.Base(targetPath), modulePath)

	// 若主项目 go.mod 中存在 tool 列表，自动复制到 got.mod 并拉取对应版本的依赖包
	if len(localTools) > 0 || len(externalTools) > 0 {
		sumPath := strings.TrimSuffix(targetPath, ".mod") + ".sum"
		_, sumExistedErr := os.Stat(sumPath)
		sumExisted := sumExistedErr == nil

		loc := &locator.Context{
			ModFile:     targetPath,
			RootDir:     rootDir,
			HasGoMod:    true,
			OrigWorkDir: workDir,
		}
		code, migrateErr := a.migrateToolsFromGoMod(ctx, r, loc, mainMod, localTools, externalTools)
		if migrateErr != nil || code != 0 {
			// 初始化拉取失败时清理半成品文件，避免阻断后续重试
			_ = os.Remove(targetPath)
			if !sumExisted {
				_ = os.Remove(sumPath)
			}
			return code, migrateErr
		}
	}

	return 0, nil
}

// migrateToolsFromGoMod 将主项目 go.mod 中的 tool 列表复制到 got.mod，
// 并按 go.mod 中记录的对应版本拉取这些工具依赖的包完成初始化。
func (a *App) migrateToolsFromGoMod(
	ctx context.Context,
	r *runner.Runner,
	loc *locator.Context,
	mainMod *gomod.ModEditJSON,
	localTools []string,
	externalTools []string,
) (int, error) {
	total := len(localTools) + len(externalTools)
	fmt.Fprintf(a.Stderr, "got: migrating %d tool(s) from go.mod to %s\n", total, filepath.Base(loc.ModFile))

	appliedReplaces := make(map[string]bool)
	applyReplacesForModules := func(modPaths []string) (int, error) {
		var editArgs []string
		for _, rep := range mainMod.Replace {
			if !slices.Contains(modPaths, rep.Old.Path) {
				continue
			}
			arg := gomod.FormatReplaceArg(rep)
			if appliedReplaces[arg] {
				continue
			}
			appliedReplaces[arg] = true
			if len(editArgs) == 0 {
				editArgs = append(editArgs, "edit")
			}
			editArgs = append(editArgs, "-replace="+arg)
		}
		if len(editArgs) == 0 {
			return 0, nil
		}
		return r.RunStreaming(ctx, loc, "mod", editArgs)
	}

	// 1. 迁移外部工具：按主项目 go.mod 中锁定的对应版本精确拉取
	if len(externalTools) > 0 {
		var toolModPaths []string
		getArgs := make([]string, 0, 1+len(externalTools))
		getArgs = append(getArgs, "-tool")

		for _, toolPath := range externalTools {
			if req, ok := gomod.MatchRequireModule(toolPath, mainMod.Require); ok && req.Version != "" {
				if !slices.Contains(toolModPaths, req.Path) {
					toolModPaths = append(toolModPaths, req.Path)
				}
				getArgs = append(getArgs, toolPath+"@"+req.Version)
			} else {
				// 兜底检查 replace 中是否存在匹配前缀
				for _, rep := range mainMod.Replace {
					if gomod.IsPackageInModule(toolPath, rep.Old.Path) && !slices.Contains(toolModPaths, rep.Old.Path) {
						toolModPaths = append(toolModPaths, rep.Old.Path)
					}
				}
				getArgs = append(getArgs, toolPath)
			}
		}

		if code, err := applyReplacesForModules(toolModPaths); err != nil || code != 0 {
			return code, err
		}
		if code, err := r.RunStreaming(ctx, loc, "get", getArgs); err != nil || code != 0 {
			return code, err
		}
	}

	// 2. 迁移仓库本地工具：写入 tool 声明，并精准拉取本地工具自身依赖的外部包版本
	if len(localTools) > 0 {
		editArgs := make([]string, 0, 1+len(localTools))
		editArgs = append(editArgs, "edit")
		for _, toolPath := range localTools {
			editArgs = append(editArgs, "-tool="+toolPath)
		}
		if code, err := r.RunStreaming(ctx, loc, "mod", editArgs); err != nil || code != 0 {
			return code, err
		}

		depSpecs, depModPaths, err := queryLocalToolDeps(ctx, r, loc.RootDir, localTools)
		if err != nil {
			return 1, err
		}
		if code, err := applyReplacesForModules(depModPaths); err != nil || code != 0 {
			return code, err
		}
		if len(depSpecs) > 0 {
			if code, err := r.RunStreaming(ctx, loc, "get", depSpecs); err != nil || code != 0 {
				return code, err
			}
		}
	}

	// 3. 确保 got.mod 中记录的所有工具依赖包均已下载到模块缓存
	return r.RunStreaming(ctx, loc, "mod", []string{"download"})
}

// queryLocalToolDeps 在主项目 go.mod 上下文中查询仓库本地工具依赖的外部模块及对应版本。
func queryLocalToolDeps(ctx context.Context, r *runner.Runner, rootDir string, localTools []string) (specs, modPaths []string, err error) {
	args := make([]string, 0, 3+len(localTools))
	args = append(args, "list", "-deps", "-f", "{{if .Module}}{{if not .Module.Main}}{{.Module.Path}}@{{.Module.Version}}{{end}}{{end}}")
	args = append(args, localTools...)

	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = rootDir
	cmd.Env = runner.BuildGoEnv(os.Environ(), r.HostOS, r.HostArch)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	slog.Debug("查询本地工具依赖的外部模块版本", "args", cmd.Args, "dir", cmd.Dir)
	if runErr := cmd.Run(); runErr != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, nil, fmt.Errorf("failed to inspect local tool dependencies: %s: %w", errMsg, runErr)
		}
		return nil, nil, fmt.Errorf("failed to inspect local tool dependencies: %w", runErr)
	}

	for line := range strings.SplitSeq(stdout.String(), "\n") {
		spec := strings.TrimSpace(line)
		if spec == "" {
			continue
		}
		modPath, ver, ok := strings.Cut(spec, "@")
		if !ok || modPath == "" {
			continue
		}
		if !slices.Contains(modPaths, modPath) {
			modPaths = append(modPaths, modPath)
		}
		if ver == "" {
			spec = modPath
		}
		if !slices.Contains(specs, spec) {
			specs = append(specs, spec)
		}
	}
	return specs, modPaths, nil
}

func createGotMod(targetPath, modulePath, goVersion string) error {
	if _, err := os.Stat(targetPath); err == nil {
		return fmt.Errorf("%s already exists", targetPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to check %s: %w", targetPath, err)
	}

	goModVer := gomod.FormatGoModVersion(goVersion)
	content := fmt.Sprintf("module %s\n\ngo %s\n", modulePath, goModVer)
	if err := os.WriteFile(targetPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", targetPath, err)
	}
	return nil
}

func (a *App) runGet(ctx context.Context, r *runner.Runner, workDir, gotFileEnv string, envInfo *gomod.EnvInfo, args []string) (int, error) {
	autoInit, cleanedArgs := ExtractInitFlag(args)

	loc, err := locator.Locate(workDir, gotFileEnv)
	if err != nil {
		if errors.Is(err, locator.ErrModFileNotFound) && autoInit {
			targetPath := filepath.Join(workDir, locator.DefaultModFileName)
			if gotFileEnv != "" {
				if filepath.IsAbs(gotFileEnv) {
					targetPath = filepath.Clean(gotFileEnv)
				} else {
					targetPath = filepath.Join(workDir, gotFileEnv)
				}
			}
			if initErr := createGotMod(targetPath, DefaultToolModulePath, envInfo.GoVersion); initErr != nil {
				return 1, initErr
			}
			fmt.Fprintf(a.Stderr, "got: creating new %s: module %s\n", filepath.Base(targetPath), DefaultToolModulePath)
			loc, err = locator.Locate(workDir, gotFileEnv)
		}
		if err != nil {
			return 1, err
		}
	}

	finalArgs := NormalizeGetArgs(cleanedArgs)
	return r.RunStreaming(ctx, loc, "get", finalArgs)
}

// ExtractInitFlag 从 `got get` 参数列表中提取并移除 got 专属的 --init / -init 标志。
func ExtractInitFlag(args []string) (autoInit bool, remaining []string) {
	remaining = make([]string, 0, len(args))
	endOfFlags := false
	for _, arg := range args {
		if !endOfFlags && arg == "--" {
			endOfFlags = true
			remaining = append(remaining, arg)
			continue
		}
		if !endOfFlags && (arg == "--init" || arg == "-init") {
			autoInit = true
			continue
		}
		remaining = append(remaining, arg)
	}
	return autoInit, remaining
}

// NormalizeGetArgs 规范化 `got get` 参数：
// 当包含 `-u`（或 `-u=patch`）且未传入任何包参数时，自动补充 `tool` 元包模式。
func NormalizeGetArgs(args []string) []string {
	hasUpdate := false
	hasPkgArg := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			if i+1 < len(args) {
				hasPkgArg = true
			}
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			hasPkgArg = true
			continue
		}
		if arg == "-u" || arg == "--u" || strings.HasPrefix(arg, "-u=") || strings.HasPrefix(arg, "--u=") {
			hasUpdate = true
			continue
		}
		if isGoGetValueFlag(arg) {
			// 跳过与该标志配对的值参数
			if i+1 < len(args) {
				i++
			}
		}
	}

	out := make([]string, 0, len(args)+1)
	out = append(out, args...)
	if hasUpdate && !hasPkgArg {
		out = append(out, "tool")
	}
	return out
}

func isGoGetValueFlag(flag string) bool {
	if strings.Contains(flag, "=") {
		return false
	}
	name := strings.TrimPrefix(strings.TrimPrefix(flag, "--"), "-")
	switch name {
	case "C", "mod", "modfile", "overlay", "tags", "ldflags", "gcflags",
		"asmflags", "buildmode", "compiler", "gccgoflags", "installsuffix",
		"pkgdir", "toolexec", "p", "covermode", "coverpkg":
		return true
	default:
		return false
	}
}

func (a *App) runRemove(ctx context.Context, r *runner.Runner, workDir, gotFileEnv string, args []string) (int, error) {
	if len(args) == 0 {
		return 2, fmt.Errorf("usage: got rm <tool-or-pkg>...")
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(a.Stdout, "Usage: got rm <tool-or-pkg>...")
		return 0, nil
	}

	loc, err := locator.Locate(workDir, gotFileEnv)
	if err != nil {
		return 1, err
	}

	mod, err := gomod.ParseModFile(ctx, loc.ModFile)
	if err != nil {
		return 1, err
	}

	resolvedTargets := make([]string, 0, len(args))
	for _, raw := range args {
		if strings.HasPrefix(raw, "-") {
			return 2, fmt.Errorf("unknown flag %q for 'got rm'", raw)
		}
		// 兼容用户误传的 @none 或 @version 后缀
		baseName, _, _ := strings.Cut(raw, "@")
		matched, matchErr := gomod.MatchTool(mod.Tool, baseName)
		if matchErr != nil {
			return 1, matchErr
		}
		resolvedTargets = append(resolvedTargets, matched+"@none")
	}

	// 第一步：依次移除每个工具声明
	for _, target := range resolvedTargets {
		code, runErr := r.RunStreaming(ctx, loc, "get", []string{"-tool", target})
		if runErr != nil {
			return 1, runErr
		}
		if code != 0 {
			return code, nil
		}
	}

	// 第二步：执行 tidy 清理无用的间接依赖与 got.sum
	return r.RunStreaming(ctx, loc, "mod", []string{"tidy"})
}

// ToolInvocation 表示解析后的 `got tool` 调用结构。
type ToolInvocation struct {
	FlagN    bool
	PreFlags []string
	ToolName string
	ToolArgs []string
}

// ParseToolInvocation 解析 `got tool [flags...] [<name> [toolArgs...]]`。
func ParseToolInvocation(args []string) ToolInvocation {
	var inv ToolInvocation
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		if arg == "-n" || arg == "--n" {
			inv.FlagN = true
		}
		inv.PreFlags = append(inv.PreFlags, arg)
		// 少数可能出现在 tool 名称前的带值构建标志
		if isGoGetValueFlag(arg) && i+1 < len(args) {
			i++
			inv.PreFlags = append(inv.PreFlags, args[i])
		}
		i++
	}

	if i < len(args) {
		inv.ToolName = args[i]
		if i+1 < len(args) {
			inv.ToolArgs = args[i+1:]
		}
	}
	return inv
}

func (a *App) runTool(ctx context.Context, r *runner.Runner, execEngine *executor.Executor, workDir, gotFileEnv string, envInfo *gomod.EnvInfo, args []string) (int, error) {
	inv := ParseToolInvocation(args)

	// 无工具名参数 -> 列表模式
	if inv.ToolName == "" {
		loc, err := locator.Locate(workDir, gotFileEnv)
		if err != nil {
			return 1, err
		}
		return r.RunStreaming(ctx, loc, "tool", args)
	}

	// 指定了工具名：尝试定位 got.mod
	loc, err := locator.Locate(workDir, gotFileEnv)
	if err != nil {
		if errors.Is(err, locator.ErrModFileNotFound) && gomod.IsBuiltinTool(envInfo.GoRoot, envInfo.GoHostOS, envInfo.GoHostArch, inv.ToolName) {
			// 无 got.mod 时，Go 内置工具直接以原始环境透传执行
			return execEngine.RunPassthrough(ctx, workDir, args)
		}
		return 1, err
	}

	mod, err := gomod.ParseModFile(ctx, loc.ModFile)
	if err != nil {
		return 1, err
	}

	targetPkg, err := gomod.MatchTool(mod.Tool, inv.ToolName)
	if err != nil {
		if errors.Is(err, gomod.ErrAmbiguousTool) {
			return 1, err
		}
		if errors.Is(err, gomod.ErrToolNotFound) {
			// 未在 got.mod 中声明：视为 Go 内置工具或未知工具，剥离 -modfile 以原始环境透传
			return execEngine.RunPassthrough(ctx, loc.OrigWorkDir, args)
		}
		return 1, err
	}

	// 命中 got.mod 声明的自定义工具
	if inv.FlagN {
		nArgs := make([]string, 0, len(inv.PreFlags)+1+len(inv.ToolArgs))
		nArgs = append(nArgs, inv.PreFlags...)
		nArgs = append(nArgs, targetPkg)
		nArgs = append(nArgs, inv.ToolArgs...)
		return r.RunStreaming(ctx, loc, "tool", nArgs)
	}

	return execEngine.RunCustomTool(ctx, loc, targetPkg, inv.ToolArgs, args)
}

func (a *App) runMod(ctx context.Context, r *runner.Runner, workDir, gotFileEnv string, args []string) (int, error) {
	loc, err := locator.Locate(workDir, gotFileEnv)
	if err != nil {
		return 1, err
	}
	return r.RunStreaming(ctx, loc, "mod", args)
}

func (a *App) runList(ctx context.Context, r *runner.Runner, workDir, gotFileEnv string, args []string) (int, error) {
	loc, err := locator.Locate(workDir, gotFileEnv)
	if err != nil {
		return 1, err
	}
	return r.RunStreaming(ctx, loc, "list", args)
}

func (a *App) runVersion(envInfo *gomod.EnvInfo) (int, error) {
	ver := Version
	var buildMeta []string
	if info, ok := debug.ReadBuildInfo(); ok {
		if ver == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			ver = info.Main.Version
		}
		var rev, modified string
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) > 12 {
					rev = s.Value[:12]
				} else {
					rev = s.Value
				}
			case "vcs.modified":
				if s.Value == "true" {
					modified = "-dirty"
				}
			}
		}
		if rev != "" {
			buildMeta = append(buildMeta, "commit="+rev+modified)
		}
	}

	if len(buildMeta) > 0 {
		fmt.Fprintf(a.Stdout, "got version %s (%s) using %s\n", ver, strings.Join(buildMeta, ", "), envInfo.GoVersion)
	} else {
		fmt.Fprintf(a.Stdout, "got version %s using %s\n", ver, envInfo.GoVersion)
	}
	return 0, nil
}

func (a *App) printUsage(w io.Writer) {
	const usage = `got manages Go development tool dependencies in an isolated got.mod / got.sum.

Usage:
	got [-C <dir>] <command> [arguments]

Commands:
	init [--same-module | module-path]   initialize a new got.mod file
	get [--init] [-tool] [packages...]   add or update tool dependencies in got.mod
	rm, remove <tool-or-pkg>...          remove tool dependencies and tidy got.mod
	tool, exe [-n] [<name> [args...]]    list or run tools declared in got.mod
	mod <subcommand> [args...]           run go mod subcommands against got.mod
	list [args...]                       run go list against got.mod
	version                              print got and Go toolchain version
	help                                 show this help message

Environment:
	GOTFILE=<path>   explicitly specify path to got.mod
	GOT_DEBUG=1      enable debug logging to stderr
`
	fmt.Fprint(w, usage)
}
