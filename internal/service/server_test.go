package service

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
)

// fakeCoreBin 是 TestMain 预编译的桩内核（复用 core 包 testdata/fakecore——行为语义
// 一致，不含任何 mihomo 代码）。
var fakeCoreBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "twsvc-build")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: mkdir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	fakeCoreBin = filepath.Join(dir, "fakecore")
	build := exec.Command("go", "build", "-o", fakeCoreBin, "../core/testdata/fakecore")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: build fakecore: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

// shortTempDir 返回 /tmp 下的短路径临时目录。WHY 不用 t.TempDir()：macOS unix
// socket sun_path 上限 104 字符（F19），默认临时目录（/var/folders/...）超限。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "twsvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// writeServiceFixture 组装桩内核 + service.json 锚（sock 全部落 /tmp 短路径；
// fakecore 经 Manager 正常模式 spawn——锚只含路径，模式无注入面）。
func writeServiceFixture(t *testing.T, dir string) (ServerConfig, ConfigFile) {
	t.Helper()
	runtime := filepath.Join(dir, "runtime.yaml")
	if err := os.WriteFile(runtime, []byte("proxies: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(dir, "mihomo.sha256")
	sum, err := exec.Command("shasum", "-a", "256", fakeCoreBin).Output()
	if err != nil {
		t.Fatalf("shasum: %v", err)
	}
	if err := os.WriteFile(sidecar, sum, 0o600); err != nil {
		t.Fatal(err)
	}
	conf := ConfigFile{
		AllowedUIDs: []int{os.Getuid()},
		DataDir:     dir,
		CoreBin:     fakeCoreBin,
		Sidecar:     sidecar,
		Runtime:     runtime,
		Sock:        filepath.Join(dir, "core.sock"),
		Log:         filepath.Join(dir, "logs", "core-service.log"),
		DataUID:     os.Getuid(),
	}
	if err := writeJSONFile(filepath.Join(dir, "service.json"), conf); err != nil {
		t.Fatal(err)
	}
	return ServerConfig{
		SockPath:   filepath.Join(dir, "svc.sock"),
		ConfigPath: filepath.Join(dir, "service.json"),
		PIDPath:    filepath.Join(dir, "core.pid"),
	}, conf
}

// trustRecorder 是执行目录信任链复核的测试桩：默认放行并计数，可切换为拒绝。
// WHY 需要桩：测试夹具都在 /tmp 下，祖先含系统符号链接 /tmp 与全局可写目录，
// 生产复核（真 Lstat）对夹具必拒——复核逻辑本身由 ownership_test.go 单独覆盖。
type trustRecorder struct {
	mu    sync.Mutex
	calls int
	deny  error
}

func (r *trustRecorder) verify(ConfigFile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.deny
}

func (r *trustRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *trustRecorder) failWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deny = err
}

// allowTrust 给直接构造的 Server 装上放行桩。
func allowTrust(s *Server) *trustRecorder {
	rec := &trustRecorder{}
	s.verifyExecTrust = rec.verify
	return rec
}

// startStubServer 启动服务（后台 goroutine + 可取消 ctx），等待 socket 就绪；
// Cleanup 负责取消并等待 Run 优雅收敛。返回复核桩（默认放行）供用例计数/切换。
func startStubServer(t *testing.T, cfg ServerConfig) *trustRecorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer(cfg)
	trust := allowTrust(srv)
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("service did not shut down within 15s")
		}
	})
	waitForFile(t, cfg.SockPath, 5*time.Second)
	return trust
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s did not appear within %s", path, timeout)
}

