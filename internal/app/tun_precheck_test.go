package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// tun_precheck_test.go：`tun on` 的归一化预检与 synthesizeBytes 拆分（批⑥b §4.4）。

// writeGlobalMergeSlot 写全局 merge 槽（<home>/enhance/merge.yaml）。
func writeGlobalMergeSlot(t *testing.T, home paths.Home, content string) {
	t.Helper()
	path := home.EnhanceGlobalMerge()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTunOnPrecheckRejectsBeforeAnyChange 增强链 merge 槽里有必拒键（external-ui）
// → tunOnWith 报错带键名；settings.tun_enabled 仍为 false；零 stopStart、零
// spawnStart、零 elevate（预检在任何状态改动与提权之前）。
func TestTunOnPrecheckRejectsBeforeAnyChange(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	writeGlobalMergeSlot(t, home, "external-ui: /tmp/x\n")
	env, _, spawns, stopStarts, _ := newTunTestEnv(t)
	elevates := 0
	env.elevate = func(string) error { elevates++; return nil }

	err := tunOnWith(context.Background(), home, env)
	if err == nil || !strings.Contains(err.Error(), "external-ui") {
		t.Fatalf("err = %v, want the precheck rejection carrying the key name", err)
	}
	if tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must stay false (precheck precedes any state change)")
	}
	if *spawns != 0 || *stopStarts != 0 || elevates != 0 {
		t.Fatalf("precheck rejection must be zero-action: spawns=%d stopStarts=%d elevates=%d",
			*spawns, *stopStarts, elevates)
	}
}

// TestTunOnPrecheckUsesTunForm 方向性证明：merge 槽在 dns 下放白名单外的键。
// 非 TUN 形态合成 → 归一化拒绝；TUN 形态合成（dns 块被整体替换）→ 接受；
// tunOnWith 越过预检、流程照旧——三者合起来证明预检确实用的是 TUN 形态。
func TestTunOnPrecheckUsesTunForm(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedTunOffHome(t, home)
	writeGlobalMergeSlot(t, home, "dns:\n  bogus-key: 1\n")
	secret, err := store.EnsureSecret(home)
	if err != nil {
		t.Fatal(err)
	}

	// 非 TUN 形态：模板 dns 段 + 合并槽的 bogus-key → 拒绝
	plain, _, err := synthesizeBytes(home, secret, false)
	if err != nil {
		t.Fatalf("synthesizeBytes(false): %v", err)
	}
	if _, rej := service.NormalizeRuntime(plain); rej == nil {
		t.Fatal("non-TUN synthesis carrying dns.bogus-key must be rejected")
	}
	// TUN 形态：dns 块在链末被整体替换，bogus-key 到不了产物 → 接受
	tunForm, _, err := synthesizeBytes(home, secret, true)
	if err != nil {
		t.Fatalf("synthesizeBytes(true): %v", err)
	}
	res, rej := service.NormalizeRuntime(tunForm)
	if rej != nil {
		t.Fatalf("TUN-form synthesis must pass normalization (dns block replaced wholesale): %v", rej)
	}
	if len(res.Ignored) != 0 {
		t.Fatalf("TUN-form normalization ignored = %v, want empty", res.Ignored)
	}

	// tunOnWith 越过预检、收敛照旧（假编排记录到收敛步骤）
	env, _, spawns, _, _ := newTunTestEnv(t)
	if err := tunOnWith(context.Background(), home, env); err != nil {
		t.Fatalf("tunOnWith must pass the precheck and converge: %v", err)
	}
	if *spawns != 1 {
		t.Fatalf("spawn count = %d, want 1 (converge proceeded past the precheck)", *spawns)
	}
	if !tunSettings(t, home).TunEnabled {
		t.Fatal("tun_enabled must be on after a passing precheck + converge")
	}
}

// TestSynthesizeBytesMatchesSynthesize 两种形态下 synthesizeBytes（传同值）的产物
// 与 Synthesize 写出的文件逐字节相同，且两种形态的产物互不相同（拆分零回归锚）。
func TestSynthesizeBytesMatchesSynthesize(t *testing.T) {
	for _, tunOn := range []bool{false, true} {
		home := paths.Home(stubSocketDir(t))
		seedRuntimeInputs(t, home)
		settings := store.DefaultSettings()
		settings.TunEnabled = tunOn
		if err := store.SaveSettings(home, settings); err != nil {
			t.Fatal(err)
		}
		const secret = "stub-secret"

		out, _, err := synthesizeBytes(home, secret, tunOn)
		if err != nil {
			t.Fatalf("synthesizeBytes(tun=%v): %v", tunOn, err)
		}
		if err := Synthesize(home, secret); err != nil {
			t.Fatalf("Synthesize(tun=%v): %v", tunOn, err)
		}
		onDisk, err := os.ReadFile(home.RuntimeFile())
		if err != nil {
			t.Fatalf("read runtime.yaml: %v", err)
		}
		if string(onDisk) != string(out) {
			t.Fatalf("tun=%v: synthesizeBytes output diverges from Synthesize's file:\n--- bytes ---\n%s\n--- file ---\n%s",
				tunOn, out, onDisk)
		}

		// 形态产物互不相同：TUN 形态多了链末注入的 tun/dns 块
		other, _, err := synthesizeBytes(home, secret, !tunOn)
		if err != nil {
			t.Fatalf("synthesizeBytes(tun=%v): %v", !tunOn, err)
		}
		if string(other) == string(out) {
			t.Fatalf("tun=%v: the two synthesis forms must not be byte-identical", tunOn)
		}
	}
}
