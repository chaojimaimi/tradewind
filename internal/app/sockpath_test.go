package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// sockpath_test.go：内核 API 路径按形态分流的判定测试（批⑥b §4.3，F22）。

// saveTunState 写 state.Core 形态（测试用）。
func saveTunState(t *testing.T, home paths.Home, tun bool) {
	t.Helper()
	st := store.DefaultState()
	st.Core = store.CoreState{Desired: store.DesiredRunning, PID: 42000, Generation: 1, Tun: tun}
	if err := store.SaveState(home, st); err != nil {
		t.Fatal(err)
	}
}

// TestCoreSockPathFollowsForm CLI 侧判据（state.Core.Tun）：真→代理入口；假→用户态
// sock；state 读不出→用户态路径（读失败按 false，绝不因读盘失败拨代理入口）。
func TestCoreSockPathFollowsForm(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	broker := stubBrokerSockPath(t)

	saveTunState(t, home, true)
	if got := coreSockPath(home); got != broker {
		t.Fatalf("coreSockPath(state Tun=true) = %q, want the broker path %q", got, broker)
	}

	saveTunState(t, home, false)
	if got := coreSockPath(home); got != home.SockFile() {
		t.Fatalf("coreSockPath(state Tun=false) = %q, want the user-mode sock %q", got, home.SockFile())
	}

	// state 损坏 = 读不出 → 用户态路径
	if err := store.WriteFileRaw(home.StateFile(), []byte("not-json")); err != nil {
		t.Fatal(err)
	}
	if got := coreSockPath(home); got != home.SockFile() {
		t.Fatalf("coreSockPath(unreadable state) = %q, want the user-mode sock", got)
	}
}

// TestStartProcessPathsFollowSettingsNotState start 进程内部判据（settings）：形态
// 与 state.Core.Tun 预置成相反的值时，网关与启动后版本读取共用的路径选择必须按
// settings 推。必含场景：TUN 会话的 start 被 kill -9（状态残留 Tun=true）→ tun off
// → 新的用户态会话：上游仍是 <data_dir>/core.sock，不因陈旧状态拨向已被摘除的
// 代理 socket（批⑥b §4.3 的 WHY 案例）。
func TestStartProcessPathsFollowSettingsNotState(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	broker := stubBrokerSockPath(t)

	// settings 开、state 残留关（新 TUN 会话尚未写状态）：网关/版本读取拨代理入口
	settings := store.DefaultSettings()
	settings.TunEnabled = true
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	saveTunState(t, home, false)
	if got := startFormSockPath(home, settings); got != broker {
		t.Fatalf("startFormSockPath(settings on) = %q, want the broker path %q", got, broker)
	}

	// kill -9 场景：TUN 会话的 start 被杀 → settings 与 state.Core.Tun 都是 true；
	// tun off 翻关 settings（假编排不写 state）→ 残留 Tun=true 的新用户态会话，
	// 网关上游必须是用户态 sock，不因陈旧状态拨向已被摘除的代理 socket
	saveTunState(t, home, true)
	env, _, _, _, _ := newTunTestEnv(t) // 代理入口缝隙已指向桩（不触真锚目录）
	if err := tunOffWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOffWith: %v", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be off after tun off")
	}
	if got := coreInTunForm(home); !got {
		t.Fatal("precondition: state.Core.Tun must still read true (kill -9 residue, fake env never marks stopped)")
	}
	now := tunSettings(t, home)
	if got := startFormSockPath(home, now); got != home.SockFile() {
		t.Fatalf("startFormSockPath(settings off, stale state Tun=true) = %q, want the user-mode sock %q (not the broker path %q)",
			got, home.SockFile(), broker)
	}
}

// TestTunOffProbeSeesEitherForm settings 说关时，用户态 sock 与代理入口任一可达 →
// 不走 "already disabled"，落入收敛（stopStart 被调用）；两条都不可达才 no-op。
func TestTunOffProbeSeesEitherForm(t *testing.T) {
	t.Run("user sock reachable", func(t *testing.T) {
		home := paths.Home(stubSocketDir(t))
		seedTunOffHome(t, home)
		env, _, _, stopStarts, _ := newTunTestEnv(t)
		listenVersionStub(t, home) // 用户态可达 = 漂移
		assertTunOffConverges(t, home, env, stopStarts)
	})
	t.Run("broker sock reachable", func(t *testing.T) {
		home := paths.Home(stubSocketDir(t))
		seedTunOffHome(t, home)
		env, _, _, stopStarts, _ := newTunTestEnv(t)
		listenVersionStubAt(t, tunBrokerSockPath) // 代理入口可达 = TUN 残留
		assertTunOffConverges(t, home, env, stopStarts)
	})
	t.Run("neither reachable", func(t *testing.T) {
		home := paths.Home(stubSocketDir(t))
		seedTunOffHome(t, home)
		env, _, spawns, stopStarts, _ := newTunTestEnv(t)
		if err := tunOffWith(context.Background(), home, env); err != nil {
			t.Fatalf("tunOffWith: %v", err)
		}
		if *stopStarts != 0 || *spawns != 0 {
			t.Fatalf("neither reachable must stay a no-op: stopStarts=%d spawns=%d", *stopStarts, *spawns)
		}
	})
}

// assertTunOffConverges 断言 tun off 越过幂等 no-op、落入收敛（stopStart 恰一次）。
func assertTunOffConverges(t *testing.T, home paths.Home, env tunEnv, stopStarts *int) {
	t.Helper()
	if err := tunOffWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOffWith: %v", err)
	}
	if *stopStarts != 1 {
		t.Fatalf("stopStart count = %d, want 1 (reachable probe must converge, not no-op)", *stopStarts)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be off")
	}
}

// TestWaitCoreReadyPollsGivenPath waitCoreReady 轮询的是**给定**路径：桩在路径 A，
// wait(B)=false、wait(A)=true（改签名后的行为锚，批⑥b §4.3）。
func TestWaitCoreReadyPollsGivenPath(t *testing.T) {
	pathA := filepath.Join(stubSocketDir(t), "a.sock")
	listenVersionStubAt(t, pathA)
	pathB := filepath.Join(stubSocketDir(t), "b.sock") // 无监听
	const secret = "stub"

	if waitCoreReady(pathB, secret, 300*time.Millisecond) {
		t.Fatal("waitCoreReady on a silent path must time out to false")
	}
	if !waitCoreReady(pathA, secret, 2*time.Second) {
		t.Fatal("waitCoreReady on the listening path must return true")
	}
}