// TestServiceStartStopStatusFlow 全流程（批⑥ §6.2）：start（同步握手，pid>0）→
// status（pid+version）→ stop → status（pid=0）→ ctx 取消优雅收敛（Cleanup）。
func TestServiceStartStopStatusFlow(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	startStubServer(t, cfg)

	cl := NewClient(cfg.SockPath, 30*time.Second)
	resp, err := cl.Start(1)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !resp.OK || resp.PID <= 0 || resp.AlreadyRunning {
		t.Fatalf("Start resp = %+v, want ok pid>0 no-already-running", resp)
	}
	st, err := cl.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.OK || st.PID != resp.PID || st.Version == "" {
		t.Fatalf("Status resp = %+v, want pid=%d version non-empty", st, resp.PID)
	}
	waitForFile(t, conf.Sock, 2*time.Second) // 服务托管内核的 core.sock 在位
	if pidData, err := os.ReadFile(cfg.PIDPath); err != nil || strings.TrimSpace(string(pidData)) != fmt.Sprint(resp.PID) {
		t.Fatalf("core.pid = %q err %v, want %d", pidData, err, resp.PID)
	}
	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st, err = cl.Status()
	if err != nil {
		t.Fatalf("Status after stop: %v", err)
	}
	if st.PID != 0 {
		t.Fatalf("Status after stop pid = %d, want 0", st.PID)
	}
	if _, err := os.Stat(cfg.PIDPath); !os.IsNotExist(err) {
		t.Fatalf("core.pid should be removed after stop, err = %v", err)
	}
}

// TestServiceStartIdempotentNoOp start 幂等（评审 P1-5）：core 托管中再收 start →
// no-op，返回现 pid + already_running（不重复 spawn——防端口冲突握手循环）。
func TestServiceStartIdempotentNoOp(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	startStubServer(t, cfg)

	cl := NewClient(cfg.SockPath, 30*time.Second)
	first, err := cl.Start(1)
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	second, err := cl.Start(99)
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}
	if !second.OK || !second.AlreadyRunning || second.PID != first.PID {
		t.Fatalf("second Start = %+v, want ok already_running pid=%d", second, first.PID)
	}
	st, _ := cl.Status()
	if st.PID != first.PID {
		t.Fatalf("pid changed after no-op start: %d != %d", st.PID, first.PID)
	}
}

// TestServiceStaleGenerationRejected generation 仲裁（批⑥ §3.1）：start 须 >
// currentGeneration——陈旧请求拒绝（错误文案指明期望值）。WHY 单测形态：拒绝路径
// 需要"curGen>0 且 core 未托管"态，服务流程只在 Monitor 预算耗尽（gaveUp）后出现，
// 桩内核无法安全触发——直接驱动 handleStart（同包）构造该态；准入/幂等/stop 归零
// 的集成面由 Flow/NoOp 两个测试覆盖。
func TestServiceStaleGenerationRejected(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	s := NewServer(cfg)
	s.conf = conf
	s.rootCtx = context.Background()
	allowTrust(s)
	s.curGen = 5 // 模拟"上一代 start 已以 5 准入"

	resp := s.handleStart(Request{Action: ActionStart, Generation: 5})
	if resp.OK || !strings.Contains(resp.Error, "generation") || !strings.Contains(resp.Error, "> 5") {
		t.Fatalf("stale Start(5) = %+v, want rejection with expectation", resp)
	}
	resp = s.handleStart(Request{Action: ActionStart, Generation: 1})
	if resp.OK {
		t.Fatalf("stale Start(1) = %+v, want rejection", resp)
	}
	// 陈旧代际的幂等分支先行语义：core 托管中 → no-op 而非拒绝（评审 P1-5）
	if resp := s.handleStart(Request{Action: ActionStart, Generation: 6}); !resp.OK {
		t.Fatalf("Start(6) = %+v, want accepted (spawn)", resp)
	}
	if !s.coreManaged() {
		t.Fatal("Start(6) should have spawned the core")
	}
	stale := s.handleStart(Request{Action: ActionStart, Generation: 1})
	if !stale.OK || !stale.AlreadyRunning {
		t.Fatalf("stale Start(1) while managed = %+v, want idempotent no-op", stale)
	}
}

// TestServiceStopResetsGenerationBaseline 显式 stop 后代际基线归零（放宽准入）：
// 同代际 start 重放被接受——WHY 防"服务 start 成功但 start 进程 state 写入丢失"
// 的代际死锁（下次 start 携带同代际必须能准入）；与"服务重启归零只会放宽准入"
// 同一 F13 语义，双 spawn 由 start 幂等 no-op + flock 串行化兜住。
func TestServiceStopResetsGenerationBaseline(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	startStubServer(t, cfg)

	cl := NewClient(cfg.SockPath, 30*time.Second)
	if _, err := cl.Start(1); err != nil {
		t.Fatalf("Start(1): %v", err)
	}
	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	resp, err := cl.Start(1)
	if err != nil || !resp.OK || resp.AlreadyRunning {
		t.Fatalf("replayed Start(1) after stop = %+v err %v, want accepted (baseline reset)", resp, err)
	}
}

