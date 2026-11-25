// sockpath.go：内核 API socket 路径按会话形态分流（批⑥b §4.3，F22）。
//
// 特权服务（root）托管的 TUN 内核，其原生 API socket 在 root 独占目录里
// （<core_home>/core.sock，0700 + 0600），用户域连不上；服务在 <anchor>/core.sock
// 开白名单代理，作为用户域的唯一入口。两种判据的分工（WHY 两套）：
//   - start 进程内部的两处（面板网关、启动后的版本读取）按 **settings** 取形态
//     （startFormSockPath）——面板网关在 postHandshake 写状态**之前**就起来了，
//     照 state 取会拿到上一会话的形态，整段会话面板都拨错地方；
//   - CLI 各命令按 **state** 取（coreSockPath = formSockPath(state.Core.Tun)）。
//     tun off 的幂等探针例外：settings 说关而核还在跑的漂移态，两种形态都可能是
//     残留，任一可达都要落入收敛（tun.go）。
package app

import (
	"context"
	"os"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// tunBrokerSockPath 是 TUN 形态的内核 API 入口——特权服务的白名单代理
// （<anchor>/core.sock，批⑥b §4.3）。包级缝隙：测试指向 /tmp 短路径桩。
var tunBrokerSockPath = service.DefaultBrokerSock

// formSockPath 按形态给出内核 API 的 socket 路径：TUN → 代理入口；否则用户态内核
// 的 <data_dir>/core.sock。
func formSockPath(tun bool, home paths.Home) string {
	if tun {
		return tunBrokerSockPath
	}
	return home.SockFile()
}

// startFormSockPath start 进程内部按 settings 取内核 API 路径（面板网关与启动后的
// 版本读取共用一份判定）。WHY 看 settings 不看 state：TUN 会话的 start 被 kill -9
// 后 state.Core.Tun 残留 true，紧接着 tun off + 新的用户态会话若照 state 取，网关
// 与版本读取会拨向已被摘除的代理 socket。
func startFormSockPath(home paths.Home, settings store.Settings) string {
	return formSockPath(settings.TunEnabled, home)
}

// settingsTunEnabled 读 settings.tun_enabled（读失败按 false——形态判定不许因读盘
// 失败而偏向 TUN）。
func settingsTunEnabled(home paths.Home) bool {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return false
	}
	return settings.TunEnabled
}

// coreInTunForm 读 state.Core.Tun（读失败按 false）。CLI 侧的会话形态判据。
func coreInTunForm(home paths.Home) bool {
	st, err := store.LoadState(home)
	if err != nil {
		return false
	}
	return st.Core.Tun
}

// coreSockPath CLI 侧按当前会话形态选路径（判定依据 state.Core.Tun）。
func coreSockPath(home paths.Home) string {
	return formSockPath(coreInTunForm(home), home)
}

// waitCoreReadyForForm self-update 拉回内核后的就绪轮询（waitReady 缝隙的生产实现）：
// 按 settings 取形态。WHY 看 settings 不看 state：到这一步 StopW 已经 markStopped，
// state 里的形态已清零，照 state 会拨错路径。
func waitCoreReadyForForm(home paths.Home, secret string, timeout time.Duration) bool {
	return waitCoreReady(formSockPath(settingsTunEnabled(home), home), secret, timeout)
}

// coreReachableAt 探测指定 socket 上的内核是否在跑：文件存在且 /version 可达。只读。
// TUN 形态下这是服务的代理入口——代理在内核已停时回 502，非 2xx 同样算不可达
// （core 客户端把非 2xx 折成 *core.StatusError）。
func coreReachableAt(sockPath, secret string) bool {
	if _, err := os.Stat(sockPath); err != nil {
		return false
	}
	client := core.NewClient(sockPath, secret)
	ctx, cancel := context.WithTimeout(context.Background(), reloadProbeTimeout)
	defer cancel()
	_, err := client.Version(ctx)
	return err == nil
}
