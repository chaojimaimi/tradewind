package app

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/release"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// coreAssetName 返回上游 mihomo 资产正名（固定取无 go 版本后缀变体，
// 名字不符中止——batch3 风险表）。
func coreAssetName(tag string) string {
	return "mihomo-darwin-arm64-" + tag + ".gz"
}

// coreVersionRe 从 `mihomo -v` 首行提取 vX.Y.Z 形态 token（F7 实测：
// 首行 "Mihomo Meta v1.19.34 darwin arm64 with go1.26.11 …"，子串匹配可行）。
var coreVersionRe = regexp.MustCompile(`v\d+(\.\d+)+[^ ]*`)

// extractCoreVersion 从 -v 输出提取版本 token；未命中返回首行原文（不硬编码）。
func extractCoreVersion(out string) string {
	first := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if v := coreVersionRe.FindString(first); v != "" {
		return v
	}
	return first
}

// SelfUpdateCore 编排 `tradewind self-update --core`（batch3 §2.3 --core 序）。
// 返回 updated_to（已是最新时为当前 tag）。失败自动回滚到 .prev（保留一版）。
func SelfUpdateCore(ctx context.Context, home paths.Home, env selfUpdateEnv) (string, error) {
	// 步 1：上游 latest（公开仓，免鉴权）
	client := env.newUpstreamClient()
	rel, err := client.UpstreamLatestRelease(ctx)
	if err != nil {
		return "", err
	}
	tag := rel.Tag
	asset, ok := rel.AssetByName(coreAssetName(tag))
	if !ok {
		return "", fmt.Errorf("app: 上游 %s 缺资产 %s（名字不符中止）", tag, coreAssetName(tag))
	}
	// 步 2：当前内核版本比对（运行中 /version 探针；未运行 bin/mihomo -v 本地回读，评审 P3）
	if coreIsUpstreamVersion(ctx, home, env, tag) {
		fmt.Fprintf(env.progress, "内核已是最新（%s），nothing to do\n", tag)
		return tag, nil
	}
	// 拉回所需 exe 先解析（kickstart 分支不用，spawn 分支必需；快失败）
	callerExe, err := env.executable()
	if err != nil {
		return "", fmt.Errorf("app: resolve executable: %w", err)
	}
	// 步 3：下载 → digest 优先校验（缺失降级）→ gunzip → 试运行 -v 回读
	gzPath := home.Join("mihomo-update.gz")
	defer os.Remove(gzPath)
	fmt.Fprintf(env.progress, "downloading %s ...\n", asset.Name)
	if err := downloadUpstreamAssetToFile(ctx, client, asset, gzPath); err != nil {
		return "", err
	}
	if err := verifyUpstreamDigest(asset, gzPath); err != nil {
		return "", err
	}
	candidate := home.Join("mihomo-update.bin")
	defer os.Remove(candidate)
	if err := gunzipFile(gzPath, candidate); err != nil {
		return "", err
	}
	if err := os.Chmod(candidate, 0o755); err != nil {
		return "", fmt.Errorf("app: chmod candidate: %w", err)
	}
	if out, err := env.runBinaryVersion(candidate, "-v"); err != nil ||
		!strings.Contains(strings.SplitN(out, "\n", 2)[0], tag) {
		return "", fmt.Errorf("app: 候选内核 -v 回读不含 %s；中止不动盘", tag)
	}
	// 步 4：内核运行中 → Stop 优雅停；.prev/.prev.sha256 保留一版回滚（三审 P3）
	wasRunning := env.reachable(home)
	if wasRunning {
		fmt.Fprintln(env.progress, "core is running; stopping via graceful shutdown first...")
		if err := env.stopCore(home); err != nil {
			return "", err
		}
	}
	prev, prevSidecar := home.Bin()+".prev", home.Sidecar()+".prev.sha256"
	hasPrev := false
	if _, err := os.Stat(home.Bin()); err == nil {
		if err := os.Rename(home.Bin(), prev); err != nil {
			return "", fmt.Errorf("app: keep prev core: %w", err)
		}
		if err := os.Rename(home.Sidecar(), prevSidecar); err != nil {
			_ = os.Rename(prev, home.Bin()) // 收敛半途态
			return "", fmt.Errorf("app: keep prev sidecar: %w", err)
		}
		hasPrev = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := replaceBinary(candidate, home.Bin()); err != nil {
		restorePrevCore(home, hasPrev, prev, prevSidecar)
		return "", err
	}
	// 重写 mihomo.sha256 sidecar（F6 格式 <hex>  <绝对路径>，crypto/sha256 本地算）
	sum, err := sha256SumFile(home.Bin())
	if err != nil {
		restorePrevCore(home, hasPrev, prev, prevSidecar)
		return "", err
	}
	if err := store.WriteFileRaw(home.Sidecar(), []byte(fmt.Sprintf("%s  %s\n", sum, home.Bin()))); err != nil {
		restorePrevCore(home, hasPrev, prev, prevSidecar)
		return "", err
	}
	// 步 5（未运行分支）：换盘 + sidecar 即可，无回滚触发点（评审 P3）
	if !wasRunning {
		fmt.Fprintln(env.progress, "core replaced (sidecar rewritten); takes effect on next start（下次启动生效）")
		return tag, nil
	}
	// 步 5：拉回内核（同主体步 7 分支）→ /version 握手且版本 == tag
	bringBack := func() error { return bringCoreBack(env, callerExe) }
	if err := bringBack(); err != nil {
		return "", coreRollback(ctx, env, home, hasPrev, prev, prevSidecar, bringBack)
	}
	secret := store.ReadSecret(home)
	if !env.waitReady(home, secret, selfUpdateHandshakeTimeout) || !coreVersionMatchesTag(env, home, tag) {
		// 步 6：失败自动回滚 → 再拉回 → 报错（不硬编码版本号）
		return "", coreRollback(ctx, env, home, hasPrev, prev, prevSidecar, bringBack)
	}
	fmt.Fprintf(env.progress, "core self-updated -> %s, core restarted\n", tag)
	return tag, nil
}

// coreIsUpstreamVersion 判断当前内核是否已是上游 tag：运行中经 /version 探针；
// 未运行 `bin/mihomo -v` 本地回读首行子串匹配（评审 P3）。
func coreIsUpstreamVersion(ctx context.Context, home paths.Home, env selfUpdateEnv, tag string) bool {
	if env.reachable(home) {
		v, ok := env.coreVersionProbe(home)
		return ok && strings.Contains(v, tag)
	}
	if _, err := os.Stat(home.Bin()); err != nil {
		return false
	}
	out, err := env.runBinaryVersion(home.Bin(), "-v")
	if err != nil {
		return false
	}
	return strings.Contains(strings.SplitN(out, "\n", 2)[0], tag)
}

// coreVersionMatchesTag 握手版本校验：/version 返回含 tag。
func coreVersionMatchesTag(env selfUpdateEnv, home paths.Home, tag string) bool {
	v, ok := env.coreVersionProbe(home)
	return ok && strings.Contains(v, tag)
}

// restorePrevCore 收敛半途态（新二进制/sidecar 落位失败时恢复 .prev）。
func restorePrevCore(home paths.Home, hasPrev bool, prev, prevSidecar string) {
	if !hasPrev {
		return
	}
	_ = os.Remove(home.Bin())
	_ = os.Rename(prev, home.Bin())
	_ = os.Rename(prevSidecar, home.Sidecar())
}

// coreRollback 失败自动回滚（batch3 §2.3 --core 步 6）：停掉残存新实例 → 恢复
// .prev 与 .prev.sha256 → 再拉回 → 报错"已回滚到 <回读 .prev -v 的版本>"
// （不硬编码版本号，评审 P3）。
func coreRollback(ctx context.Context, env selfUpdateEnv, home paths.Home, hasPrev bool, prev, prevSidecar string, bringBack func() error) error {
	_ = env.stopCore(home) // 回滚期尽力而为：停掉可能残存的新内核实例
	if hasPrev {
		if err := os.Remove(home.Bin()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("app: rollback: remove new core: %w", err)
		}
		if err := os.Rename(prev, home.Bin()); err != nil {
			return fmt.Errorf("app: rollback: restore prev core: %w", err)
		}
		if err := os.Rename(prevSidecar, home.Sidecar()); err != nil {
			return fmt.Errorf("app: rollback: restore prev sidecar: %w", err)
		}
	}
	ver := "unknown"
	if out, err := env.runBinaryVersion(home.Bin(), "-v"); err == nil && strings.TrimSpace(out) != "" {
		ver = extractCoreVersion(out)
	}
	if err := bringBack(); err != nil {
		return fmt.Errorf("app: 回滚后拉回失败: %w; run `tradewind start` manually", err)
	}
	// 无 .prev（落位前 bin 缺失的竞态）没有可恢复物，分叉文案防误导（code-reviewer P3-1）
	if !hasPrev {
		return fmt.Errorf("app: 无 prev 内核可回滚，新内核 %s 已保留在盘；请手动 start 确认可用", ver)
	}
	return fmt.Errorf("app: 已回滚到 %s（新内核握手失败/版本不符）", ver)
}

// verifyUpstreamDigest 优先 API assets[].digest 内联 sha256 对比（F7，格式
// "sha256:<hex>"）；digest 缺失降级为 TLS+官方域直下（无附加校验）。
func verifyUpstreamDigest(asset release.Asset, path string) error {
	if !strings.HasPrefix(asset.Digest, "sha256:") {
		return nil
	}
	want := strings.ToLower(strings.TrimPrefix(asset.Digest, "sha256:"))
	got, err := sha256SumFile(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("app: 上游 %s digest 不符: got %s, want %s；中止不动盘", asset.Name, got, want)
	}
	return nil
}

// downloadUpstreamAssetToFile 下载上游资产到 dest（0600；octet-stream 单通道）。
func downloadUpstreamAssetToFile(ctx context.Context, client *release.Client, asset release.Asset, dest string) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("app: create %s: %w", dest, err)
	}
	n, err := client.DownloadUpstreamAsset(ctx, release.UpstreamRepo, asset, f)
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("app: download upstream asset %d: %w", asset.ID, err)
	}
	if closeErr != nil {
		return closeErr
	}
	if n == 0 {
		return fmt.Errorf("app: download upstream asset %d: empty body", asset.ID)
	}
	return nil
}

