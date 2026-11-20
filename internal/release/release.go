// Package release 提供 GitHub Release 查询与资产下载（批③ B1 self-update 地基）。
//
// 端点纪律（batch3-plan F1/评审 P0）：本仓为 PRIVATE 且发版带 --prerelease，
// releases/latest 端点不返回 prerelease/draft（恒 404）——查询一律
// GET /releases?per_page=10 过滤 draft 取首条。资产下载走
// GET /releases/assets/<id> + Accept: application/octet-stream（私有仓
// browser_download_url 不接受 Bearer 鉴权恒 404；302 至签名 URL 由
// http.Client 默认行为处理，跨域重定向自动丢弃 Authorization 恰为所需）。
//
// 红线：token 只经 HTTP 头传递，绝不落入日志/输出/argv/错误信息
// （错误文案只说"token 缺失或无效"）。
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// DefaultAPIBase 是 GitHub API 基址（测试以 httptest 覆盖 APIBase）。
const DefaultAPIBase = "https://api.github.com"

// Repo 是本仓 slug（PRIVATE，查询与资产下载均需 Bearer）。
const Repo = "chaojimaimi/tradewind"

// UpstreamRepo 是 mihomo 上游仓（PUBLIC，prerelease=false，latest 端点可用）。
const UpstreamRepo = "MetaCubeX/mihomo"

// assetTimeout 是单次 API 请求/资产下载的超时（60s/资产，batch3 §2.3 步 2）。
const assetTimeout = 60 * time.Second

// tokenFilePerm 是 release_token 的唯一合法权限（600，凭据最小暴露）。
const tokenFilePerm = 0o600

// tokenSetupHint 是 token 引导文案（错误信息与 README 自升级章节同文，batch3 §2.1）。
const tokenSetupHint = "未配置 release_token（数据目录），见 README 自升级章节：" +
	"gh auth token > ~/Library/Application\\ Support/tradewind/release_token && " +
	"chmod 600 ~/Library/Application\\ Support/tradewind/release_token"

// ReadToken 读取 release_token（路径由 paths.Home 派生，不写死字面量）。
// 600 权限强制：凭据文件权限过宽直接拒绝并引导 chmod，绝不回显内容。
func ReadToken(home paths.Home) (string, error) {
	info, err := os.Stat(home.ReleaseTokenFile())
	if err != nil {
		return "", errors.New("release: " + tokenSetupHint)
	}
	if info.Mode().Perm() != tokenFilePerm {
		return "", fmt.Errorf("release: release_token 权限过宽（应为 600），先执行 chmod 600 再重试")
	}
	data, err := os.ReadFile(home.ReleaseTokenFile())
	if err != nil {
		return "", errors.New("release: token 缺失或无效")
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("release: token 缺失或无效")
	}
	return token, nil
}

