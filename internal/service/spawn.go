// spawn.go：内核 spawn 编排（批⑥b §4.1/§4.4）——从 server.go 拆出（单文件 ≤400 行）。
// 每次 spawn 的前置序：信任链复核 → restoreRuntime（L5：快照与 R 核对/回写，经
// PreSpawn 钩子）→ syncGeo → 建 tmp 目录 → spawn → 握手。归一化与落盘在 handleStart
// （L3）已完成，本文件不再搬运配置字节。Monitor 崩溃自愈的内部 respawn 在 Manager
// 内发生、不经 spawnCore，内核专属的闸（PreSpawn 钩子、LogOwner、OnSpawned）都挂
// 在 core.Config 上覆盖两条路径。
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/core"
)

// coreEnvProduction 生产内核环境：恰好三个变量，不继承 launchd 环境（批⑥b §4.1）。
// WHY：SKIP_SAFE_PATH_CHECK/SAFE_PATHS 一旦被 launchctl setenv 带入，内核的全部
// 路径约束失效（M8）——环境必须由服务钉死而不是"碰巧干净"。
func coreEnvProduction(c ConfigFile) []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + c.CoreHome,
		"TMPDIR=" + filepath.Join(c.CoreHome, "tmp"),
	}
}

// spawnCore 以 root 身份 spawn + 同步握手 + 启动 Monitor 自愈。调用方须持 s.mu——
// 唯一的例外是启动期的 adoptOrphans：它先于 listen/acceptLoop 执行，此时还没有
// 任何请求处理协程，是唯一会碰 s.mu 所保护字段的执行流，无需持锁。
// pid 记录由 Manager 的 OnSpawned 回调负责（进程一启动就写——覆盖自愈 respawn，
// F20）。进入本函数前调用方已把归一化产物提交（promote 快照 + 落盘 R，L3/L2）；
// 每次 spawn 前复核执行目录与内核工作目录两道信任链：不达标即拒绝并返回明确错误。
func (s *Server) spawnCore() error {
	if err := s.verifyExecTrust(s.conf); err != nil {
		return fmt.Errorf("service: refusing to spawn, exec dir failed the ownership re-check: %w", err)
	}
	if err := s.verifyHomeTrust(s.conf); err != nil {
		return fmt.Errorf("service: refusing to spawn, core home failed the trust re-check: %w", err)
	}
	// geo 同步（§4.4）：内部已 best-effort，失败不阻断
	syncGeo(s.conf)
	// 内核显式 TMPDIR 指到 <core_home>/tmp（root 0700）——杜绝内核版本日后把 root
	// 临时文件落进共享的 /tmp
	if err := os.MkdirAll(filepath.Join(s.conf.CoreHome, "tmp"), 0o700); err != nil {
		return fmt.Errorf("service: refusing to spawn, mkdir tmp: %w", err)
	}
	uid := s.conf.DataUID // LogOwner 取副本——Manager 只读该值
	mgr := core.NewManager(core.Config{
		BinPath:     s.conf.CoreBin,
		SidecarPath: s.conf.Sidecar,
		DataDir:     s.conf.CoreHome,            // 内核工作目录 = root 属主 <core_home>
		RuntimePath: RuntimeIn(s.conf.CoreHome), // -f = 服务搬运的产物
		SockPath:    s.conf.KernelSock,          // 内核原生 API（仅 root 服务可达）
		LogPath:     s.conf.CoreLog,             // <anchor>/logs/core.log
		// 批⑥b 起不传 SockPerm：沿用 DefaultSockPerm 0600——用户域只经代理访问内核
		// API，目录 0700 + 文件 0600 两道屏障
		LogOwner: &uid, // 新 core.log 经 fd 收敛为 data_uid 0600（含自愈轮转路径，F21）
		// 每次 spawn 都重写 core.pid（OnSpawned 在 cmd.Start 成功后回调）——自愈
		// respawn 不经过本函数，回调保证自愈出来的内核也记录在案（F20）
		OnSpawned: func(pid int) { s.writeCorePID(pid) },
		Env:       s.coreEnv(s.conf),
		// Monitor 崩溃自愈的 respawn 在 Manager 内部发生，不经 spawnCore——两道信任
		// 链复核 + L5 restoreRuntime（快照为 nil 拒绝 spawn；R 与快照漂移即按快照
		// 回写）经同一份钩子闭包两条路径都过（I3 的最后一道闸）。
		PreSpawn: s.preSpawnHook(),
	})
	if err := mgr.Spawn(); err != nil {
		return fmt.Errorf("service: spawn core: %w", err)
	}
	hsCtx, cancel := context.WithTimeout(s.rootCtx, handshakeWindow)
	defer cancel()
	if err := mgr.WaitReady(hsCtx); err != nil {
		_ = mgr.Stop()
		// OnSpawned 已在进程启动时写过 core.pid；握手失败后不留死 pid——否则下次
		// 服务重启的 adoptOrphans 会拿它当收编判据
		_ = os.Remove(s.cfg.PIDPath)
		return fmt.Errorf("service: core handshake: %w", err)
	}
	s.mgr = mgr
	s.gaveUp = false
	go s.monitorLoop(mgr)
	fmt.Printf("service: core spawned (pid %d)\n", mgr.PID())
	return nil
}

