package app

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/release"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/version"
)

// selfUpdateHandshakeTimeout 是 self-update 拉回内核后的就绪轮询窗口（batch3 §2.3 步 8）。
const selfUpdateHandshakeTimeout = 15 * time.Second

// selfUpdateEnv 是 self-update 家族（主体 / --core）的可注入编排环境：
// 测试构造假编排（临时 plist、假二进制、记录式缝隙），绝不真出网/触碰 launchd。
type selfUpdateEnv struct {
	// newRepoClient 构造本仓客户端（token 由编排读取后传入；测试注入假 API 基址）。
	newRepoClient func(token string) *release.Client
	// newUpstreamClient 构造 mihomo 上游客户端（公开仓免鉴权；--core 专用，c3）。
	newUpstreamClient func() *release.Client
	// executable 返回调用者自身路径（os.Executable 缝隙）。
	executable func() (string, error)
	// launchDir 返回用户 LaunchAgents 目录（测试指向临时目录，绝不触碰真实 plist）。
	launchDir func() (string, error)
	// autostartOn 报告 LaunchAgent 是否已安装（autostartEnabled 缝隙）。
	autostartOn func() bool
	// reachable 探测内核是否在跑（CoreReachable 缝隙）。
	reachable func(paths.Home) bool
	// stopCore 停止运行中实例（StopW 缝隙；--json 模式进度走 stderr）。
	stopCore func(paths.Home) error
	// waitReady 轮询 /version 就绪（waitCoreReady 缝隙，15s 档）。
	waitReady func(paths.Home, string, time.Duration) bool
	// kickstart 经 launchctl 拉回内核（autostart on 分支；无 -k，F8）。
	kickstart func() error
	// spawnStart detached spawn `tradewind start`（手动 start 分支；Setpgid 脱管）。
	spawnStart func(exe string) error
	// runBinaryVersion 执行二进制并返回输出（--core 回读/payload version 探针缝隙）。
	runBinaryVersion func(binPath string, args ...string) (string, error)
	// coreVersionProbe 运行中内核 /version 探针（--core 比对与握手专用缝隙）。
	coreVersionProbe func(paths.Home) (string, bool)
	// progress 步骤打印（人读模式 stdout / --json 模式 stderr，红线 2）。
	progress io.Writer
}

// ProductionEnv 组装生产编排环境（progress 由 CLI 层按 --json 选 stdout/stderr）。
func ProductionEnv(ctx context.Context, home paths.Home, progress io.Writer) selfUpdateEnv {
	egress := func() *http.Client { return selfEgressClient(ctx, home) }
	return selfUpdateEnv{
		newRepoClient:     func(token string) *release.Client { return newReleaseClient(token, egress) },
		newUpstreamClient: func() *release.Client { return release.NewClient("", egress) },
		executable:        os.Executable,
		launchDir:         launchAgentsDir,
		autostartOn:       autostartEnabled,
		reachable:         CoreReachable,
		stopCore:          func(home paths.Home) error { return StopW(home, progress) },
		waitReady:         waitCoreReady,
		kickstart:         kickstartAgentCore,
		spawnStart:        spawnDetachedCoreStart,
		runBinaryVersion:  runBinaryVersionOutput,
		coreVersionProbe:  coreVersionProbeFor(ctx, home),
		progress:          progress,
	}
}

// coreVersionProbeFor 生产 /version 探针（reload.go 同款 sock 客户端，2s 档）。
func coreVersionProbeFor(ctx context.Context, home paths.Home) func(paths.Home) (string, bool) {
	return func(h paths.Home) (string, bool) {
		client := core.NewClient(h.SockFile(), store.ReadSecret(h))
		cctx, cancel := context.WithTimeout(ctx, reloadProbeTimeout)
		defer cancel()
		v, err := client.Version(cctx)
		return v, err == nil
	}
}

// runBinaryVersionOutput 执行二进制并返回合并输出（15s 上限；缝隙供测试注入假脚本）。
var runBinaryVersionOutput = func(binPath string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, args...)
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("app: run %s %v: %w", binPath, args, err)
	}
	return out.String(), nil
}

