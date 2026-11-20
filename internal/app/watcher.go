package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/sysproxy"
)

// 代理守护（批④ §2.1，DESIGN #19）：
//   - 挂点照 startScheduler 同款单 goroutine（ctx 取消即退出），tick 周期 30s；
//   - 每 tick LoadSettings——期望端口/bypass 以当前生效值为准（评审 P3）；
//   - 恒开无开关（applied 即守护）——DESIGN enable_proxy_guard 开关有意简化，偏差登记。
const watchInterval = 30 * time.Second

// graceWindow Applied 后 30s 内不判抢改（防自家 Apply 误报）。进程内变量判据：
// postHandshake 写入；CLI 侧 retarget/形态重放在别的进程无法写本变量，
// 由 mutation.lock 互斥覆盖（watcher LOCK_NB 忙跳过即让路）。
const graceWindow = 30 * time.Second

// sysProxyLastApplyUnix 最近一次本进程系统代理写入的 unix 秒（grace 判据）。
var sysProxyLastApplyUnix atomic.Int64

// noteSysProxyApply 记录一次本进程系统代理写入（postHandshake 成功后调用）。
func noteSysProxyApply() { sysProxyLastApplyUnix.Store(time.Now().Unix()) }

// runNotifier osascript 通知缝隙（best-effort 失败仅日志；单测注入桩）。
var runNotifier = func(title, body string) error {
	return syscallExecNotify(title, body)
}

// watcher 是代理守护会话态（runner 注入缝：单测 fake runner 绝不触真系统代理）。
type watcher struct {
	home   paths.Home
	runner sysproxy.Runner
}

// newWatcher 可注入 runner 的构造（单测）。
func newWatcher(home paths.Home, runner sysproxy.Runner) *watcher {
	return &watcher{home: home, runner: runner}
}

// startWatcher 在 start 运行循环内拉起代理守护；wg 由 Run 传入——退出序收敛
// （waitLoop 两调用点先 cancel+Wait 再 fullShutdown，批④评审二轮 P1-2）。
func startWatcher(ctx context.Context, home paths.Home, wg *sync.WaitGroup) {
	w := newWatcher(home, sysproxy.SystemRunner())
	wg.Add(1)
	go func() {
		defer wg.Done()
		w.loop(ctx)
	}()
}

// loop 巡检循环：单 tick 失败仅记日志——守护是后台尽力而为面，绝不因自身错误拖垮 start。
func (w *watcher) loop(ctx context.Context) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := w.tick(); err != nil {
			fmt.Printf("watcher: %v\n", err)
		}
	}
}

