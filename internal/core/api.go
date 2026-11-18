package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// apiTimeout 是单次内核 API 请求的超时。
const apiTimeout = 10 * time.Second

// apiMaxBody 限制响应体读取上限，防异常内核响应拖垮内存。
const apiMaxBody = 1 << 20

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