// kickstartAgentCore 经 launchctl kickstart 拉回 LaunchAgent 托管的内核
// （铁律 F8：无 -k —— kickstart -k 对 wrapper 模式是 no-op）。
func kickstartAgentCore() error {
	cmd := exec.Command("launchctl", "kickstart",
		fmt.Sprintf("gui/%d/%s", os.Getuid(), LaunchAgentLabel))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("app: launchctl kickstart: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// spawnDetachedCoreStart detached spawn `tradewind start`（手动 start 场景无
// LaunchAgent，F8）：SysProcAttr.Setpgid 脱管，父进程退出不牵连内核。
func spawnDetachedCoreStart(exe string) error {
	cmd := exec.Command(exe, "start")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("app: detached start %s: %w", exe, err)
	}
	go func() { _ = cmd.Wait() }() // 回收子进程（不阻塞 self-update）
	return nil
}

// SelfUpdate 编排 `tradewind self-update` 主体（batch3 §2.3 编排序 1-9）。
// 返回 updated_to（已是最新时为当前 tag）。每步失败即中止，可重入。
func SelfUpdate(ctx context.Context, home paths.Home, env selfUpdateEnv) (string, error) {
	// 步 1：latest + asset（version --check 逻辑复用）
	if version.Version == "dev" {
		return "", errors.New("app: 开发构建无法比对（当前版本 dev，无比对基线）")
	}
	token, err := release.ReadToken(home)
	if err != nil {
		return "", err
	}
	client := env.newRepoClient(token)
	res, err := checkUpdateWith(ctx, client)
	if err != nil {
		return "", err
	}
	if !res.UpdateAvailable {
		fmt.Fprintf(env.progress, "已是最新（%s == %s），nothing to do\n", res.Current, res.Latest)
		return res.Latest, nil
	}
	// 替换目标先解析（步 6 前置快失败）：plist 解析失败中止不猜，不动内核
	targets, err := selfUpdateTargets(env)
	if err != nil {
		return "", err
	}
	// 步 2：下载整包资产（octet-stream 通道，60s HTTP 超时/资产）
	tarball := home.Join("self-update.tar.gz")
	defer os.Remove(tarball)
	fmt.Fprintf(env.progress, "downloading %s ...\n", res.Asset)
	if err := downloadAssetToFile(ctx, client, res.rel, res.Asset, tarball); err != nil {
		return "", err
	}
	// 步 3：sidecar 完整性校验（缺失/不符 → 中止不动盘）
	if err := verifyTarballSidecar(ctx, client, res, tarball, env.progress); err != nil {
		return "", err
	}
	// 步 4：解包 tradewind 二进制并回读版本行 == tag
	payload := home.Join("self-update.payload")
	defer os.Remove(payload)
	if err := extractAndVerifyPayload(tarball, payload, res.Latest, env); err != nil {
		return "", err
	}
	// 步 5：内核运行中 → 先 Stop（fullShutdown 优雅管线，F3）
	wasRunning := env.reachable(home)
	if wasRunning {
		fmt.Fprintln(env.progress, "core is running; stopping via graceful shutdown first...")
		if err := env.stopCore(home); err != nil {
			return "", err
		}
	}
	// 步 6：双路径替换（rm+cp 新 inode，chmod 0755；相同只换一次）
	for _, target := range targets {
		fmt.Fprintf(env.progress, "replacing %s ...\n", target)
		if err := replaceBinary(payload, target); err != nil {
			return "", err
		}
	}
	secret := store.ReadSecret(home)
	if !wasRunning {
		// 步 8（未运行分支）：只换盘，提示下次启动生效
		fmt.Fprintln(env.progress, "binary replaced; takes effect on next start（下次启动生效）")
	} else {
		// 步 7：拉回内核（kickstart / detached start 分支）
		if err := bringCoreBack(env, targets[0]); err != nil {
			return "", err
		}
		// 步 8：轮询 /version 就绪（15s）
		if !env.waitReady(home, secret, selfUpdateHandshakeTimeout) {
			return "", errors.New("app: core did not come back within 15s; run `tradewind start` manually")
		}
		fmt.Fprintf(env.progress, "self-updated %s -> %s, core restarted\n", res.Current, res.Latest)
	}
	// 步 9：app/ui 不替换（裁剪登记：app 终局 Sparkle，ui 随整包）
	fmt.Fprintln(env.progress, "note: app/ui 不随 self-update 替换；如需同步请重新安装整包 tarball")
	return res.Latest, nil
}

// selfUpdateTargets 解析两处替换目标：a) 调用者自身路径；b) LaunchAgent plist
// ProgramArguments[0]。相同只换一次；plist 缺失（未开 autostart 的手动 start
// 场景，F8）跳过 b；存在但解析失败 → 中止不猜（batch3 §2.3 步 6）。
func selfUpdateTargets(env selfUpdateEnv) ([]string, error) {
	exe, err := env.executable()
	if err != nil {
		return nil, fmt.Errorf("app: resolve executable: %w", err)
	}
	targets := []string{exe}
	dir, err := env.launchDir()
	if err != nil {
		return nil, err
	}
	plist := plistPathFor(dir)
	if _, statErr := os.Stat(plist); errors.Is(statErr, os.ErrNotExist) {
		return targets, nil
	}
	program, err := launchdProgramPath(plist)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if t == program {
			return targets, nil // 两路径相同只换一次
		}
	}
	return append(targets, program), nil
}

