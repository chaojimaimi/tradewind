// broker_allow.go：代理入口的白名单路由判定（批⑥b §4.3，I4）——纯函数，无网络、
// 无 IO，全部判定输入是字符串与字节。放行清单是内核路由表（mihomo v1.19.34 固定
// 40 条）里挑出的 29 条，逐条写死、不含通配符（param 只占一个段）；其余一律拒绝。
// PATCH /configs 的内核处理器能热改 20 多个字段（端口、TUN、allow-lan……），闸门
// 只放行键 ⊆ {mode}——端口、TUN 等属于服务钉死或须走 `tradewind settings` 的配置，
// 面板不得经代理改。
package service

import (
	"bytes"
	"encoding/json"
	"strings"
)

// 路径与请求体上限。
const (
	brokerMaxPathBytes  = 2048     // routePath 总长上限
	brokerMaxBodyBytes  = 64 << 10 // 非 WebSocket 请求体上限
	brokerMaxPatchBytes = 16 << 10 // PATCH /configs 请求体上限
)

// routeShape 是一条放行路由的形状：方法 + 逐段字面量；param 表示"恰一个非空原始段"。
type routeShape struct {
	method string
	segs   []string
}

// param 占位一个原始段（逐字节比对时匹配任意非空段；与 chi 的 {param} 消费规则
// 同形——到下一个字面 / 为止，段内不解码）。
const param = ""

// allowedRoutes 是放行的 29 条内核路由（出处 hub/route v1.19.34：server.go 及各
// *Router()；见 broker_allow_test.go 的内核 40 条全集对照）。段内百分号编码照常
// 放行——它改变不了分段，内核恰恰靠段内 %2F 支持名字里带 / 的节点与策略组。
var allowedRoutes = []routeShape{
	{"GET", []string{"version"}},
	{"GET", []string{"configs"}},
	{"PATCH", []string{"configs"}}, // 唯一带键闸的路由：见 isModeGateRoute
	{"GET", []string{"proxies"}},
	{"GET", []string{"proxies", param}},
	{"PUT", []string{"proxies", param}},
	{"DELETE", []string{"proxies", param}},
	{"GET", []string{"proxies", param, "delay"}},
	{"GET", []string{"group"}},
	{"GET", []string{"group", param}},
	{"GET", []string{"group", param, "delay"}},
	{"GET", []string{"rules"}},
	{"GET", []string{"connections"}},
	{"DELETE", []string{"connections"}},
	{"DELETE", []string{"connections", param}},
	{"GET", []string{"providers", "proxies"}},
	{"GET", []string{"providers", "proxies", param}},
	{"PUT", []string{"providers", "proxies", param}},
	{"GET", []string{"providers", "proxies", param, "healthcheck"}},
	{"GET", []string{"providers", "proxies", param, param}},
	{"GET", []string{"providers", "proxies", param, param, "healthcheck"}},
	{"GET", []string{"providers", "rules"}},
	{"PUT", []string{"providers", "rules", param}},
	{"POST", []string{"cache", "fakeip", "flush"}},
	{"POST", []string{"cache", "dns", "flush"}},
	{"GET", []string{"dns", "query"}},
	{"GET", []string{"traffic"}},
	{"GET", []string{"memory"}},
	{"GET", []string{"logs"}},
}

// allowedModes 是 PATCH /configs 唯一放行的键 mode 的合法取值（小写；内核的
// tunnel.TunnelMode.UnmarshalText 先 ToLower 再查表——大小写不敏感，本表与之同判）。
var allowedModes = map[string]bool{"rule": true, "global": true, "direct": true}

// routeDecision 是代理对一个请求的处置。
type routeDecision int

const (
	routeDeny      routeDecision = iota // 403：不在允许清单
	routeForward                        // 原样转发
	routeModePatch                      // PATCH /configs：请求体只许 {"mode": …}
)