// TestServiceRejectsForeignUID 非法 uid 断连（评审 P3）：allowed_uids 不含本进程
// uid → connect 成功但被即刻断连（无响应，白名单是唯一认证闸）。
func TestServiceRejectsForeignUID(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	conf.AllowedUIDs = []int{os.Getuid() + 12345} // 白名单换成无关 uid
	if err := writeJSONFile(cfg.ConfigPath, conf); err != nil {
		t.Fatal(err)
	}
	startStubServer(t, cfg)

	cl := NewClient(cfg.SockPath, 5*time.Second)
	if _, err := cl.Status(); err == nil {
		t.Fatal("foreign uid request must be rejected, got response")
	}
}

// TestServiceRejectsUnknownFields 协议层拒收未知字段（批⑥ §3.1 请求零路径）：
// 携带路径等未知字段的请求 / 未知 action / 非法 JSON → 带内 ok:false 错误应答。
func TestServiceRejectsUnknownFields(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	startStubServer(t, cfg)

	for _, payload := range []string{
		`{"action":"start","path":"/etc/passwd"}`,
		`{"action":"restart"}`,
		`not json at all`,
	} {
		resp, err := rawRequest(cfg.SockPath, payload)
		if err != nil {
			t.Fatalf("payload %s: transport error %v (want in-band error response)", payload, err)
		}
		if resp.OK {
			t.Fatalf("payload %s accepted; want rejection", payload)
		}
	}
}

// rawRequest 原始行写入（协议层负路径测试，不走 Client 编码）。
func rawRequest(sockPath, payload string) (Response, error) {
	conn, err := DialUnix(sockPath, 5*time.Second)
	if err != nil {
		return Response{}, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(payload + "\n")); err != nil {
		return Response{}, err
	}
	return ReadResponse(conn)
}