// launchdProgramPath 解析 LaunchAgent plist 的 ProgramArguments[0]（stdlib
// encoding/xml；launchdPlist 生成的 dict 下仅一个 array——ProgramArguments）。
func launchdProgramPath(plistPath string) (string, error) {
	data, err := os.ReadFile(plistPath)
	if err != nil {
		return "", fmt.Errorf("app: read plist %s: %w", plistPath, err)
	}
	var parsed struct {
		ProgramArguments []string `xml:"dict>array>string"`
	}
	if err := xml.Unmarshal(data, &parsed); err != nil {
		return "", fmt.Errorf("app: parse plist %s: %w（请手动确认 LaunchAgent plist 形态后再试）", plistPath, err)
	}
	if len(parsed.ProgramArguments) == 0 || parsed.ProgramArguments[0] == "" {
		return "", fmt.Errorf("app: plist %s 无 ProgramArguments[0]，中止不猜", plistPath)
	}
	return parsed.ProgramArguments[0], nil
}

// verifyTarballSidecar 下载 sidecar 资产并校验 tarball sha256（内容格式
// <hex>  <tarball 文件名>，与 F6 sha256sum 风格同构；缺失/不符 → 中止不动盘，
// batch3 §2.3 步 3）。
func verifyTarballSidecar(ctx context.Context, client *release.Client, res CheckUpdateResult, tarball string, progress io.Writer) error {
	name := SidecarNameFor(res.Latest)
	asset, ok := res.rel.AssetByName(name)
	if !ok {
		return fmt.Errorf("app: 目标版本 %s 缺校验文件 %s（该 release 早于 sidecar 规范，不可自升级）", res.Latest, name)
	}
	fmt.Fprintf(progress, "downloading %s ...\n", name)
	sidecar := tarball + ".sha256"
	defer os.Remove(sidecar)
	if err := downloadAssetByID(ctx, client, asset, sidecar); err != nil {
		return err
	}
	data, err := os.ReadFile(sidecar)
	if err != nil {
		return fmt.Errorf("app: read sidecar: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	if len(line) < 64 {
		return fmt.Errorf("app: sidecar %s 内容过短，中止不动盘", name)
	}
	want := strings.ToLower(line[:64])
	got, err := sha256SumFile(tarball)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("app: sha256 mismatch for %s: got %s, want %s; 中止不动盘", res.Asset, got, want)
	}
	fmt.Fprintln(progress, "sha256 verified")
	return nil
}

// extractAndVerifyPayload 解包 tar.gz 内 tradewind 二进制到 payload 并回读版本：
// 版本行（首行，"tradewind" 前缀）必须 == latest tag（三审 P3：version 输出多行
// 含 core 行，整段全等恒败）；不符中止。
func extractAndVerifyPayload(tarball, payload, tag string, env selfUpdateEnv) error {
	if err := extractTarEntry(tarball, "tradewind", payload); err != nil {
		return err
	}
	if err := os.Chmod(payload, 0o755); err != nil {
		return fmt.Errorf("app: chmod payload: %w", err)
	}
	out, err := env.runBinaryVersion(payload, "version")
	if err != nil {
		return fmt.Errorf("app: payload version probe failed: %w", err)
	}
	firstLine := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	want := "tradewind " + tag
	if firstLine != want {
		return fmt.Errorf("app: payload reports %q, want %q; 中止不动盘", firstLine, want)
	}
	return nil
}

