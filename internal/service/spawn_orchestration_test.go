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
	// staged 现在是归一化产物（I3，c3 形态）：== 归一化器的产物；且不用纯同义反复
	// 的断言——产物以用户文件开头、恰有一个顶层 tun: 键、external-controller 钉死 ""。
	userBytes, err := os.ReadFile(conf.UserRuntime)
	if err != nil {
		t.Fatal(err)
	}
	product, rej := NormalizeRuntime(userBytes)
	if rej != nil {
		t.Fatalf("normalize fixture rejected at %s: %s", rej.Key, rej.Reason)
	}
	if string(staged) != string(product.Bytes) {
		t.Fatalf("staged runtime = %q, want the normalized product %q", staged, product.Bytes)
	}
	if !bytes.HasPrefix(staged, []byte("proxies: []\n")) {
		t.Fatalf("staged runtime %q must start with the user's own proxies key", staged)
	}
	if n := countTopLevelKeys(staged, "tun:"); n != 1 {
		t.Fatalf("top-level tun: keys = %d, want exactly 1 (pinned block)", n)
	}
	if n := countTopLevelKeys(staged, "external-controller:"); n != 1 {
		t.Fatalf("top-level external-controller: keys = %d, want exactly 1", n)
	}
	if !bytes.Contains(staged, []byte("external-controller: \"\"\n")) {
		t.Fatalf("staged runtime %q must pin external-controller to the empty string", staged)
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

// countTopLevelKeys 统计顶格出现的行首键（顶层键不缩进；tun 块内层键带缩进，
// 不会被计入）。
func countTopLevelKeys(data []byte, key string) int {
	n := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, key) {
			n++
		}
	}
	return n
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

// TestStartRefusedWhenUserRuntimeMissing 用户 runtime.yaml 缺失 → start 被拒（服务
// 侧读取失败，非配置拒绝：rejected=false）、内核未 spawn（绝不让内核以默认配置跑
// 起来，M17）。
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
	if resp.OK || resp.Rejected || !strings.Contains(resp.Error, "runtime") {
		t.Fatalf("Start without user runtime = %+v, want a non-rejected refusal naming the runtime read", resp)
	}
	if st, _ := cl.Status(); st.PID != 0 {
		t.Fatalf("no core may spawn without a staged runtime (status pid %d)", st.PID)
	}
	if _, err := os.Stat(conf.KernelSock); !os.IsNotExist(err) {
		t.Fatalf("kernel sock must not exist (err = %v)", err)
	}
}

// TestStartRefusedWhenUserRuntimeSymlink 用户 runtime.yaml 是符号链接 → 拒绝（服务
// 侧读取失败，rejected=false；readSafe 的 O_NOFOLLOW 闸在编排层的呈现）。
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
	if resp.OK || resp.Rejected || !strings.Contains(resp.Error, "symbolic link") {
		t.Fatalf("Start with a symlinked runtime = %+v, want a non-rejected refusal", resp)
	}
	if st, _ := cl.Status(); st.PID != 0 {
		t.Fatalf("no core may spawn behind a symlinked runtime (status pid %d)", st.PID)
	}
}