// gunzipFile 解压 gz 到 dest。
func gunzipFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("app: open %s: %w", src, err)
	}
	defer in.Close()
	gz, err := gzip.NewReader(in)
	if err != nil {
		return fmt.Errorf("app: gunzip %s: %w", src, err)
	}
	defer gz.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("app: create %s: %w", dest, err)
	}
	_, copyErr := io.Copy(out, gz)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("app: gunzip write %s: %w", dest, copyErr)
	}
	return closeErr
}

// SelfUpdateJSONLine 是 `self-update [--core] --json` 的最终输出行（成功
// {"ok":true,"updated_to":tag} / 失败 {"error":...}；进度/步骤一律 stderr）。
func SelfUpdateJSONLine(ctx context.Context, home paths.Home, withCore bool) string {
	env := ProductionEnv(ctx, home, os.Stderr)
	if withCore {
		return selfUpdateCoreJSONLine(ctx, home, env)
	}
	return selfUpdateJSONLine(ctx, home, env)
}

// selfUpdateCoreJSONLine 是 --core 流的 JSON 行可注入编排形态（测试以假 env 构造）。
func selfUpdateCoreJSONLine(ctx context.Context, home paths.Home, env selfUpdateEnv) string {
	tag, err := SelfUpdateCore(ctx, home, env)
	if err != nil {
		return jsonErrorLine(err)
	}
	return marshalSelfUpdateOK(tag)
}

// marshalSelfUpdateOK 序列化成功行 {"ok":true,"updated_to":tag}。
func marshalSelfUpdateOK(tag string) string {
	data, merr := json.Marshal(selfUpdateOKJSON{OK: true, UpdatedTo: tag})
	if merr != nil {
		return jsonErrorLine(merr)
	}
	return string(data)
}