// tick 单轮巡检（评审 P1-2 并发防护三件套）：
// ① mutation.lock LOCK_NB 非阻塞获取——忙（别的编排/CLI on-off/关停收敛在途）=
// 本 tick 跳过，无死锁无交错；② 持锁后重读 state：applied==false → 本 tick 中止
// （用户主动 off/裸跑不被打扰）；③ 检测（Form 分流）④ 恢复。检测与恢复全程持锁。
func (w *watcher) tick() error {
	lock, err := TryAcquireMutationLock(w.home)
	if errors.Is(err, ErrMutationBusy) {
		fmt.Println("watcher: another mutation in progress; skipping this tick")
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Release()
	st, err := store.LoadState(w.home)
	if err != nil {
		return err
	}
	if !st.SysProxy.Applied {
		return nil
	}
	// grace：本进程最近写入窗口内不判抢改
	if ts := sysProxyLastApplyUnix.Load(); ts > 0 && time.Since(time.Unix(ts, 0)) < graceWindow {
		return nil
	}
	settings, err := store.LoadSettings(w.home)
	if err != nil {
		return err
	}
	form := st.SysProxy.Form
	if form == "" {
		form = sysproxy.FormManual // legacy 视为 manual
	}
	hijacked, err := w.detect(st, settings, form)
	if err != nil {
		return fmt.Errorf("detect: %w", err)
	}
	if !hijacked {
		return nil
	}
	return w.restore(st, settings, form)
}

// detect 按会话 Form 分流检测期望态（批④ §2.1）：
//   - 手工：备份各服务三项 get == 期望（127.0.0.1:mixed_port + enabled=true）且
//     bypass == 生效 bypass；
//   - PAC：-getautoproxyurl == 期望 URL（生效 gateway_port）且 autoproxy enabled。
//
// 任一不符 = 被抢改。误报面：用户经系统设置手工改代理——这正是要恢复的对象（DESIGN #19）。
func (w *watcher) detect(st store.State, settings store.Settings, form string) (bool, error) {
	wantBypass := sysproxy.EffectiveBypass(settings.BypassDomains)
	for _, b := range st.SysProxy.Services {
		if form == sysproxy.FormPAC {
			out, err := w.runner.Run("-getautoproxyurl", b.Service)
			if err != nil {
				return false, err
			}
			url, enabled, err := sysproxy.ParseAutoProxyState(out)
			if err != nil {
				return false, err
			}
			wantURL := fmt.Sprintf("http://127.0.0.1:%d/pac", settings.GatewayPort)
			if url != wantURL || !enabled {
				return true, nil
			}
			continue
		}
		for _, item := range []struct {
			action string
			want   store.ProxyState
		}{
			{"-getwebproxy", store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: settings.MixedPort}},
			{"-getsecurewebproxy", store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: settings.MixedPort}},
			{"-getsocksfirewallproxy", store.ProxyState{Enabled: true, Server: "127.0.0.1", Port: settings.MixedPort}},
		} {
			out, err := w.runner.Run(item.action, b.Service)
			if err != nil {
				return false, err
			}
			got, err := sysproxy.ParseProxyState(out)
			if err != nil {
				return false, err
			}
			if got != item.want {
				return true, nil
			}
		}
		out, err := w.runner.Run("-getproxybypassdomains", b.Service)
		if err != nil {
			return false, err
		}
		if got := sysproxy.ParseBypassDomains(out); !equalStrings(got, wantBypass) {
			return true, nil
		}
	}
	return false, nil
}

// restore 抢改恢复（批④ §2.1）：按 Form 重放对应 Apply（bypass 用生效值；只触碰
// 备份中存在的服务）→ state 事件计数+时间戳 → 日志 → osascript 通知（best-effort）。
// WHY 不重做备份：恢复只是把被抢改的条目写回期望值，备份仍指向用户原始网络配置，
// 还原语义不变（retarget 同理）。
func (w *watcher) restore(st store.State, settings store.Settings, form string) error {
	svcs := make([]string, 0, len(st.SysProxy.Services))
	for _, b := range st.SysProxy.Services {
		svcs = append(svcs, b.Service)
	}
	bypass := sysproxy.EffectiveBypass(settings.BypassDomains)
	var applyErr error
	if form == sysproxy.FormPAC {
		pacURL := fmt.Sprintf("http://127.0.0.1:%d/pac", settings.GatewayPort)
		applyErr = sysproxy.ApplyPAC(w.runner, svcs, pacURL, bypass)
	} else {
		applyErr = sysproxy.Apply(w.runner, svcs, "127.0.0.1", settings.MixedPort, bypass)
	}
	if applyErr != nil {
		// 恢复失败不记事件（LastRestore 只在成功后写——失败态下轮 tick 重检重试）
		return fmt.Errorf("restore: %w", applyErr)
	}
	st.SysProxy.HijackEvents++
	st.SysProxy.LastRestore = time.Now().Unix()
	if err := store.SaveState(w.home, st); err != nil {
		return err
	}
	fmt.Println("watcher: system proxy hijack detected; restored expected state")
	if err := runNotifier("Tradewind", "系统代理被外部修改，已自动恢复"); err != nil {
		fmt.Printf("watcher: notify: %v\n", err) // best-effort：state.json 事件为权威面
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// syscallExecNotify 经 osascript 发 macOS 通知（best-effort；runNotifier 注入缝
// 的生产实现——单测替换 runNotifier，不触达真实通知）。
func syscallExecNotify(title, body string) error {
	script := fmt.Sprintf("display notification %s with title %s", quoteAS(body), quoteAS(title))
	return exec.Command("osascript", "-e", script).Run()
}

func quoteAS(s string) string {
	// AppleScript 字符串字面量：反斜杠与双引号转义（正文为固定文案，防御性处理）
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' {
			out = append(out, '\\')
		}
		out = append(out, c)
	}
	return string(append(out, '"'))
}
