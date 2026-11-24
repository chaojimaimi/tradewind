package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// spawn 编排（批⑥b §4.1/§4.4）：内核 argv/环境、staged runtime、原生 sock 收敛、
// I1 行为级断言、源码静态断言、自愈 respawn 的 pid 记录与日志轮转、start 拒绝路径。

// TestServiceKernelArgsEnvAndStagedRuntime start 后：内核 argv 为 -d <core_home>
// -f <core_home>/runtime.yaml -ext-ctl-unix <core_home>/core.sock；进程环境恰好是
// 生产的三个固定变量；<core_home>/runtime.yaml 内容 == 用户 runtime.yaml；握手成功
// 后内核原生 sock 权限为 0600（fakecore 对齐 mihomo 先自 chmod 666，WaitReady 压回）。
// WHY 需要 coreEnv 缝：生产只传三个变量，桩内核的 dump 模式靠环境变量读取——缝
// 只在测试里追加变量，生产路径绝不拼接 os.Environ()。
func TestServiceKernelArgsEnvAndStagedRuntime(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	dump := filepath.Join(dir, "kernel-args.txt")
	startStubServerTuned(t, cfg, func(s *Server, _ *trustRecorder) {
		s.coreEnv = func(c ConfigFile) []string {
			return append(coreEnvProduction(c), "FAKE_CORE_MODE=dump-args", "FAKE_CORE_DUMP="+dump)
		}
	})
	cl := NewClient(cfg.SockPath, 30*time.Second)
	resp, err := cl.Start(1)
	if err != nil || !resp.OK || resp.PID <= 0 {
		t.Fatalf("Start = %+v err = %v, want ok", resp, err)
	}
	waitForFile(t, dump, 5*time.Second)
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	argv, env := parseDump(t, string(data))
	wantArgs := []string{fakeCoreBin, "-d", conf.CoreHome, "-f", RuntimeIn(conf.CoreHome), "-ext-ctl-unix", conf.KernelSock}
	if len(argv) < len(wantArgs) || !equalSlices(argv[:len(wantArgs)], wantArgs) {
		t.Fatalf("kernel argv = %v, want prefix %v", argv, wantArgs)
	}
	wantEnv := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + conf.CoreHome,
		"TMPDIR=" + filepath.Join(conf.CoreHome, "tmp"),
	}
	// 生产三变量必须原样在最前；本用例经 coreEnv 缝追加了桩内核的两个测试变量
	//（生产路径只有前三项）。总长度受限 = 没有任何继承自父进程的变量。
	if len(env) < len(wantEnv) || !equalSlices(env[:len(wantEnv)], wantEnv) {
		t.Fatalf("kernel env prefix = %v, want %v", env, wantEnv)
	}
	if len(env) != len(wantEnv)+2 {
		t.Fatalf("kernel env = %v, want %d entries (3 production + 2 test-mode) — inherited vars leak", env, len(wantEnv)+2)
	}
	staged, err := os.ReadFile(RuntimeIn(conf.CoreHome))
	if err != nil {
		t.Fatalf("read staged runtime: %v", err)
	}
	if string(staged) != "proxies: []\n" {
		t.Fatalf("staged runtime = %q, want the user file's bytes", staged)
	}
	if info, err := os.Stat(conf.KernelSock); err != nil {
		t.Fatalf("stat kernel sock: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("kernel sock perm = %o, want 0600 (fakecore self-chmod 666 must be reined back)", info.Mode().Perm())
	}
}

// TestCoreEnvProductionExactlyThreeVars 生产默认环境恰好三个固定变量（§4.1：环境
// 必须由服务钉死而不是"碰巧干净"——SKIP_SAFE_PATH_CHECK 一旦经 launchctl setenv
// 带入即全线失守，M8；绝不拼接 os.Environ()）。
func TestCoreEnvProductionExactlyThreeVars(t *testing.T) {
	c := ConfigFile{CoreHome: "/x/service-data"}
	got := coreEnvProduction(c)
	want := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=/x/service-data",
		"TMPDIR=/x/service-data/tmp",
	}
	if !equalSlices(got, want) {
		t.Fatalf("coreEnvProduction = %v, want exactly %v", got, want)
	}
}

