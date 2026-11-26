package service

// normalize_gate_test.go：归一化并发闸（批⑥b c3b 审查 P1）。start 与 reload 两条
// 路径都过闸；闸只在 loadNormalized 内部持有——它返回即释放，绝不跨到获取 s.mu
// 之上（锁序恒为 s.mu → normalizeGate）。用例全部以"占住闸、看请求是否排队"的
// 方式钉住行为，不依赖归一化耗时。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNormalizeGateHeldOnlyInLoader：闸只在 rootstate.go（loadNormalized 所在）出现。
// 这是锁序 s.mu → normalizeGate 的结构保证：持闸的代码是一个不取任何锁的叶子函数，
// 闸不可能跨到获取 s.mu 之上。别的文件一旦引用闸（比如在 handleReload 里持闸取锁），
// 互锁就只隔一次调度——行为测试撞不上那种窗口，所以在源码层面钉死。按语法树的
// 标识符匹配：注释里提到闸的名字是说明，不是引用。
func TestNormalizeGateHeldOnlyInLoader(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "rootstate.go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		used := false
		ast.Inspect(file, func(node ast.Node) bool {
			if id, ok := node.(*ast.Ident); ok && id.Name == "normalizeGate" {
				used = true
			}
			return true
		})
		if used {
			offenders = append(offenders, name)
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("normalizeGate may only be used in rootstate.go (loadNormalized); found in %v", offenders)
	}
}

// holdNormalizeGate 占住归一化闸，返回幂等的归还函数（Cleanup 兜底：用例中途
// Fatal 也不会把闸留给后面的用例）。
func holdNormalizeGate(t *testing.T) (release func()) {
	t.Helper()
	normalizeGate <- struct{}{}
	var once sync.Once
	release = func() { once.Do(func() { <-normalizeGate }) }
	t.Cleanup(release)
	return release
}

// gateIsFree 报告闸此刻是否可取（取到即归还，不留占）。
func gateIsFree() bool {
	select {
	case normalizeGate <- struct{}{}:
		<-normalizeGate
		return true
	default:
		return false
	}
}

// waitGateFree 轮询至闸可取或超时。
func waitGateFree(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gateIsFree() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("normalizeGate stayed held")
}