// monitorLoop 崩溃自愈（Manager Monitor 复用，退避 1s/2s/4s）：预算耗尽 → gaveUp
// 置位（新 start 请求可重新准入）+ pid 记录清理。
func (s *Server) monitorLoop(mgr *core.Manager) {
	err := mgr.Monitor(s.rootCtx, func(giveUp error) {
		s.mu.Lock()
		s.gaveUp = true
		s.mu.Unlock()
		_ = os.Remove(s.cfg.PIDPath)
		fmt.Fprintf(os.Stderr, "service: core restart budget exhausted: %v\n", giveUp)
	})
	if err != nil {
		return // giveUp 回调已处理；Monitor 返回值仅诊断
	}
}

// stopCoreLocked 停核 + pid 记录清理（调用方须持 s.mu；未托管时 no-op）。
// 批⑥b I1：数据目录里已没有 root 产物，属主回收随之消失（R10 关闭）。
func (s *Server) stopCoreLocked() error {
	if s.mgr == nil {
		return nil
	}
	mgr := s.mgr
	s.mgr = nil
	if err := mgr.Stop(); err != nil {
		return fmt.Errorf("service: stop core: %w", err)
	}
	_ = os.Remove(s.cfg.PIDPath)
	fmt.Println("service: core stopped")
	return nil
}

// stopCore 并发安全停核（Run 收敛路径用）。
func (s *Server) stopCore() {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.stopCoreLocked()
}

// adoptOrphans 服务自身重启的孤儿收编（批⑥ §3.1/R13；L2——批⑥b §4.4）：launchd
// 拉起时旧子进程 mihomo 仍存活（父死后被 launchd 收养，cmd.Wait 不可用）——读
// core.pid：pid 存活且身份匹配（命令行含 -d <core_home>）→ root SIGTERM 回收。
// 孤儿一律回收；是否重拉取决于 L1 种子：snapshot != nil → 按快照重拉（"恢复同一
// 会话"，代价=健康 core 全断重启一次，R13 登记）；snapshot == nil → 不重拉，服务
// 继续监听——等用户域下一次 start（L3）重新归一化（绝不让内核以默认配置跑起来）。
func (s *Server) adoptOrphans() error {
	data, err := os.ReadFile(s.cfg.PIDPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("service: read core.pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(s.cfg.PIDPath) // 损坏记录清零（保守收敛）
		return nil
	}
	_ = os.Remove(s.cfg.PIDPath)
	if !processAlive(pid) || !coreProcessMatches(pid, s.conf.CoreHome) {
		return nil // 已死或身份不符（pid 复用防护）：无可收编
	}
	fmt.Printf("service: reclaiming orphan core (pid %d)\n", pid)
	if err := killProcessGracefully(pid); err != nil {
		return fmt.Errorf("service: reclaim orphan pid %d: %w", pid, err)
	}
	if s.snapshotCopy() == nil {
		fmt.Println("service: orphan reclaimed; no normalized runtime snapshot on record, not respawning (waiting for the next start)")
		return nil
	}
	if err := s.spawnCore(); err != nil {
		return fmt.Errorf("service: adopt: %w", err)
	}
	return nil
}

// writeCorePID 写 root core pid 记录（孤儿收编判据；root-owned 目录内 0644）。
// 由 core.Config.OnSpawned 在每次内核子进程启动成功后回调（含 Monitor 自愈的
// respawn——那条路径不经过 spawnCore，F20）。
func (s *Server) writeCorePID(pid int) {
	_ = os.WriteFile(s.cfg.PIDPath, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}