// routePathOf 返回内核路由器会看到的同一个字符串（github.com/metacubex/chi 的判定
// 输入：RawPath 非空取 RawPath，否则取 Path）。代理必须用同一个字符串分段——否则
// 代理眼里的形状可能与内核眼里的不同（例如 RawPath 里的 %2F 取 Path 就成了多一段）。
func routePathOf(rawPath, path string) string {
	if rawPath != "" {
		return rawPath
	}
	return path
}

// decideRoute 判定一个请求，严格按序：
//  1. 总长上限；
//  2. 剥掉恰好一个末尾 /（chi 的 Mount 子路由对尾斜杠同义）；
//  3. 剥完为空、或不以 / 开头 → 拒（覆盖 GET /、OPTIONS *、无前导斜杠）；
//  4. 按 / 字面切段，任一段为空 / 字面 . / 字面 .. → 拒（改变分段形状的输入；
//     段内 %2F、%2E 等编码不解码、照常放行）；
//  5. 与 allowedRoutes 逐条对合：方法相等（大小写敏感；HEAD/OPTIONS 不在表内自然
//     被拒）、段数相等、每段是 param 或逐字节相等；
//  6. 命中 PATCH /configs → routeModePatch；其它命中 → routeForward；未命中 → 拒。
func decideRoute(method, rawPath, path string) routeDecision {
	p := routePathOf(rawPath, path)
	if len(p) > brokerMaxPathBytes {
		return routeDeny
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" || !strings.HasPrefix(p, "/") {
		return routeDeny
	}
	segs := strings.Split(p[1:], "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return routeDeny
		}
	}
	for i := range allowedRoutes {
		shape := &allowedRoutes[i]
		if shape.method != method || len(shape.segs) != len(segs) {
			continue
		}
		if !segmentsMatch(shape.segs, segs) {
			continue
		}
		if isModeGateRoute(shape) {
			return routeModePatch // 表内唯一一条：/configs 的 mode 键闸
		}
		return routeForward
	}
	return routeDeny
}

// segmentsMatch 逐段比对：pattern 段是 param 即匹配任意非空段，否则逐字节相等。
func segmentsMatch(pattern, segs []string) bool {
	for i, want := range pattern {
		if want != param && want != segs[i] {
			return false
		}
	}
	return true
}

// isModeGateRoute 报告一条放行路由是否带键闸（请求体只许 {"mode": …}）。按整条路由
// 判、不按方法判：表里将来再加第二条 PATCH 路由时，不会被无声地套上 /configs 的闸。
func isModeGateRoute(s *routeShape) bool {
	return s.method == "PATCH" && len(s.segs) == 1 && s.segs[0] == "configs"
}

// modePatchAllowed 判定 PATCH /configs 的请求体是否"键 ⊆ {mode} 且取值合法"。
// WHY 解码进 map 而不是结构体：null 解进结构体不报错、会把闸放过去；map 解出 nil
// 一眼可辨。重复键两边（map 与内核的解码器）都是"后者胜"，结论一致。
func modePatchAllowed(body []byte) bool {
	if len(body) > brokerMaxPatchBytes {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return false // 非法 JSON、数组、字符串、数字
	}
	if fields == nil || dec.More() {
		return false // null、尾随数据
	}
	for key, value := range fields {
		if key != "mode" {
			return false
		}
		var mode string
		if json.Unmarshal(value, &mode) != nil {
			return false // 必须是字符串（null 也不行——解出来是空串，不合法）
		}
		if !allowedModes[strings.ToLower(mode)] {
			return false
		}
	}
	return true
}

// routeLogText 把方法/路径压成可安全写日志的形态：只保留 [A-Za-z0-9/_-]，其余字节
// 丢弃，最多 brokerRouteLogMax 字节（I6：未受信输入不原样进日志——换行能劈出行、
// ANSI 转义能改写终端显示）。
func routeLogText(s string) string {
	b := make([]byte, 0, brokerRouteLogMax)
	for i := 0; i < len(s) && len(b) < brokerRouteLogMax; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b = append(b, c)
		case c == '/' || c == '_' || c == '-':
			b = append(b, c)
		}
	}
	return string(b)
}
