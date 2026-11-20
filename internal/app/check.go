package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/release"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/version"
)

// AssetNameFor 返回 CLI 整包资产名（tag 含 v 前缀，与 release.sh 生成侧同名钉死，
// batch3 §2.2/§4.2）。
func AssetNameFor(tag string) string {
	return "tradewind-" + tag + "-darwin-arm64.tar.gz"
}

// SidecarNameFor 返回 sha256 sidecar 资产名（内容格式 <hex>  <tarball 文件名>，
// 与 F6 sha256sum 风格同构）。
func SidecarNameFor(tag string) string {
	return AssetNameFor(tag) + ".sha256"
}

// CheckUpdateResult 是 version --check 的结构化结果。
type CheckUpdateResult struct {
	Current         string
	Latest          string
	UpdateAvailable bool
	Asset           string
	AssetSize       int64
	// rel 保留资产清单（self-update 步 2/3 按资产 ID 走 octet-stream 通道；不外发）。
	rel *release.Release
}

// checkUpdateWith 在给定客户端上执行版本比对（CheckUpdate 与 self-update 步 1 共用）。
func checkUpdateWith(ctx context.Context, client *release.Client) (CheckUpdateResult, error) {
	rel, err := client.LatestRelease(ctx)
	if err != nil {
		return CheckUpdateResult{}, err
	}
	assetName := AssetNameFor(rel.Tag)
	asset, ok := rel.AssetByName(assetName)
	if !ok {
		return CheckUpdateResult{}, fmt.Errorf("app: 目标版本 %s 缺资产 %s（旧 release 无整包资产，不可自升级）", rel.Tag, assetName)
	}
	return CheckUpdateResult{
		Current:         version.Version,
		Latest:          rel.Tag,
		UpdateAvailable: rel.Tag != version.Version,
		Asset:           assetName,
		AssetSize:       asset.Size,
		rel:             rel,
	}, nil
}

// CheckUpdate 编排 `tradewind version --check`：比对 internal/version.Version 与
// GitHub 最新 release tag（显式相等性判定——tag != current 即可更新，不猜 semver
// 排序，发布节奏由用户掌握）。progress 接收人类可读输出（--json 模式传 io.Discard）。
func CheckUpdate(ctx context.Context, home paths.Home, progress io.Writer) (CheckUpdateResult, error) {
	if version.Version == "dev" {
		return CheckUpdateResult{}, errors.New("app: 开发构建无法比对（当前版本 dev，无比对基线）")
	}
	token, err := release.ReadToken(home)
	if err != nil {
		return CheckUpdateResult{}, err
	}
	client := newReleaseClient(token, func() *http.Client { return selfEgressClient(ctx, home) })
	res, err := checkUpdateWith(ctx, client)
	if err != nil {
		return CheckUpdateResult{}, err
	}
	fmt.Fprintf(progress, "current: %s\nlatest:  %s\n", res.Current, res.Latest)
	if res.UpdateAvailable {
		fmt.Fprintf(progress, "update available: yes (%s, %d bytes)\n", res.Asset, res.AssetSize)
	} else {
		fmt.Fprintln(progress, "update available: no")
	}
	return res, nil
}

// selfEgressClient 直连失败时的自举出口：本地内核在跑 → 经 127.0.0.1:mixed 代理
// （自举，batch3 §2.1；复用 update.go 的 proxyClient 构造）。
func selfEgressClient(ctx context.Context, home paths.Home) *http.Client {
	if !CoreReachable(home) {
		return nil
	}
	settings, err := store.LoadSettings(home)
	if err != nil {
		return nil
	}
	client, err := proxyClient(settings.MixedPort)
	if err != nil {
		return nil
	}
	return client
}

// --- --json 契约（恒退出 0：stdout 仅最终一行；错误也是 {"error":...}）---

// checkUpdateJSON 是 `version --check --json` 的成功输出结构。
type checkUpdateJSON struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Asset           string `json:"asset"`
	AssetSize       int64  `json:"asset_size"`
}

// errorJSON 是五个新命令面 --json 模式共用的错误形态（batch3 红线 2）。
type errorJSON struct {
	Error string `json:"error"`
}

// jsonErrorLine 把错误折叠为单行 {"error":...}（换行由 json.Marshal 转义，
// 保证 stdout 仅一行；红线：错误文案不含 token）。
func jsonErrorLine(err error) string {
	data, merr := json.Marshal(errorJSON{Error: err.Error()})
	if merr != nil {
		return `{"error":"internal error"}`
	}
	return string(data)
}

// MarshalCheckUpdateJSON 序列化检查结果（缩进关闭，壳按 stdout 单行 JSON 解析）。
func MarshalCheckUpdateJSON(r CheckUpdateResult) (string, error) {
	data, err := json.Marshal(checkUpdateJSON{
		Current:         r.Current,
		Latest:          r.Latest,
		UpdateAvailable: r.UpdateAvailable,
		Asset:           r.Asset,
		AssetSize:       r.AssetSize,
	})
	if err != nil {
		return "", fmt.Errorf("app: marshal check update json: %w", err)
	}
	return string(data), nil
}

// CheckUpdateJSONLine 是 `version --check --json` 的最终输出行（恒退出 0 契约：
// 失败折叠为 {"error":...} 单行，壳侧零容错解析）。
func CheckUpdateJSONLine(ctx context.Context, home paths.Home) string {
	res, err := CheckUpdate(ctx, home, io.Discard)
	if err != nil {
		return jsonErrorLine(err)
	}
	line, err := MarshalCheckUpdateJSON(res)
	if err != nil {
		return jsonErrorLine(err)
	}
	return line
}

// productionReleaseClient 构造生产 release 客户端（默认基址）。
var productionReleaseClient = func(token string, egress func() *http.Client) *release.Client {
	return release.NewClient(token, egress)
}

// newReleaseClient 构造 release 客户端（var 缝隙：测试指向 httptest 假 API，
// 生产为 release.NewClient 默认基址）。
var newReleaseClient = productionReleaseClient
