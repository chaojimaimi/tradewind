// tun_env.go：tun 编排家族的可注入环境（批⑥ §2.2；真机 fold-in 增三项检查缝隙）。
package app

import (
	"context"
	"os"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// tunServiceClientT 是特权服务客户端缝隙接口（测试注入假客户端——编排测试绝不
// 触真 /var/run socket；生产为 *service.Client）。
type tunServiceClientT interface {
	Start(generation int) (service.Response, error)
	Stop() (service.Response, error)
	Status() (service.Response, error)
}

// tunEnv 是 tun 编排家族的可注入环境（selfUpdateEnv 同款形态）：测试构造假编排，
// 绝不触真系统代理/真 launchd/真实提权。
type tunEnv struct {
	// serviceClient 特权服务客户端工厂（timeout 按动作给）。
	serviceClient func(time.Duration) (tunServiceClientT, error)
	// serviceAnchorExists service.json 锚存在性（安装判据/兜底 stop 判据）。
	serviceAnchorExists func() bool
	// helperMatchesCurrent 安装位 helper（锚 helper_path）sha256 == 当前 CLI（R4 陈旧检测）。
	helperMatchesCurrent func() bool
	// coreMatchesCurrent root 内核副本（锚 core_bin）sha256 == 数据目录 bin/mihomo
	// （self-update --core 后陈旧——服务会以旧内核跑新 runtime.yaml，须拦）。
	coreMatchesCurrent func(paths.Home) bool
	// serviceReachable 特权服务可达性探测（3s；tun on 在杀旧 start 之前拦）。
	serviceReachable func() bool
	// waitForServiceReady 提权安装后的安装验证（12s 轮询服务 status；失败带诊断）。
	waitForServiceReady func() error
	// elevate 提权脚本执行缝隙（测试桩——绝不真实提权）。
	elevate func(scriptPath string) error
	// stopStart 杀旧 start（复用 stop 编排语义 20s/StopW）。
	stopStart func(paths.Home) error
	// spawnStart detached spawn `start --no-sysproxy`。
	spawnStart func() error
	// waitServiceCore 单次服务就绪探针（status pid>0 + /version 双确认）。
	waitServiceCore func(context.Context, paths.Home) (int, error)
	// waitUserCore 用户态就绪轮询（回滚重拉/tun off 就绪判定）。
	waitUserCore func(paths.Home, time.Duration) bool
	// ensureGeoip geoip.metadb 前置检查+下载缝隙。
	ensureGeoip func(context.Context, paths.Home) error
	// sysproxyRunner sysproxy 意图恢复 runner（测试注入 fake——绝不触真系统代理）。
	sysproxyRunner sysproxy.Runner
}

// productionTunEnv 组装生产编排环境（coreMatchesCurrent 需要数据目录定位
// bin/mihomo，故带 home）。
func productionTunEnv(home paths.Home) tunEnv {
	return tunEnv{
		serviceClient:        func(timeout time.Duration) (tunServiceClientT, error) { return tunNewClient(timeout) },
		serviceAnchorExists:  serviceConfigInstalled,
		helperMatchesCurrent: helperBinaryMatches,
		coreMatchesCurrent:   coreBinaryMatches,
		serviceReachable:     tunServiceReachable,
		waitForServiceReady:  tunWaitForServiceReady,
		elevate:              runElevatedScript,
		stopStart:            func(home paths.Home) error { return StopW(home, os.Stdout) },
		spawnStart:           spawnTunStartDetached,
		waitServiceCore:      tunProbeServiceCore,
		waitUserCore: func(home paths.Home, timeout time.Duration) bool {
			return waitCoreReady(home, store.ReadSecret(home), timeout)
		},
		ensureGeoip:    ensureGeoipMetadb,
		sysproxyRunner: sysproxy.SystemRunner(),
	}
}
