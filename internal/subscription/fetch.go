// Package subscription 负责订阅的拉取与 clash yaml 结构校验。
package subscription

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// UA 是拉取订阅使用的 User-Agent。
// WHY clash 兼容 UA：机场对陌生 UA 常回退返回 base64 节点列表（DESIGN §7.8），
// 与"只支持 clash yaml"冲突；自有 UA 等 M1 做成可配置项后再切。
const UA = "clash.meta/1.19.34 (tradewind/0.1)"

// fetchTimeout 是单次订阅拉取的超时。机场偶发慢响应，必须有界。
const fetchTimeout = 30 * time.Second

// Extra 是 subscription-userinfo 响应头解析出的流量/到期信息（字节/秒时间戳）。
type Extra struct {
	Upload   int64
	Download int64
	Total    int64
	Expire   int64
}

// Result 是一次成功拉取的产物。
type Result struct {
	Body  []byte
	Extra Extra
	// HasExtra 表示订阅响应是否携带 subscription-userinfo（无响应头时 extra 全 0 且此标志为 false）
	HasExtra bool
}

// Fetch 拉取订阅内容并解析流量信息头。
// 非 200 / 超时 / 读失败均返回错误——调用方保证失败不落盘。
func Fetch(ctx context.Context, client *http.Client, url string) (Result, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Result{}, fmt.Errorf("subscription: bad url: %w", err)
	}
	req.Header.Set("User-Agent", UA)
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("subscription: fetch %s: %w", sanitizeURL(url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("subscription: fetch %s: status %d", sanitizeURL(url), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubSize))
	if err != nil {
		return Result{}, fmt.Errorf("subscription: read body: %w", err)
	}
	res := Result{Body: body}
	if raw := resp.Header.Get("subscription-userinfo"); raw != "" {
		if extra, ok := ParseUserinfo(raw); ok {
			res.Extra = extra
			res.HasExtra = true
		}
	}
	return res, nil
}

// maxSubSize 订阅体积上限 16MB：防止异常响应拖垮内存（正常订阅 <1MB）。
const maxSubSize = 16 << 20

// ParseUserinfo 解析 `upload=1; download=2; total=3; expire=4` 格式。
// 宽容解析：缺失/非法字段按 0 处理；全部字段缺失视为解析失败。
func ParseUserinfo(raw string) (Extra, bool) {
	var extra Extra
	seen := 0
	for _, part := range strings.Split(raw, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(k)) {
		case "upload":
			extra.Upload, seen = n, seen+1
		case "download":
			extra.Download, seen = n, seen+1
		case "total":
			extra.Total, seen = n, seen+1
		case "expire":
			extra.Expire, seen = n, seen+1
		}
	}
	return extra, seen > 0
}

// sanitizeURL 隐藏订阅 URL 的查询串（内嵌 token），避免错误信息泄露凭据（DESIGN §2.7.3）。
func sanitizeURL(url string) string {
	if i := strings.IndexByte(url, '?'); i >= 0 {
		return url[:i] + "?***"
	}
	return url
}