// bringCoreBack 拉回内核（评审 P2-2 分支）：autostartEnabled → kickstart（无 -k）；
// 手动 start 场景无 agent → detached spawn 新二进制 `tradewind start`（Setpgid 脱管）。
func bringCoreBack(env selfUpdateEnv, exe string) error {
	if env.autostartOn() {
		fmt.Fprintln(env.progress, "core restarting via launchctl kickstart...")
		return env.kickstart()
	}
	fmt.Fprintf(env.progress, "core restarting via detached `tradewind start` (%s)...\n", exe)
	return env.spawnStart(exe)
}

// downloadAssetToFile 按资产名定位并经 octet-stream 通道下载到 dest（0600 落盘）。
func downloadAssetToFile(ctx context.Context, client *release.Client, rel *release.Release, name, dest string) error {
	asset, ok := rel.AssetByName(name)
	if !ok {
		return fmt.Errorf("app: release %s 缺资产 %s", rel.Tag, name)
	}
	return downloadAssetByID(ctx, client, asset, dest)
}

// downloadAssetByID 经 GET /releases/assets/<id> + Accept: application/octet-stream
// 下载资产（302 至签名 URL 由 http.Client 默认行为处理，batch3 评审二轮 P1-1）。
func downloadAssetByID(ctx context.Context, client *release.Client, asset release.Asset, dest string) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("app: create %s: %w", dest, err)
	}
	n, err := client.DownloadAsset(ctx, asset, f)
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("app: download asset %d: %w", asset.ID, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if n == 0 {
		return fmt.Errorf("app: download asset %d: empty body", asset.ID)
	}
	return nil
}

// replaceBinary 以 rm+cp 换新 inode（铁律 F8：严禁 cp 原地覆写运行中二进制——
// SIGKILL exit 137），落位 chmod 0755（显式 chmod 收敛 umask）。
func replaceBinary(src, dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("app: remove %s: %w", dst, err)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("app: open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("app: create %s: %w", dst, err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("app: replace %s: %w", dst, copyErr)
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Chmod(dst, 0o755)
}

// extractTarEntry 从 tar.gz 提取单个条目到 dest（路径穿越防御：Clean(name) 必须
// 精确等于 want 且为普通文件；512MB 上限防解压炸弹）。
func extractTarEntry(tarball, want, dest string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return fmt.Errorf("app: open %s: %w", tarball, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("app: gunzip %s: %w", tarball, err)
	}
	defer gz.Close()
	const maxPayload = 512 << 20
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("app: tarball %s has no %q entry", tarball, want)
		}
		if err != nil {
			return fmt.Errorf("app: read tar: %w", err)
		}
		if filepath.Clean(hdr.Name) != want || hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size > maxPayload {
			return fmt.Errorf("app: entry %s too large (%d bytes)", hdr.Name, hdr.Size)
		}
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("app: create %s: %w", dest, err)
		}
		n, copyErr := io.Copy(out, io.LimitReader(tr, maxPayload+1))
		closeErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("app: extract %s: %w", hdr.Name, copyErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if n != hdr.Size {
			return fmt.Errorf("app: extract %s: short write", hdr.Name)
		}
		return nil
	}
}

// sha256SumFile 计算文件 sha256 hex。
func sha256SumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("app: open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("app: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// --- --json 契约（红线 2：恒退出 0，stdout 仅最终一行；进度一律 stderr）---

// selfUpdateOKJSON 是 `self-update --json` 的成功输出。
type selfUpdateOKJSON struct {
	OK        bool   `json:"ok"`
	UpdatedTo string `json:"updated_to"`
}

// selfUpdateJSONLine 是主体流程 JSON 行的可注入编排形态（测试以假 env 构造——
// self-update 是破坏性编排，测试绝不可经 ProductionEnv 触碰真实可执行文件/launchd）。
func selfUpdateJSONLine(ctx context.Context, home paths.Home, env selfUpdateEnv) string {
	tag, err := SelfUpdate(ctx, home, env)
	if err != nil {
		return jsonErrorLine(err)
	}
	return marshalSelfUpdateOK(tag)
}
