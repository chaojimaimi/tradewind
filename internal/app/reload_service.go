// reload_service.go：TUN 形态的热重载走服务（批⑥b §4.4 用户域改动，F8/F9）。
//
// 特权服务托管的内核，用户域连不上原生 socket——热重载改走服务的 reload 动作：
// 服务端读用户 runtime.yaml 并归一化，内核接受才落盘。处置分流：
//   - 归一化拒绝 / 内核 400（rejected）→ 显式报错，**不降级重启**——同一份配置
//     重启也会被拒，重启只会把内核弄停；
//   - 其它 !OK（服务侧问题）→ 显式报错，同样不降级；
//   - 传输层失败（拨号/超时）→ 落回既有降级重启（degradeRestartW）。
package app

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// tunReloadRequestTimeout 走 service.ReloadTimeout（60s 档：排队等一次 start 握手
// + 推送 + 归一化与余量）。
const tunReloadRequestTimeout = service.ReloadTimeout

// reloadViaService reloadIfRunning 的 TUN 分流：合成 → 服务 reload。信号量与代理
// 不碰——progress 是注入的 writer（--json 调用方传 stderr，stdout 必须保持单行）。
func reloadViaService(ctx context.Context, home paths.Home, progress io.Writer, synthesize bool) error {
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	// 未运行：仅落盘，与原路径同语义（服务端对未托管内核同样仅落盘）
	if !coreReachableAt(formSockPath(true, home), secret) {
		return nil
	}
	if synthesize {
		// 写用户 runtime.yaml——服务端 reload 会读它并归一化
		if err := Synthesize(home, secret); err != nil {
			return err
		}
	}
	// 到不了"未安装"分支——分流条件已要求锚存在
	cl, err := tunNewClient(tunReloadRequestTimeout)
	if err != nil {
		return err
	}
	resp, err := cl.Reload()
	if err != nil {
		// 传输层失败（拨号/超时）→ 既有降级：服务 stop + start（generation+1）
		fmt.Fprintf(progress, "service reload failed (%v); degrading to core restart\n", err)
		return degradeRestartW(home, secret, progress)
	}
	if resp.Rejected {
		return fmt.Errorf("app: 配置被 TUN 模式拒绝，内核仍在运行上一份有效配置：%s", resp.Error)
	}
	if !resp.OK {
		return fmt.Errorf("app: tun service reload failed: %s", resp.Error)
	}
	if len(resp.Ignored) > 0 {
		fmt.Fprintf(progress, "warning: 特权服务忽略了 %d 个配置键：%s\n",
			len(resp.Ignored), strings.Join(resp.Ignored, ", "))
	}
	fmt.Fprintln(progress, "core hot-reloaded")
	return nil
}