// TestLoadNormalizedReleasesGate：闸只在 loadNormalized 内部持有——无论结局如何，
// 它返回后必须立即可取。
func TestLoadNormalizedReleasesGate(t *testing.T) {
	dir := shortTempDir(t)
	_, conf := writeServiceFixture(t, dir)

	if _, _, err := loadNormalized(conf); err != nil {
		t.Fatalf("loadNormalized(accepted config): %v", err)
	}
	if !gateIsFree() {
		t.Fatal("gate still held after an accepted loadNormalized")
	}

	if err := os.WriteFile(conf.UserRuntime, []byte(rejectedUserConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, rej, err := loadNormalized(conf); rej == nil || err != nil {
		t.Fatalf("loadNormalized(rejected config) = rej %v err %v, want rejection", rej, err)
	}
	if !gateIsFree() {
		t.Fatal("gate still held after a rejected loadNormalized")
	}

	if err := os.Remove(conf.UserRuntime); err != nil {
		t.Fatal(err)
	}
	if _, rej, err := loadNormalized(conf); rej != nil || err == nil {
		t.Fatalf("loadNormalized(missing file) = rej %v err %v, want read error", rej, err)
	}
	if !gateIsFree() {
		t.Fatal("gate still held after a failed read")
	}
}

// TestReloadWaitsForNormalizeGate：闸被占时 reload 不开始归一化（排队，不叠加
// 内存）；归还后照常完成。排队期间 status 照常应答——归一化在 s.mu 之外，reload
// 等闸不占锁（R15）。
func TestReloadWaitsForNormalizeGate(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	release := holdNormalizeGate(t)
	if err := os.WriteFile(conf.UserRuntime, []byte("mode: rule\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		resp Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := cl.Reload()
		done <- outcome{resp, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("reload finished while the gate was held: %+v err %v", got.resp, got.err)
	case <-time.After(300 * time.Millisecond):
	}

	// 排队中的 reload 不得挡住 status（未托管：status 只探 /version，秒回）
	statusDone := make(chan Response, 1)
	go func() {
		st, _ := cl.Status()
		statusDone <- st
	}()
	select {
	case <-statusDone:
	case <-time.After(3 * time.Second):
		t.Fatal("status blocked behind a reload waiting for the gate")
	}

	release()
	select {
	case got := <-done:
		if got.err != nil || !got.resp.OK {
			t.Fatalf("Reload after the gate was released = %+v err %v, want ok", got.resp, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reload did not complete after the gate was released")
	}
}

// TestStartWaitsForNormalizeGate：start 的归一化同样过闸（它在 s.mu 之内调用
// loadNormalized——与持闸的 reload 并发时在此排队）；归还后照常 spawn。
func TestStartWaitsForNormalizeGate(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	startStubServer(t, cfg)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	release := holdNormalizeGate(t)
	type outcome struct {
		resp Response
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		resp, err := cl.Start(1)
		done <- outcome{resp, err}
	}()
	select {
	case got := <-done:
		t.Fatalf("start finished while the gate was held: %+v err %v", got.resp, got.err)
	case <-time.After(300 * time.Millisecond):
	}

	release()
	select {
	case got := <-done:
		if got.err != nil || !got.resp.OK || got.resp.PID <= 0 {
			t.Fatalf("Start after the gate was released = %+v err %v, want ok with a pid", got.resp, got.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("start did not complete after the gate was released")
	}
}

// TestReloadNeverHoldsGateWhileTakingMu（锁序 s.mu → normalizeGate 的回归）：reload
// 在 s.mu 之外归一化、之后才取 s.mu。测试先占住 s.mu，再发 reload：它的归一化
// 取闸、完成、**归还闸**，然后才在 s.mu 上等——所以持 s.mu 期间闸必须保持空闲。
// 若有人把闸改成跨到取 s.mu 之上，闸会被等锁的 reload 一直占着，而持 s.mu 的 start
// 又在等闸——互锁。观测用窗口式（连续占用 ≥200ms 即失败）而非一次快照：快照的
// 首轮轮询几乎必然跑在 reload 取闸之前、看到的是空闲，抓不住违反形状（变异验证
// 实证）；而 reload 的归一化是毫秒级的一次短暂占用，绝不会连续占到 200ms。
func TestReloadNeverHoldsGateWhileTakingMu(t *testing.T) {
	dir := shortTempDir(t)
	cfg, _ := writeServiceFixture(t, dir)
	srv, _ := startStubServerTuned(t, cfg, nil)
	cl := NewClient(cfg.SockPath, 30*time.Second)

	// 幂等解锁 + Cleanup 兜底：用例中途 Fatal 也释放 s.mu，否则服务收尾的 stopCore
	// 取不到锁，startStubServerTuned 的 Cleanup 会等满 15s 报错
	srv.mu.Lock()
	var once sync.Once
	unlock := func() { once.Do(srv.mu.Unlock) }
	t.Cleanup(unlock)

	done := make(chan error, 1)
	go func() {
		_, err := cl.Reload()
		done <- err
	}()
	waitGateFree(t, 5*time.Second) // reload 已归还闸（或尚未取闸）、正在 s.mu 上等
	// 持 s.mu 期间持续观测闸。判据是"连续占用时长"而不是"是否出现过占用"：
	// waitGateFree 只保证某一瞬时空闲，reload 的归一化可能此刻才刚开始（毫秒级的
	// 短暂占用，须容忍）；跨锁持闸则会占到 unlock 为止（数秒）——这是违反形状。
	observed := time.Now()
	var occupiedSince time.Time
	for time.Since(observed) < 600*time.Millisecond {
		if gateIsFree() {
			occupiedSince = time.Time{}
		} else {
			if occupiedSince.IsZero() {
				occupiedSince = time.Now()
			} else if time.Since(occupiedSince) >= 200*time.Millisecond {
				t.Fatalf("normalizeGate has been held for %v while s.mu is taken (the gate must not extend across the lock)",
					time.Since(occupiedSince))
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("reload completed while s.mu was held by the test: %v", err)
	default:
	}

	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Reload: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reload did not complete after s.mu was released")
	}
}
