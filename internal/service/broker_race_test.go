package service

// broker_race_test.go：代理入口的 close / start / Serve 协程之间的竞态交错（批⑥b
// §4.3）。真实并发压不出低概率交错（http.Server.Close 会同步等 Serve 返回，旧协程
// 几乎总先拿到锁），所以经 apiBroker.hook 把时序钉成确定性的：
//   - Serve 晚进门：close 抢在它登记 listener 之前，随后的重启被它的 defer 摘掉 socket；
//   - Serve 晚醒来：新实例已在同一路径上监听，旧协程才走到收敛登记；
//   - close 到摘完路径才放锁：并发的 start 必须等它做完。

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestBrokerCloseThenRestartKeepsNewSocket 紧接的 close → start 循环 200 轮：每轮
// 新实例必须立即可用。钉的是"close() 先摘登记、摘路径，start 才能重新监听"这条主
// 线；循环本身还压着一条偶发——close 落在 Serve 进门之前的那次重启（150 轮量级才
// 撞上一次），它的确定性回归是 TestBrokerLateServeDoesNotUnlinkNewSocket。
func TestBrokerCloseThenRestartKeepsNewSocket(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	b := startBroker(t, brokerSock, sk.sock(), []int{os.Getuid()})

	for i := 0; i < 200; i++ {
		b.close()
		if err := b.start(brokerSock, sk.sock(), []int{os.Getuid()}); err != nil {
			t.Fatalf("iteration %d: restart: %v", i, err)
		}
		probeBroker(t, b, brokerSock, fmt.Sprintf("iteration %d", i))
	}
	// 给最后一轮的旧协程一个醒来的机会：它醒来之后新实例必须仍然完好
	time.Sleep(50 * time.Millisecond)
	probeBroker(t, b, brokerSock, "after the old serve goroutine had time to wake")
}

// probeBroker 断言代理在 sockPath 上登记在册且 /version 经它可达（生命周期用例共用）。
func probeBroker(t *testing.T, b *apiBroker, sockPath, step string) {
	t.Helper()
	if path, ok := b.listening(); !ok || path != sockPath {
		t.Fatalf("%s: listening = (%q, %v), want (%q, true)", step, path, ok, sockPath)
	}
	if status, _ := brokerDo(t, sockPath, "GET", "/version", nil); status != http.StatusOK {
		t.Fatalf("%s: /version status = %d, want 200 (the socket must stay usable)", step, status)
	}
}

// releaser 返回一个幂等的放行函数，并登记进 t.Cleanup：停在缝里的协程在用例失败的
// 路径上也会被放行——否则收尾的 b.close() 一旦也停进同一个缝，测试二进制就挂死。
// 调用方须先登记 t.Cleanup(b.close) 再调它（Cleanup 后登记的先执行）。
func releaser(t *testing.T, ch chan struct{}) func() {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return release
}

// TestBrokerLateServeDoesNotUnlinkNewSocket 回归（150 轮量级的真实偶发）：start 之后
// Serve 协程还没进门，close() 就到了——http.Server.Close 立即返回（没有已登记的
// listener）、路径被摘、下一次 start 在同一路径上重新监听；这时旧协程才进 Serve，
// 一进门就因"已在关停"返回。它 defer 关自己的 listener 时**不得**按路径 unlink——
// 那条路径现在是新实例的（WHY start 里关掉 net 的"关闭即 unlink"）。
func TestBrokerLateServeDoesNotUnlinkNewSocket(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	entered, release := make(chan struct{}), make(chan struct{})
	exited := make(chan struct{}, 1)
	var once sync.Once
	b := newAPIBroker()
	b.hook = func(stage hookStage) {
		switch stage {
		case stageBeforeServe:
			once.Do(func() { close(entered) })
			<-release // 第二个实例的协程进来时 release 已关，直接通过
		case stageServeFinished:
			select {
			case exited <- struct{}{}:
			default:
			}
		}
	}
	t.Cleanup(b.close)
	unpark := releaser(t, release)
	if err := b.start(brokerSock, sk.sock(), []int{os.Getuid()}); err != nil {
		t.Fatal(err)
	}
	<-entered

	b.close() // Serve 未进门：Close 立即返回，路径被摘
	if _, err := os.Lstat(brokerSock); !os.IsNotExist(err) {
		t.Fatalf("socket must be removed by close (err = %v)", err)
	}
	if err := b.start(brokerSock, sk.sock(), []int{os.Getuid()}); err != nil {
		t.Fatalf("restart while the old serve goroutine has not entered Serve: %v", err)
	}
	unpark() // 新实例已在同一路径上监听，旧协程现在才进门——进门即退
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the old serve goroutine never returned from Serve")
	}
	// 旧协程的 defer（关它自己的 listener）已跑完：新实例的 socket 必须还在
	if _, err := os.Lstat(brokerSock); err != nil {
		t.Fatalf("the late Serve unlinked the new instance's socket: %v", err)
	}
	probeBroker(t, b, brokerSock, "after the old serve goroutine exited")
}

