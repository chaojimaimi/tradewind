package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiTimeout 是单次内核 API 请求的超时。
const apiTimeout = 10 * time.Second

// apiMaxBody 限制响应体读取上限，防异常内核响应拖垮内存。
// M1 提升到 8MB：大订阅的 GET /proxies 全量响应可超 1MB（M1-plan §5.1）。
const apiMaxBody = 8 << 20

// 延迟测试默认参数（M1-plan §5.1 / DESIGN P0 #5）。
const (
	DefaultDelayTimeoutMS = 5000
	DefaultDelayTestURL   = "http://www.gstatic.com/generate_204"
)

// DelaySample 是节点延迟历史中的单条采样（仅 M1 所需字段）。
type DelaySample struct {
	Delay int `json:"delay"`
}

// Proxy 是 GET /proxies 的单条目（字段裁剪到 M1 所需）。
type Proxy struct {
	Name    string        `json:"name"`
	Type    string        `json:"type"`
	Now     string        `json:"now"`
	All     []string      `json:"all"`
	History []DelaySample `json:"history"`
}

// IsGroup 判断该条目是否策略组（Selector/URLTest/Fallback/LoadBalance/Relay）。
func (p Proxy) IsGroup() bool {
	switch p.Type {
	case "Selector", "URLTest", "Fallback", "LoadBalance", "Relay":
		return true
	}
	return false
}

// LastDelay 返回最近一次延迟采样（毫秒）；无记录或记录为 0（未测）时返回 false。
func (p Proxy) LastDelay() (int, bool) {
	if n := len(p.History); n > 0 {
		if d := p.History[n-1].Delay; d > 0 {
			return d, true
		}
	}
	return 0, false
}

// Client 是 mihomo unix socket RESTful 客户端。
// M0 仅需两个端点（M0-plan §5.3）：GET /version、PUT /configs?force=true。
type Client struct {
	hc     *http.Client
	secret string
}

// NewClient 构造经 unix socket 通信的客户端（base URL 的 host 被自定义 Dialer 忽略）。
func NewClient(sockPath, secret string) *Client {
	return &Client{
		hc: &http.Client{
			Timeout: apiTimeout,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var dialer net.Dialer
					return dialer.DialContext(ctx, "unix", sockPath)
				},
			},
		},
		secret: secret,
	}
}

// Version 请求 GET /version，返回内核版本字符串。握手与健康检查共用。
func (c *Client) Version(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("core: parse /version: %w", err)
	}
	return v.Version, nil
}

// PutConfigs 热重载配置：PUT /configs?force=true，body 携带 runtime.yaml 绝对路径。
// SAFE_PATHS 约束：path 必须位于 -d 工作目录内（当前布局 runtime.yaml 与 -d 同目录，成立）。
func (c *Client) PutConfigs(ctx context.Context, configPath string) error {
	payload, err := json.Marshal(map[string]string{"path": configPath})
	if err != nil {
		return fmt.Errorf("core: marshal payload: %w", err)
	}
	_, err = c.do(ctx, http.MethodPut, "/configs?force=true", payload)
	return err
}

// PatchConfigsMode 运行时热切换出站模式：PATCH /configs {"mode": mode}。
// mihomo 语义：mode 属可热生效字段，无需 PUT 整份配置重载（M2-plan §2.1）。
func (c *Client) PatchConfigsMode(ctx context.Context, mode string) error {
	payload, err := json.Marshal(map[string]string{"mode": mode})
	if err != nil {
		return fmt.Errorf("core: marshal payload: %w", err)
	}
	_, err = c.do(ctx, http.MethodPatch, "/configs", payload)
	return err
}

// GetProxies 请求 GET /proxies，返回全量节点/组（键为节点或组名）。
func (c *Client) GetProxies(ctx context.Context) (map[string]Proxy, error) {
	data, err := c.do(ctx, http.MethodGet, "/proxies", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		Proxies map[string]Proxy `json:"proxies"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, fmt.Errorf("core: parse /proxies: %w", err)
	}
	return res.Proxies, nil
}

// SelectProxy 切换组内节点：PUT /proxies/{group}，body {"name": node}（M1-plan §5.1）。
// 组名经 url.PathEscape——emoji/空格必须编码为单个 path 段。
func (c *Client) SelectProxy(ctx context.Context, group, node string) error {
	payload, err := json.Marshal(map[string]string{"name": node})
	if err != nil {
		return fmt.Errorf("core: marshal payload: %w", err)
	}
	_, err = c.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), payload)
	return err
}

// UnpinProxy 清除 URLTest/Fallback 组的手动固定节点：DELETE /proxies/{group}，
// 组恢复自动测速选择（mihomo 官方语义；v0.4.16 用户拍板"恢复自动"）。
// WHY PathEscape 与 SelectProxy 同口径：组名含 emoji/空格。
func (c *Client) UnpinProxy(ctx context.Context, group string) error {
	_, err := c.do(ctx, http.MethodDelete, "/proxies/"+url.PathEscape(group), nil)
	return err
}

// ProxyDelay 单节点延迟测试：GET /proxies/{name}/delay?timeout=&url=，
// 返回毫秒数；内核测试失败（超时/不可达）按非 2xx 折为 error（实测 503）。
func (c *Client) ProxyDelay(ctx context.Context, name string, timeoutMS int, testURL string) (int, error) {
	q := url.Values{}
	q.Set("timeout", strconv.Itoa(timeoutMS))
	q.Set("url", testURL)
	endpoint := "/proxies/" + url.PathEscape(name) + "/delay?" + q.Encode()
	data, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	var res struct {
		Delay int `json:"delay"`
	}
	if err := json.Unmarshal(data, &res); err != nil {
		return 0, fmt.Errorf("core: parse /proxies/{name}/delay: %w", err)
	}
	return res.Delay, nil
}

// do 执行请求；非 2xx 返回携带响应摘要的错误。
func (c *Client) do(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("core: build request: %w", err)
	}
	// 实测 mihomo unix ctl 对 /version 不强制鉴权，但按方案恒带 Bearer（纵深防御，无害）
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("core: %s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, apiMaxBody))
	if err != nil {
		return nil, fmt.Errorf("core: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("core: %s %s: status %d: %s",
			method, endpoint, resp.StatusCode, truncate(string(data), 200))
	}
	return data, nil
}

// truncate 截断字符串用于错误摘要。
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
