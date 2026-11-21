// tun_geoip.go：geoip.metadb 前置检查 + 钉死来源下载（批⑥ §3.2，评审二轮 P2-d
// + 三轮 P2）。geoip 解析依赖数据目录 geoip.metadb——其是否在用取决于当前订阅
// 规则内容（换订阅/新环境即失守，R11）；缺失时 mihomo 首启自动下载在 TUN 接管
// 环境可能失败 → 握手超时。tun on 编排在 settings 翻转前检查：缺失先下载、
// 失败明确报错终止编排（不半开 TUN）。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// 钉死来源（bootstrap 语义：版本内容随发版更新常量——不上线自动追新）。上游
// 资产轮换后 sha256 失配 → 显式报错终止编排（R11 失败出口），随下个发版更新
// 常量或手动放置文件。snapshot 2026-11-21（MetaCubeX/meta-rules-dat，8.4MB）。
const (
	geoipDownloadURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb"
	geoipSHA256      = "41bce1c35748f2748433739e0f3d3093db2fbabe244c159e2d9fa5f5566b1bbf"
	// geoipMaxSize 大小上限防异常响应拖垮内存/磁盘（selfupdate 同款校验形态）。
	geoipMaxSize = 64 << 20
)

// ensureGeoipMetadb 前置检查入口：数据目录已有 geoip.metadb → 就位；缺失 →
// 钉死来源下载（sha256 常量校验 + 大小上限）。参数化变体供单测注入 URL/摘要。
func ensureGeoipMetadb(ctx context.Context, home paths.Home) error {
	return ensureGeoipMetadbWith(ctx, home, geoipDownloadURL, geoipSHA256, geoipDownloadTo)
}

func ensureGeoipMetadbWith(ctx context.Context, home paths.Home, urlStr, wantSHA string,
	fetch func(context.Context, *http.Client, string, string, int64) error) error {
	if _, err := os.Stat(home.GeoIP()); err == nil {
		return nil
	}
	tmp := home.Join("geoip.metadb.download")
	defer os.Remove(tmp)
	if err := fetch(ctx, geoipEgressClient(ctx, home), urlStr, tmp, geoipMaxSize); err != nil {
		return fmt.Errorf("app: geoip.metadb 下载失败: %w（TUN 开启需要 geoip 数据，已中止编排）", err)
	}
	got, err := sha256SumFile(tmp)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, wantSHA) {
		return fmt.Errorf("app: geoip.metadb sha256 mismatch (got %s, want %s)；钉死来源已轮换，请更新 tradewind 或手动放置 %s",
			got, wantSHA, home.GeoIP())
	}
	if err := os.Rename(tmp, home.GeoIP()); err != nil {
		return fmt.Errorf("app: place geoip.metadb: %w", err)
	}
	fmt.Printf("geoip.metadb bootstrap complete (%s...)\n", wantSHA[:12])
	return nil
}

// geoipEgressClient 出口选择（§3.2）：内核在跑 → 经 127.0.0.1:mixed-port 代理
// （拉取走自身隧道）；否则直连。
func geoipEgressClient(ctx context.Context, home paths.Home) *http.Client {
	const geoipFetchTimeout = 120 * time.Second
	transport := &http.Transport{}
	if CoreReachable(home) {
		if settings, err := store.LoadSettings(home); err == nil {
			if proxyURL, perr := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", settings.MixedPort)); perr == nil {
				transport.Proxy = http.ProxyURL(proxyURL)
			}
		}
	}
	return &http.Client{Timeout: geoipFetchTimeout, Transport: transport}
}

// geoipDownloadTo 生产下载实现：GET → 大小上限（Content-Length + LimitReader 双闸）
// → 落盘 0600。变量缝供单测替换（绝不真出网）。
var geoipDownloadTo = func(ctx context.Context, client *http.Client, urlStr, dest string, maxSize int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	if resp.ContentLength > maxSize {
		return fmt.Errorf("content too large (%d bytes)", resp.ContentLength)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxSize+1))
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n == 0 {
		return errors.New("empty body")
	}
	if n > maxSize {
		return fmt.Errorf("content exceeds size limit %d", maxSize)
	}
	return nil
}