// Release 是本实现所需的 release 字段裁剪（tag + 资产清单）。
type Release struct {
	Tag        string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Asset 是 release 资产条目（Digest 为上游 mihomo 提供的内联摘要 "sha256:<hex>"）。
type Asset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// AssetByName 按资产名定位（未命中返回 false）。
func (r *Release) AssetByName(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// Client 是 GitHub API 客户端：出口默认直连，连接失败且 Egress 提供自举出口时
// 经本地内核代理重试一次（自举，batch3 §2.1）；两次都败报错。
type Client struct {
	// APIBase 可被测试覆盖（httptest 假 API，不出网）。
	APIBase string
	// Token 为空则不带 Authorization 头（上游公开仓场景）。
	Token string
	// HTTP 是直连客户端（测试可注入 Transport 制造定向失败）。
	HTTP *http.Client
	// Egress 自举出口工厂：直连传输层失败时回调一次，返回经 127.0.0.1:mixed 的
	// http.Client；返回 nil = 无自举出口（未运行/构造失败）。
	Egress func() *http.Client
}

// NewClient 构造默认客户端（APIBase=DefaultAPIBase）。
func NewClient(token string, egress func() *http.Client) *Client {
	return &Client{APIBase: DefaultAPIBase, Token: token, Egress: egress}
}

// LatestRelease 查询本仓最新 release（prerelease 是预期目标，不过滤）。
// WHY 不用 releases/latest：该端点不返回 prerelease/draft，与 --prerelease
// 发版策略互斥恒 404（评审 P0）；per_page=10+draft 过滤取首条（三审 P3：
// per_page=1+过滤组合会自我挫败——预建草稿占首条时误报无 release）。
func (c *Client) LatestRelease(ctx context.Context) (*Release, error) {
	return c.latestRelease(ctx, Repo)
}

// UpstreamLatestRelease 查询 mihomo 上游 latest（公开仓，releases/latest 可用性
// 已实查 prerelease=false；仍走列表端点保持单一查询代码路径）。
func (c *Client) UpstreamLatestRelease(ctx context.Context) (*Release, error) {
	return c.latestRelease(ctx, UpstreamRepo)
}

// latestRelease 按仓查询最新 release：列表端点 + draft 过滤取首条。
func (c *Client) latestRelease(ctx context.Context, repo string) (*Release, error) {
	var list []Release
	url := fmt.Sprintf("%s/repos/%s/releases?per_page=10", c.APIBase, repo)
	if err := c.getJSON(ctx, url, &list); err != nil {
		return nil, err
	}
	for i := range list {
		if !list[i].Draft {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("release: repo %s has no published release", repo)
}

// DownloadAsset 经 octet-stream 通道下载本仓资产到 w（302 至签名 URL 由
// http.Client 默认处理），返回写入字节数。
func (c *Client) DownloadAsset(ctx context.Context, a Asset, w io.Writer) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/assets/%d", c.APIBase, Repo, a.ID)
	return c.download(ctx, url, w)
}

// DownloadUpstreamAsset 经同一 octet-stream 通道下载上游资产（公开仓免鉴权，
// 单一通道消除实现分叉）。
func (c *Client) DownloadUpstreamAsset(ctx context.Context, repo string, a Asset, w io.Writer) (int64, error) {
	url := fmt.Sprintf("%s/repos/%s/releases/assets/%d", c.APIBase, repo, a.ID)
	return c.download(ctx, url, w)
}

// getJSON 查询并解码 JSON 响应（鉴权头仅本仓私有查询需要，公开仓带之亦无害）。
func (c *Client) getJSON(ctx context.Context, rawURL string, out any) error {
	resp, err := c.do(ctx, http.MethodGet, rawURL, map[string]string{
		"Accept": "application/vnd.github+json",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("release: read response: %w", err)
	}
	if err := c.statusError(resp, rawURL); err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("release: decode response: %w", err)
	}
	return nil
}

// download 流式下载资产（octet-stream 头），返回写入字节数。
func (c *Client) download(ctx context.Context, rawURL string, w io.Writer) (int64, error) {
	resp, err := c.do(ctx, http.MethodGet, rawURL, map[string]string{
		"Accept": "application/octet-stream",
	})
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err := c.statusError(resp, rawURL); err != nil {
		return 0, err
	}
	n, err := io.Copy(w, resp.Body)
	if err != nil {
		return n, fmt.Errorf("release: download %s: %w", rawURL, err)
	}
	return n, nil
}

// statusError 把非 2xx 折为显式错误（鉴权失败与限流各有专用文案；
// 文案绝不携带 token，URL 不含凭据——token 只在请求头）。
func (c *Client) statusError(resp *http.Response, rawURL string) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return errors.New("release: GitHub 鉴权失败（token 缺失或无效）")
	}
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return fmt.Errorf("release: GitHub API 限流，%s 后重置", rateLimitResetIn(resp.Header.Get("X-RateLimit-Reset")))
	}
	return fmt.Errorf("release: GET %s: status %d", rawURL, resp.StatusCode)
}

// rateLimitResetIn 把 X-RateLimit-Reset（unix 秒）折为可读等待时长。
func rateLimitResetIn(reset string) string {
	sec, err := strconv.ParseInt(reset, 10, 64)
	if err != nil || sec <= 0 {
		return "unknown time"
	}
	d := time.Until(time.Unix(sec, 0))
	if d < 0 {
		return "now"
	}
	return d.Round(time.Second).String()
}

// do 执行请求：默认直连；传输层失败且 Egress 提供自举出口 → 经代理重试一次
// （自举，batch3 §2.1）。HTTP 响应到达（含 4xx/5xx）不触发重试——那是确定性答复。
func (c *Client) do(ctx context.Context, method, rawURL string, headers map[string]string) (*http.Response, error) {
	resp, directErr := c.attempt(ctx, c.directClient(), method, rawURL, headers)
	if directErr == nil {
		return resp, nil
	}
	if c.Egress == nil {
		return nil, directErr
	}
	proxy := c.Egress()
	if proxy == nil {
		return nil, directErr
	}
	resp, err := c.attempt(ctx, proxy, method, rawURL, headers)
	if err != nil {
		return nil, fmt.Errorf("release: direct (%v) and via-local-core (%v) both failed", directErr, err)
	}
	return resp, nil
}

// attempt 单次请求（headers 逐项透传；Authorization 由调用方按需置入）。
func (c *Client) attempt(ctx context.Context, client *http.Client, method, rawURL string, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("release: build request: %w", err)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: %s %s: %w", method, rawURL, err)
	}
	return resp, nil
}

// directClient 直连客户端（60s 上限；测试可注入 HTTP 字段）。
func (c *Client) directClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: assetTimeout}
}
