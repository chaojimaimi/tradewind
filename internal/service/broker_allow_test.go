package service

// broker_allow_test.go：白名单路由判定的单元用例（纯函数，无网络无 IO）——29 条放行
// 路由逐一放行、11 条明确拒绝 + 条件挂载 + 组合/畸形输入全拒、允许清单与内核 40 条
// 固定路由的补集关系钉死、RawPath 判定、PATCH mode 键闸、日志净化。

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// TestDecideRouteAllowsKernelRoutes 29 条放行路由各至少一个具体路径（query 不属于
// 路径——decideRoute 的输入只有 method 与 RawPath/Path），外加末尾斜杠变体与段内
// 百分号编码放行。
func TestDecideRouteAllowsKernelRoutes(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   routeDecision
	}{
		{"GET", "/version", routeForward},
		{"GET", "/configs", routeForward},
		{"PATCH", "/configs", routeModePatch},
		{"GET", "/proxies", routeForward},
		{"GET", "/proxies/HK-01", routeForward},
		{"PUT", "/proxies/HK-01", routeForward},
		{"DELETE", "/proxies/HK-01", routeForward},
		{"GET", "/proxies/HK-01/delay", routeForward},
		{"GET", "/group", routeForward},
		{"GET", "/group/MyGroup", routeForward},
		{"GET", "/group/MyGroup/delay", routeForward},
		{"GET", "/rules", routeForward},
		{"GET", "/connections", routeForward},
		{"DELETE", "/connections", routeForward},
		{"DELETE", "/connections/abc123", routeForward},
		{"GET", "/providers/proxies", routeForward},
		{"GET", "/providers/proxies/sub", routeForward},
		{"PUT", "/providers/proxies/sub", routeForward},
		{"GET", "/providers/proxies/sub/healthcheck", routeForward},
		{"GET", "/providers/proxies/sub/node-1", routeForward},
		{"GET", "/providers/proxies/sub/node-1/healthcheck", routeForward},
		{"GET", "/providers/rules", routeForward},
		{"PUT", "/providers/rules/provider-a", routeForward},
		{"POST", "/cache/fakeip/flush", routeForward},
		{"POST", "/cache/dns/flush", routeForward},
		{"GET", "/dns/query", routeForward},
		{"GET", "/traffic", routeForward},
		{"GET", "/memory", routeForward},
		{"GET", "/logs", routeForward},
		// 末尾斜杠变体：剥掉恰好一个末尾 / 后同形放行
		{"GET", "/version/", routeForward},
		{"GET", "/proxies/x/", routeForward},
		{"GET", "/proxies/", routeForward},
		// 段内百分号编码照常放行：改变不了分段；内核靠段内 %2F 支持名字里带 / 的
		// 节点与策略组（%2E 解码后是 .，但它只是一个段名，不是字面 ..）
		{"GET", "/proxies/a%2Fb", routeForward},
		{"GET", "/proxies/%2e%2e", routeForward},
	}
	for _, tc := range cases {
		if got := decideRoute(tc.method, "", tc.path); got != tc.want {
			t.Errorf("decideRoute(%q, %q) = %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

// TestDecideRouteDeniesEverythingElse 明确拒绝的 11 条内核固定路由、4 类条件挂载的
// 形状、"方法 × 形状"不在表内的组合、畸形路径、总长超限——全部拒绝。
func TestDecideRouteDeniesEverythingElse(t *testing.T) {
	cases := []struct{ method, rawPath, path string }{
		// 内核固定路由中被拒绝的 11 条（出处 hub/route v1.19.34）
		{"GET", "", "/"},                // hello
		{"PUT", "", "/configs"},         // updateConfigs
		{"POST", "", "/configs/geo"},    // updateGeoDatabases
		{"PATCH", "", "/rules/disable"}, // disableRules
		{"GET", "", "/storage/k"},       // getStorage
		{"PUT", "", "/storage/k"},       // setStorage
		{"DELETE", "", "/storage/k"},    // deleteStorage
		{"POST", "", "/restart"},        // restart
		{"POST", "", "/upgrade"},        // upgradeCore
		{"POST", "", "/upgrade/ui"},     // updateUI
		{"POST", "", "/upgrade/geo"},    // updateGeoDatabases
		// 条件挂载（external-ui → /ui；log-level debug → /debug）——照"不在清单"处理
		{"GET", "", "/ui"},
		{"GET", "", "/ui/"},
		{"GET", "", "/ui/index.html"},
		{"GET", "", "/debug/pprof/"},
		{"PUT", "", "/debug/gc"},
		// 方法与形状的组合不在表内（方法大小写敏感；HEAD/OPTIONS 不在表里自然被拒）
		{"POST", "", "/version"},
		{"DELETE", "", "/proxies"},
		{"PUT", "", "/connections"},
		{"GET", "", "/cache/fakeip/flush"},
		{"PATCH", "", "/proxies/x"},
		{"HEAD", "", "/version"},
		{"OPTIONS", "", "/version"},
		{"get", "", "/version"},
		// 畸形路径：空串、无前导斜杠、通配请求行、空段、字面 ./..
		{"GET", "", ""},
		{"GET", "", "/"},
		{"GET", "", "//"},
		{"GET", "", "//version"},
		{"GET", "", "/version//"},
		{"GET", "", "/proxies//delay"},
		{"GET", "", "/proxies/./x"},
		{"GET", "", "/proxies/../x"},
		{"GET", "", "/proxies/x/../y"},
		{"GET", "", "/../version"},
		{"GET", "", "/."},
		{"GET", "", "/.."},
		{"GET", "", "/%2e%2e/x"},
		{"GET", "", "*"},
		{"GET", "", "version"},
		// 上面几条即使不拒 . / .. 也会因形状不匹配被拒——下面这些形状本会放行（. / ..
		// 落在 param 位），只因它们是字面 . / .. 才被拒
		{"GET", "", "/proxies/.."},
		{"GET", "", "/proxies/."},
		{"GET", "", "/group/.."},
		{"GET", "", "/providers/proxies/./x"},
		{"GET", "", "/providers/proxies/x/.."},
		// 无前导斜杠：两道规则各自独立地拒（不以 / 开头；去掉首字符后又多出一个空段）
		{"GET", "", "x/proxies/y"},
		// 总长超限：形状本会放行（param 段）的一条，只因超过 2048 被拒
		{"GET", "", "/proxies/" + strings.Repeat("a", 2048)},
	}
	for _, tc := range cases {
		if got := decideRoute(tc.method, tc.rawPath, tc.path); got != routeDeny {
			t.Errorf("decideRoute(%q, %q, %q) = %v, want deny", tc.method, tc.rawPath, tc.path, got)
		}
	}
}

// TestDecideRoutePathLengthBoundary 路径总长上限的边界：形状本会放行的 param 路由，
// 总长恰为 2048 放行、2049 拒——上限独立于形状匹配起作用。
func TestDecideRoutePathLengthBoundary(t *testing.T) {
	atLimit := "/proxies/" + strings.Repeat("a", brokerMaxPathBytes-len("/proxies/"))
	if len(atLimit) != brokerMaxPathBytes {
		t.Fatalf("fixture length = %d, want %d", len(atLimit), brokerMaxPathBytes)
	}
	if got := decideRoute("GET", atLimit, ""); got != routeForward {
		t.Fatalf("decideRoute(len %d) = %v, want forward", len(atLimit), got)
	}
	if got := decideRoute("GET", atLimit+"a", ""); got != routeDeny {
		t.Fatalf("decideRoute(len %d) = %v, want deny", len(atLimit)+1, got)
	}
}

// kernelRoutes 是内核 v1.19.34 固定路由表的 40 条全集（hub/route/server.go:105-160
// 及各 *Router()），形状记法与 allowedRoutes 相同（param 占一个 {name}/{key}/{id}
// 段）。锚定两条事实：允许清单每条都是内核真实存在的形状；内核 40 条里不在允许
// 清单的恰是 11 条明确拒绝。
var kernelRoutes = []routeShape{
	// 顶层（hub/route/server.go:118-136）
	{"GET", []string{}}, // / → hello
	{"GET", []string{"logs"}},
	{"GET", []string{"traffic"}},
	{"GET", []string{"memory"}},
	{"GET", []string{"version"}},
	// configRouter（hub/route/configs.go:25-34）
	{"GET", []string{"configs"}},
	{"PUT", []string{"configs"}},         // updateConfigs
	{"POST", []string{"configs", "geo"}}, // updateGeoDatabases
	{"PATCH", []string{"configs"}},       // patchConfigs
	// proxyRouter（hub/route/proxies.go:24-34）
	{"GET", []string{"proxies"}},
	{"GET", []string{"proxies", param}},
	{"GET", []string{"proxies", param, "delay"}},
	{"PUT", []string{"proxies", param}},
	{"DELETE", []string{"proxies", param}},
	// groupRouter（hub/route/groups.go:20-27）
	{"GET", []string{"group"}},
	{"GET", []string{"group", param}},
	{"GET", []string{"group", param, "delay"}},
	// ruleRouter（hub/route/rules.go:14-20）
	{"GET", []string{"rules"}},
	{"PATCH", []string{"rules", "disable"}}, // disableRules
	// connectionRouter（hub/route/connections.go:17-21）
	{"GET", []string{"connections"}},
	{"DELETE", []string{"connections"}},
	{"DELETE", []string{"connections", param}},
	// proxyProviderRouter / proxyProviderProxyRouter（hub/route/provider.go:16-37）
	{"GET", []string{"providers", "proxies"}},
	{"GET", []string{"providers", "proxies", param}},
	{"PUT", []string{"providers", "proxies", param}},
	{"GET", []string{"providers", "proxies", param, "healthcheck"}},
	{"GET", []string{"providers", "proxies", param, param}},
	{"GET", []string{"providers", "proxies", param, param, "healthcheck"}},
	// ruleProviderRouter（hub/route/provider.go:113-121）
	{"GET", []string{"providers", "rules"}},
	{"PUT", []string{"providers", "rules", param}},
	// cacheRouter（hub/route/cache.go:12-15）
	{"POST", []string{"cache", "fakeip", "flush"}},
	{"POST", []string{"cache", "dns", "flush"}},
	// dnsRouter（hub/route/dns.go:17-19）
	{"GET", []string{"dns", "query"}},
	// storageRouter（hub/route/storage.go:15-20）
	{"GET", []string{"storage", param}},
	{"PUT", []string{"storage", param}},
	{"DELETE", []string{"storage", param}},
	// restartRouter（hub/route/restart.go:19-22）
	{"POST", []string{"restart"}},
	// upgradeRouter（hub/route/upgrade.go:16-23）
	{"POST", []string{"upgrade"}},
	{"POST", []string{"upgrade", "ui"}},
	{"POST", []string{"upgrade", "geo"}},
}

// shapeKey 把形状压成单射标识：方法 + 段数 + 各段。WHY 带段数：根形状（0 段）与
// "单个 param 段"的形状只以 / 连接 segs 会撞成同一个空串。
func shapeKey(s routeShape) string {
	return fmt.Sprintf("%s#%d:%s", s.method, len(s.segs), strings.Join(s.segs, "/"))
}

// TestAllowListIsExactlyTheKernelComplement 允许清单 ⊆ 内核 40 条；补集恰是 11 条
// 明确拒绝，且每条 decideRoute 为拒；带键闸的路由恰一条（PATCH /configs）。
func TestAllowListIsExactlyTheKernelComplement(t *testing.T) {
	kernel := make(map[string]bool, len(kernelRoutes))
	for _, s := range kernelRoutes {
		kernel[shapeKey(s)] = true
	}
	if len(kernel) != 40 {
		t.Fatalf("kernel fixed routes = %d distinct shapes, want exactly 40 (v1.19.34 hub/route)", len(kernel))
	}
	allowed := make(map[string]bool, len(allowedRoutes))
	gated := 0
	for _, s := range allowedRoutes {
		key := shapeKey(s)
		if !kernel[key] {
			t.Fatalf("allowed route %v %v is not a kernel route (hand-written shape drift)", s.method, s.segs)
		}
		allowed[key] = true
		if isModeGateRoute(&s) {
			gated++
			if s.method != "PATCH" || len(s.segs) != 1 || s.segs[0] != "configs" {
				t.Fatalf("key-gated route = %v %v, want only PATCH /configs", s.method, s.segs)
			}
		}
	}
	if len(allowed) != 29 {
		t.Fatalf("allowed routes = %d distinct shapes, want exactly 29", len(allowed))
	}
	if gated != 1 {
		t.Fatalf("key-gated routes = %d, want exactly 1 (PATCH /configs)", gated)
	}
	denied := []struct {
		shape        routeShape
		method, path string
		note         string
	}{
		{routeShape{"GET", []string{}}, "GET", "/", "hello"},
		{routeShape{"PUT", []string{"configs"}}, "PUT", "/configs", "updateConfigs"},
		{routeShape{"POST", []string{"configs", "geo"}}, "POST", "/configs/geo", "updateGeoDatabases"},
		{routeShape{"PATCH", []string{"rules", "disable"}}, "PATCH", "/rules/disable", "disableRules"},
		{routeShape{"GET", []string{"storage", param}}, "GET", "/storage/k", "getStorage"},
		{routeShape{"PUT", []string{"storage", param}}, "PUT", "/storage/k", "setStorage"},
		{routeShape{"DELETE", []string{"storage", param}}, "DELETE", "/storage/k", "deleteStorage"},
		{routeShape{"POST", []string{"restart"}}, "POST", "/restart", "restart"},
		{routeShape{"POST", []string{"upgrade"}}, "POST", "/upgrade", "upgradeCore"},
		{routeShape{"POST", []string{"upgrade", "ui"}}, "POST", "/upgrade/ui", "updateUI"},
		{routeShape{"POST", []string{"upgrade", "geo"}}, "POST", "/upgrade/geo", "updateGeoDatabases"},
	}
	for _, d := range denied {
		key := shapeKey(d.shape)
		if !kernel[key] {
			t.Fatalf("denied route %v %v is not a kernel route", d.shape.method, d.shape.segs)
		}
		if allowed[key] {
			t.Fatalf("denied route %v %v must not be in allowedRoutes", d.shape.method, d.shape.segs)
		}
		if got := decideRoute(d.method, "", d.path); got != routeDeny {
			t.Errorf("kernel-only route %s %s (%s) = %v, want deny", d.method, d.path, d.note, got)
		}
	}
	// 40 - 29 == 11：补集恰好全部核销（没有第三类条目）
	if len(kernel)-len(allowed) != len(denied) {
		t.Fatalf("kernel(%d) - allowed(%d) != denied list(%d): the complement is not exactly the denied set",
			len(kernel), len(allowed), len(denied))
	}
}

// TestRouteDecisionUsesRawPathWhenSet 钉住"代理与内核用同一个字符串"：RawPath 非空
// 取 RawPath——若取 Path，%2F 解码后多出一段，形状就变了。
func TestRouteDecisionUsesRawPathWhenSet(t *testing.T) {
	if got := decideRoute("GET", "/proxies/a%2Fb", "/proxies/a/b"); got != routeForward {
		t.Fatalf("decideRoute with RawPath /proxies/a%%2Fb = %v, want forward (Path would be 3 segments)", got)
	}
	if got := decideRoute("GET", "/proxies/%2F", "/proxies//"); got != routeForward {
		t.Fatalf("decideRoute with RawPath /proxies/%%2F = %v, want forward", got)
	}
	// 只给 Path（RawPath 空）则看到空段 → 拒：同一输入按不同字符串判定结论不同，
	// 证明判定确实吃的是 RawPath
	if got := decideRoute("GET", "", "/proxies//"); got != routeDeny {
		t.Fatalf("decideRoute with Path /proxies// = %v, want deny", got)
	}
}

// TestModePatchAllowed PATCH /configs 的键闸：键 ⊆ {mode} 且取值合法（大小写不敏感，
// 同内核 UnmarshalText）才放行。
func TestModePatchAllowed(t *testing.T) {
	allowed := []string{
		`{"mode":"rule"}`,
		`{"mode":"global"}`,
		`{"mode":"direct"}`,
		`{"mode":"Rule"}`,         // 大小写不敏感（同内核 ToLower 后查表）
		`{}`,                      // 空对象：无键可拒
		`{"mode":"\u0072ule"}`,    // JSON 转义解码后是合法取值
		"  {\"mode\":\"rule\"}  ", // 前后空白
		// 恰 16 KiB（合法 JSON，键与取值都合规）：上限之内放行
		strings.Repeat(" ", brokerMaxPatchBytes-len(`{"mode":"rule"}`)) + `{"mode":"rule"}`,
	}
	for _, body := range allowed {
		if !modePatchAllowed([]byte(body)) {
			t.Errorf("modePatchAllowed(%q) = false, want true", body)
		}
	}
	denied := []string{
		`{"port":7890}`,
		`{"mode":"rule","port":7890}`,
		`{"tun":{"enable":true}}`,
		`{"allow-lan":true}`,
		`{"mode":"bogus"}`,
		`{"mode":1}`,
		`{"mode":null}`,
		`[]`,
		`"rule"`,
		`null`,
		`{"mode":"rule"} {"mode":"x"}`, // 尾随数据
		`{"mode":"rule"`,               // 截断
		``,
		// 合法 JSON、键与取值都合规，只因超过 16 KiB 被拒（上面的非法输入会被解码
		// 拒绝，钉不住体积上限本身）
		strings.Repeat(" ", brokerMaxPatchBytes) + `{"mode":"rule"}`,
	}
	for _, body := range denied {
		if modePatchAllowed([]byte(body)) {
			t.Errorf("modePatchAllowed(%q) = true, want false", body)
		}
	}
	// 16 KiB + 1 字节：超限在解码之前即拒
	if modePatchAllowed(bytes.Repeat([]byte("a"), brokerMaxPatchBytes+1)) {
		t.Fatal("16KiB+1 body must be denied")
	}
}

// TestRouteLogText 日志净化：换行/回车/ANSI 转义/控制字符/非 ASCII 全部丢弃，结果
// 只含 [A-Za-z0-9/_-]，超长输入截到 64 字节（I6）。
func TestRouteLogText(t *testing.T) {
	pattern := regexp.MustCompile(`^[A-Za-z0-9/_-]*$`)
	for _, in := range []string{
		"GET /a\nX-Evil: 1", // 换行
		"/a\rb",             // 回车
		"/a\x1b[31mb",       // ANSI 转义
		"/a\x00\x07b",       // 控制字符
		"/节点/测试",            // 非 ASCII
		"/a%0db",            // 百分号本身丢弃
	} {
		got := routeLogText(in)
		if !pattern.MatchString(got) {
			t.Errorf("routeLogText(%q) = %q, must match ^[A-Za-z0-9/_-]*$", in, got)
		}
	}
	if got := routeLogText(strings.Repeat("a", 100) + "\n" + strings.Repeat("b", 100)); got != strings.Repeat("a", 64) {
		t.Errorf("routeLogText long input = %q (%d bytes), want the first 64 plain bytes", got, len(got))
	}
	if got := routeLogText(strings.Repeat("x", 200)); len(got) != 64 {
		t.Errorf("routeLogText 200-char input = %d bytes, want 64", len(got))
	}
}