// parseDump 解析 fakecore dump-args 的输出（argv:/env: 两段，行首两空格缩进）。
func parseDump(t *testing.T, data string) (argv, env []string) {
	t.Helper()
	section := ""
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		switch line {
		case "argv:", "env:":
			section = line
			continue
		case "":
			continue
		}
		switch section {
		case "argv:":
			argv = append(argv, line)
		case "env:":
			env = append(env, line)
		}
	}
	return argv, env
}

func equalSlices(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// dirSnapshot 是目录快照的单条目投影：类型/属主/权限/大小 + inode/mtime（I1 的
// 行为级断言面）。inode 与 mtime 一并记下：原地覆写会动 mtime，"写临时文件再
// rename"会换 inode——只有"条目集合与属主/权限"的话这两种写入都看不出来。
type dirSnapshot struct {
	mode  os.FileMode
	uid   int
	size  int64
	ino   uint64
	mtime int64
}

// snapshotDataDir 对目录做全量快照（路径 → 投影）。
func snapshotDataDir(t *testing.T, dir string) map[string]dirSnapshot {
	t.Helper()
	out := map[string]dirSnapshot{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		uid, ino := -1, uint64(0)
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, ino = int(sys.Uid), sys.Ino
		}
		out[path] = dirSnapshot{
			mode: info.Mode(), uid: uid, size: info.Size(),
			ino: ino, mtime: info.ModTime().UnixNano(),
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestI1DataDirUntouchedByFullFlow I1 行为级断言：全流程（start → stop）前后对
// DataDir 各做一次目录快照（条目名、类型、属主、权限、大小、inode、mtime），逐项
// 相同——root 对用户数据目录零写入（没有新增、没有删除、没有属主/权限变化、没有
// 内容被重写）。数据目录里放一份 geo 文件，让 geo 同步的读取路径也落在断言里。
func TestI1DataDirUntouchedByFullFlow(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	if err := os.WriteFile(filepath.Join(conf.DataDir, "geoip.metadb"), []byte("geo-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	before := snapshotDataDir(t, conf.DataDir)
	if _, err := cl.Start(1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := cl.Status(); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	after := snapshotDataDir(t, conf.DataDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("user data dir changed across the full flow:\nbefore=%+v\nafter =%+v", before, after)
	}
}

// TestSourceStaticChownChmodInvariant 源码静态断言（I1 的可检查形态）：
// internal/service 与 internal/core 的非测试 .go 里 `os.Chown(` 出现 0 次；
// `os.Chmod(` 恰好两处——core/manager.go（WaitReady 握手后压内核原生 sock）与
// service/server.go（服务 socket 的 listen）。c4 会在代理入口新增第三处（届时本
// 用例的期望集合同步追加 broker 监听文件）。
func TestSourceStaticChownChmodInvariant(t *testing.T) {
	type hit struct{ file string }
	var chowns []hit
	chmods := map[string]bool{}
	for _, root := range []string{".", filepath.Join("..", "core")} {
		err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// 根目录本身（名字可能是 "."）不跳过；其下的 testdata/隐藏目录跳过
				if path != root && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			src := string(data)
			if strings.Contains(src, "os.Chown(") {
				chowns = append(chowns, hit{file: path})
			}
			if strings.Contains(src, "os.Chmod(") {
				chmods[path] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(chowns) != 0 {
		t.Fatalf("os.Chown( must not appear in service/core sources, found in %v", chowns)
	}
	want := map[string]bool{
		filepath.Join("server.go"):                true, // 服务 socket listen（/var/run 下按路径 chmod 0666）
		filepath.Join("..", "core", "manager.go"): true, // WaitReady 握手后收敛内核原生 sock
	}
	if !reflect.DeepEqual(chmods, want) {
		t.Fatalf("os.Chmod( sites = %v, want exactly %v", chmods, want)
	}
}

// TestMonitorRespawnRewritesCorePID (k)：start 后 kill 内核触发 Monitor 自愈
// respawn → OnSpawned 重写 core.pid，内容等于新内核 pid（批⑥b F20：自愈路径不经
// spawnCore，回调保证自愈出来的内核记录在案，否则服务重启时收编不到）。
func TestMonitorRespawnRewritesCorePID(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	first, err := cl.Start(1)
	if err != nil || !first.OK || first.PID <= 0 {
		t.Fatalf("initial Start = %+v err = %v", first, err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stub core: %v", err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(cfg.PIDPath); err == nil {
			var pid int
			if _, perr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); perr == nil &&
				pid > 0 && pid != first.PID && processAlive(pid) && coreProcessMatches(pid, conf.CoreHome) &&
				probeAlive(conf.KernelSock) {
				return // 自愈 respawn 完成且 core.pid 指向新内核
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("core.pid was not rewritten to the respawned kernel's pid within 12s")
}

// TestMonitorRespawnRotatesAndOwnsCoreLog (l)：预置超过 5 MiB 的 core.log（当前
// 用户属主、0644）→ start 后往日志里继续堆到超限并放宽权限 → kill 触发自愈
// respawn（不经 spawnCore 的那条路径）→ 发生轮转（出现 core.log.1），且新 core.log
// 经 LogOwner 收敛为 dataUID 属主、0600。
func TestMonitorRespawnRotatesAndOwnsCoreLog(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	if err := os.MkdirAll(filepath.Dir(conf.CoreLog), 0o755); err != nil {
		t.Fatal(err)
	}
	big := bytes.Repeat([]byte("x"), (5<<20)+1024)
	if err := os.WriteFile(conf.CoreLog, big, 0o644); err != nil {
		t.Fatal(err)
	}
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	first, err := cl.Start(1)
	if err != nil || !first.OK || first.PID <= 0 {
		t.Fatalf("initial Start = %+v err = %v", first, err)
	}
	// 首次 spawn 已把预置日志轮转掉；再造一份超限日志 + 权限漂移，让轮转与 LogOwner
	// 收敛都落在自愈 respawn 那次 Spawn 里
	if err := os.WriteFile(conf.CoreLog, big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(first.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("kill stub core: %v", err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		rotated, rerr := os.Stat(conf.CoreLog + ".1")
		_, ferr := os.Stat(conf.CoreLog)
		if rerr == nil && rotated.Size() > 5<<20 && ferr == nil {
			uid, perm := fileOwner(t, conf.CoreLog)
			if perm == 0o600 && uid == conf.DataUID {
				return
			}
			t.Fatalf("fresh core.log uid=%d perm=%o, want dataUID %d 0600", uid, perm, conf.DataUID)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("log rotation + ownership convergence did not happen during the self-heal respawn within 12s")
}

// TestStartRefusedWhenUserRuntimeMissing 用户 runtime.yaml 缺失 → stageRuntime
// 失败 → start 被拒、内核未 spawn（绝不让内核以默认配置跑起来，M17）。
func TestStartRefusedWhenUserRuntimeMissing(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	if err := os.Remove(conf.UserRuntime); err != nil {
		t.Fatal(err)
	}
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	resp, err := cl.Start(1)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "refusing to spawn, runtime") {
		t.Fatalf("Start without user runtime = %+v, want refusal", resp)
	}
	if st, _ := cl.Status(); st.PID != 0 {
		t.Fatalf("no core may spawn without a staged runtime (status pid %d)", st.PID)
	}
	if _, err := os.Stat(conf.KernelSock); !os.IsNotExist(err) {
		t.Fatalf("kernel sock must not exist (err = %v)", err)
	}
}

// TestStartRefusedWhenUserRuntimeSymlink 用户 runtime.yaml 是符号链接 → 拒绝
// （readSafe 的 O_NOFOLLOW 闸在编排层的呈现）。
func TestStartRefusedWhenUserRuntimeSymlink(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	target := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("proxies: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(conf.UserRuntime); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, conf.UserRuntime); err != nil {
		t.Fatal(err)
	}
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	resp, err := cl.Start(1)
	if err != nil {
		t.Fatalf("Start transport: %v", err)
	}
	if resp.OK || !strings.Contains(resp.Error, "symbolic link") {
		t.Fatalf("Start with a symlinked runtime = %+v, want refusal", resp)
	}
	if st, _ := cl.Status(); st.PID != 0 {
		t.Fatalf("no core may spawn behind a symlinked runtime (status pid %d)", st.PID)
	}
}
