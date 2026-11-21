package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/synth"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// Synthesize 合成 runtime.yaml（模板 embed + 订阅 current + 增强链 + settings + secret 注入）。
// 增强链槽文件缺失=空转（零开销）；未命中/撞名告警经 stderr 输出（不进 runtime，
// batch3 §3.2）。synth.Synth 保持纯函数——文件读取与告警打印都收口在本编排。
func Synthesize(home paths.Home, secret string) error {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return err
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		return err
	}
	// 无 current 订阅时合成纯模板（裸内核也可启动，便于先 start 后 add）
	var sub []byte
	if item, ok := idx.CurrentItem(); ok {
		sub, err = os.ReadFile(home.ProfileFile(item.File))
		if err != nil {
			return fmt.Errorf("app: read current profile %s: %w", item.File, err)
		}
	}
	enh, err := loadEnhanceLayers(home, idx.Current)
	if err != nil {
		return err
	}
	var warnings []string
	out, err := synth.Synth(sub, synth.Injection{Settings: settings, Secret: secret, Warnings: &warnings}, enh)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	return store.WriteFileRaw(home.RuntimeFile(), out)
}

// startCore 合成 + spawn + 握手（M0-plan §5.1 步骤 4-6）。
func startCore(home paths.Home, secret string) (*core.Manager, error) {
	if err := Synthesize(home, secret); err != nil {
		return nil, err
	}
	mgr := core.NewManager(core.Config{
		BinPath:     home.Bin(),
		SidecarPath: home.Sidecar(),
		DataDir:     home.String(),
		RuntimePath: home.RuntimeFile(),
		SockPath:    home.SockFile(),
		Secret:      secret,
		LogPath:     home.LogFile(),
	})
	if err := mgr.Spawn(); err != nil {
		return nil, err
	}
	fmt.Printf("core spawned (pid %d), handshaking...\n", mgr.PID())
	if err := mgr.WaitReady(context.Background()); err != nil {
		_ = mgr.Stop() // 握手失败也必须回收进程并清理 sock
		return nil, err
	}
	return mgr, nil
}

// fullShutdown 完整还原流程（M0-plan §5.1 步骤 10）：
// 停机快照回写 → 还原系统代理 → 内核 SIGTERM（5s 后 SIGKILL）→ 移除 core.sock → desired=stopped。
// generation 归属校验（M2-plan §2.3）：state 中代际与本实例不符 → 新实例已接管，
// 本进程只停自己的内核子进程，绝不覆写他人 state / 还原他人的系统代理。
// myGen=0 表示本进程尚未写入代际（start 早期失败路径，持 pidfile 锁期间无接管可能）→ 照常还原。
// flock 释放由调用方 defer 完成。任何一步失败都不阻断后续步骤（合并报错）。
func fullShutdown(home paths.Home, mgr *core.Manager, myGen int) error {
	if myGen > 0 && !generationIsMine(home, myGen) {
		fmt.Printf("shutdown: state taken over by newer instance (generation %d); skip restore, stop own core only\n",
			stGeneration(home))
		if mgr != nil {
			return mgr.Stop()
		}
		return nil
	}
	var errs []error
	// 触发二（M1-plan §4.3）：内核仍在跑时快照当前选择；必须先于 mgr.Stop()，
	// 失败不阻断还原流程（内核不可达在 snapshot 内部按跳过语义处理）
	if err := snapshotBeforeStop(home); err != nil {
		errs = append(errs, err)
	}
	if err := restoreSysProxyIfApplied(home); err != nil {
		errs = append(errs, err)
	}
	if mgr != nil {
		if err := mgr.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	if err := markStopped(home); err != nil {
		errs = append(errs, err)
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("app: shutdown: %w", err)
	}
	return nil
}

// generationIsMine 校验 state 归属：generation 与自身一致（或 state 无代际记录）→ 归我。
func generationIsMine(home paths.Home, myGen int) bool {
	return stGeneration(home) == myGen
}

// stGeneration 读取当前 state 代际（读失败按 0 处理，走兼容路径）。
func stGeneration(home paths.Home) int {
	st, err := store.LoadState(home)
	if err != nil {
		return 0
	}
	return st.Core.Generation
}

// restoreSysProxyIfApplied 按 state.json 备份精确还原系统代理（applied=false 即无操作）。
// autoproxy 还原段按备份记录的会话 Form 分流（Restore 决策表，批④ §3.3）。
func restoreSysProxyIfApplied(home paths.Home) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if !st.SysProxy.Applied {
		return nil
	}
	fmt.Println("restoring system proxy from backup...")
	if err := sysproxy.Restore(sysproxy.SystemRunner(), st.SysProxy.Services, st.SysProxy.Form); err != nil {
		return fmt.Errorf("app: restore sysproxy: %w", err)
	}
	fmt.Println("system proxy restored")
	return nil
}

// markStopped 写回 desired=stopped、applied=false（保留 generation——代际单调递增不重置）。
func markStopped(home paths.Home) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	st.Core = store.CoreState{Desired: store.DesiredStopped, PID: 0, Generation: st.Core.Generation}
	st.SysProxy.Applied = false
	return store.SaveState(home, st)
}

// cleanupResidues 启动期双残留清理（M0-plan §5.1 步骤 2，防 kill -9 遗患）：
// a. 系统代理 applied==true → 按备份精确还原；
// b. 孤儿内核 pid 存活且身份匹配 → 回收（TUN 形态分流服务请求，批⑥ §2.1 分流 3
//
//	——root pid 用户态 SIGTERM=EPERM（F21），不分流则 start 崩溃重拉死循环）。
//
// 注：残留 core.sock 无需处理——mihomo 在 listen 前 unlink 自清理。
func cleanupResidues(home paths.Home) error {
	st, err := store.LoadState(home)
	if err != nil {
		return err
	}
	if st.SysProxy.Applied {
		fmt.Println("cleanup: restoring system proxy left by previous run")
		if err := restoreSysProxyIfApplied(home); err != nil {
			return err
		}
		st.SysProxy.Applied = false
	}
	if st.Core.PID > 0 && processAlive(st.Core.PID) && pidCommandIsCore(st.Core.PID, home) {
		if st.Core.Tun {
			// TUN 形态：服务 stop 请求收敛（锚缺失 = 角态报错指引，不进 EPERM kill）
			if err := cleanupTunOrphan(st.Core.PID); err != nil {
				return err
			}
			st.Core.PID = 0
		} else {
			fmt.Printf("cleanup: killing orphan core (pid %d)\n", st.Core.PID)
			if err := killProcessGracefully(st.Core.PID); err != nil {
				return err
			}
			st.Core.PID = 0
		}
	}
	if st.Core.Desired != store.DesiredStopped {
		st.Core.Desired = store.DesiredStopped
	}
	return store.SaveState(home, st)
}

// applySysProxy start 路径的系统代理写入（真实 runner；form 按 settings 推导）。
// 实现已抽到 app/sysproxy.go 的 applySysProxyRunner（sysproxy on 与 start 共用一份语义）。
func applySysProxy(home paths.Home, port int, form string) ([]store.ServiceBackup, error) {
	return applySysProxyRunner(home, port, sysproxy.SystemRunner(), form)
}