// TestBrokerStaleServeExitKeepsNewInstance 旧 Serve 协程晚醒：close() 之后新实例已在
// 同一路径上监听，上一轮的协程这时才走到收敛登记——它不得摘掉新实例的 socket、
// 不得清掉新登记。WHY 要缝：http.Server.Close() 同步等 Serve 返回，旧协程几乎总是
// 先拿到锁，裸写的紧接 close/start 循环撞不出这种交错（变异实测：去掉登记判定，
// 循环用例照样通过）。
func TestBrokerStaleServeExitKeepsNewInstance(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	parked, release := make(chan struct{}), make(chan struct{})
	finished := make(chan struct{}, 1)
	var once sync.Once
	b := newAPIBroker()
	b.hook = func(stage hookStage) {
		switch stage {
		case stageServeReturned:
			once.Do(func() { close(parked) })
			<-release
		case stageServeFinished:
			select {
			case finished <- struct{}{}:
			default:
			}
		}
	}
	t.Cleanup(b.close)
	unpark := releaser(t, release)
	if err := b.start(brokerSock, sk.sock(), []int{os.Getuid()}); err != nil {
		t.Fatal(err)
	}

	b.close() // 摘登记 → Close（等 Serve 返回）→ 摘路径；旧协程此刻停在缝里
	<-parked
	if err := b.start(brokerSock, sk.sock(), []int{os.Getuid()}); err != nil {
		t.Fatalf("restart while the old serve goroutine is parked: %v", err)
	}
	probeBroker(t, b, brokerSock, "restart")

	unpark()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("the stale serve goroutine never finished")
	}
	probeBroker(t, b, brokerSock, "after the stale serve goroutine finished")
}

// TestBrokerConcurrentCloseAndStartKeepBrokerUsable close 与 start 真并发跑 100 轮
// （关停路径上代理的 close 在 s.mu 之外，迟到的 start 请求可能撞上）：不 panic、不死
// 锁，事后补一次 start 必然得到一个可用的新实例。WHY 另有确定性用例：这种交错里
// 关闭方几乎总先完成摘除，"close 全程持锁"那条性质靠并发压不出来（见
// TestBrokerCloseHoldsLockUntilPathRemoved）。
func TestBrokerConcurrentCloseAndStartKeepBrokerUsable(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	uids := []int{os.Getuid()}
	b := startBroker(t, brokerSock, sk.sock(), uids)

	for i := 0; i < 100; i++ {
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			b.close()
		}()
		if err := b.start(brokerSock, sk.sock(), uids); err != nil {
			t.Fatalf("iteration %d: concurrent start: %v", i, err)
		}
		<-closed
		// 收尾：确保在监听（已监听则 no-op），然后必须可达
		if err := b.start(brokerSock, sk.sock(), uids); err != nil {
			t.Fatalf("iteration %d: settle start: %v", i, err)
		}
		probeBroker(t, b, brokerSock, fmt.Sprintf("iteration %d after concurrent close/start", i))
	}
}

// TestBrokerCloseHoldsLockUntilPathRemoved close 到摘完路径才放锁：并发的 start 必须
// 等它做完。否则 start 会在旧路径还没摘掉时重新监听，随后被旧 close 的 os.Remove
// 摘掉新实例的 socket（登记还在、路径已没——再 start 成了 no-op，探不到）。WHY 要
// 缝：真并发时关闭方几乎总先完成摘除（变异实测 2000 轮撞不出反例），这里把 close
// 停在"已关服务、未摘路径"处，让 start 明确插进来。
func TestBrokerCloseHoldsLockUntilPathRemoved(t *testing.T) {
	dir := shortTempDir(t)
	sk := newStubKernel(t)
	brokerSock := filepath.Join(dir, "broker.sock")
	uids := []int{os.Getuid()}
	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	b := newAPIBroker()
	b.hook = func(stage hookStage) {
		if stage == stageCloseBeforeUnlink {
			once.Do(func() { close(parked) })
			<-release // 收尾的 close 再进来时 release 已关，直接通过
		}
	}
	t.Cleanup(b.close)
	unpark := releaser(t, release)
	if err := b.start(brokerSock, sk.sock(), uids); err != nil {
		t.Fatal(err)
	}
	probeBroker(t, b, brokerSock, "before close")

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		b.close()
	}()
	<-parked
	started := make(chan error, 1)
	go func() { started <- b.start(brokerSock, sk.sock(), uids) }()
	// close 还没摘路径：start 必须仍在等锁，不得先行重新监听
	select {
	case err := <-started:
		t.Fatalf("start returned %v while close has not unlinked the old path yet (close must hold its lock until then)", err)
	case <-time.After(100 * time.Millisecond):
	}
	unpark()
	<-closed
	if err := <-started; err != nil {
		t.Fatalf("start after close: %v", err)
	}
	probeBroker(t, b, brokerSock, "after close and start")
}