// TestServiceAdoptsOrphanCore 孤儿收编（R13）：服务启动时 core.pid 记录的孤儿
// 内核存活且身份匹配 → SIGTERM 回收 → 正常 start 流程收编（新 pid 接管）。
func TestServiceAdoptsOrphanCore(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	// 手工拉起"孤儿"桩内核（同 Manager 参数形态：-d dataDir -f runtime -ext-ctl-unix sock）
	orphan := exec.Command(fakeCoreBin, "-d", conf.DataDir, "-f", conf.Runtime,
		"-ext-ctl-unix", conf.Sock)
	orphan.Env = append(os.Environ(), "FAKE_CORE_MODE=normal")
	if err := orphan.Start(); err != nil {
		t.Fatalf("spawn orphan: %v", err)
	}
	// 并发 Wait 收尸：不收尸则孤儿退出后成僵尸，processAlive(zombie) 恒真——
	// 服务的 killGrace 轮询会耗满 5s（生产中孤儿由 launchd 收养收尸，无此问题）
	reaped := make(chan struct{})
	go func() { _ = orphan.Wait(); close(reaped) }()
	t.Cleanup(func() {
		select {
		case <-reaped:
		default:
			_ = orphan.Process.Kill()
		}
	})
	waitForFile(t, conf.Sock, 5*time.Second)
	if err := os.WriteFile(cfg.PIDPath, []byte(fmt.Sprintf("%d\n", orphan.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	startStubServer(t, cfg)
	// 孤儿被回收（旧 pid 死）+ 服务以新内核收编（core.pid 记录新 pid、sock 可达）
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(cfg.PIDPath)
		if err == nil {
			var newPID int
			if _, perr := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &newPID); perr == nil &&
				newPID > 0 && newPID != orphan.Process.Pid && probeAlive(conf.Sock) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("orphan core not adopted within 15s")
}

// TestServiceConfigInvalidExits 锚非法/缺失 → Run 返回错误（CLI exit 1 → launchd
// 节流重试，R3——绝不带病运行）。
func TestServiceConfigInvalidExits(t *testing.T) {
	dir := shortTempDir(t)
	cfg := ServerConfig{SockPath: filepath.Join(dir, "svc.sock"), ConfigPath: filepath.Join(dir, "missing.json"), PIDPath: filepath.Join(dir, "core.pid")}
	if err := NewServer(cfg).Run(context.Background()); err == nil {
		t.Fatal("missing service.json: want error")
	}
	if err := os.WriteFile(cfg.ConfigPath, []byte(`{"allowed_uids":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewServer(cfg).Run(context.Background()); err == nil {
		t.Fatal("invalid service.json (empty allowed_uids): want error")
	}
}

// TestChownDataDirBack 属主回收递归遍历（R10 的纯逻辑层——root 方向翻转进真机目验，
// R9）：嵌套目录/文件全量可达、无错误。
func TestChownDataDirBack(t *testing.T) {
	dir := shortTempDir(t)
	nested := filepath.Join(dir, "logs")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "core-service.log"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chownDataDirBack(dir, os.Getuid()); err != nil {
		t.Fatalf("chown back: %v", err)
	}
}

// probeAlive 测试内 /version 探活。
func probeAlive(sockPath string) bool {
	cl := core.NewClient(sockPath, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := cl.Version(ctx)
	return err == nil
}

// TestChownDataDirBackSkipsSymlinks chown 提权面封堵（batch6 review P1-1）：
// dataDir 内植入 symlink（悬空 + 指向外部文件），回收回调必须跳过——旧实现
// os.Chown 走 chown(2) 跟随 symlink：悬空链必败（ENOENT 污染回收）、root 下
// 指向 /etc/sudoers 等会被 chown 给用户 uid（本地提权）。
func TestChownDataDirBackSkipsSymlinks(t *testing.T) {
	base := shortTempDir(t)
	dataDir := filepath.Join(base, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "cache.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "target.txt") // dataDir 之外——绝不被触碰
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "missing-target"), filepath.Join(dataDir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dataDir, "linked")); err != nil {
		t.Fatal(err)
	}
	// 旧实现：dangling symlink 令 chown(2) ENOENT → 整个回收报错（行为可辨锚）
	if err := chownDataDirBack(dataDir, os.Getuid()); err != nil {
		t.Fatalf("chown back with symlinks: %v (symlink must be skipped)", err)
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("external target must stay untouched: mode=%v err=%v", info.Mode(), err)
	}
}

// TestXucredUIDDecodesUint32 cr_uid 32 位解析（batch6 review P1-2）：uid_t=
// __uint32_t——>65535 的 uid 必须完整解析；旧 Uint16 读法在该样例上只取低 16 位
// （碰撞误判），本样例构造为两法可辨。
func TestXucredUIDDecodesUint32(t *testing.T) {
	buf := make([]byte, xucredSize)
	binary.LittleEndian.PutUint32(buf[0:4], xucredVersion)
	const uid = 16777216 // >65535（2^24）
	binary.LittleEndian.PutUint32(buf[xucredUIDOff:xucredUIDOff+xucredUIDSize], uid)
	got, err := xucredUID(buf)
	if err != nil {
		t.Fatalf("xucredUID: %v", err)
	}
	if got != uid {
		t.Fatalf("uid = %d, want %d (uint32 parse)", got, uid)
	}
	low16 := int(binary.LittleEndian.Uint16(buf[xucredUIDOff : xucredUIDOff+2]))
	if low16 == uid {
		t.Fatal("sample must distinguish uint16 vs uint32 parse")
	}
}

// TestXucredUIDRejectsShortBufferAndBadVersion 解析防御面：短缓冲/版本不符拒绝。
func TestXucredUIDRejectsShortBufferAndBadVersion(t *testing.T) {
	if _, err := xucredUID(make([]byte, 6)); err == nil {
		t.Fatal("short buffer: want error")
	}
	buf := make([]byte, xucredSize)
	binary.LittleEndian.PutUint32(buf[0:4], 7) // 非 XUCRED_VERSION
	if _, err := xucredUID(buf); err == nil {
		t.Fatal("bad version: want error")
	}
}

// TestAllowedUIDsMatchLargeUID 白名单匹配 >65535 uid（P1-2 配套：白名单与解析
// 同为 int 全宽，uint16 截断不可 resurrect）。
func TestAllowedUIDsMatchLargeUID(t *testing.T) {
	if !containsInt([]int{501, 16777216}, 16777216) {
		t.Fatal("large uid in whitelist must match")
	}
	if containsInt([]int{501}, 16777216) {
		t.Fatal("uid not in whitelist must not match")
	}
}
