package app

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// modeStubCore 起一个记录 PATCH /configs body 的桩内核。
func modeStubCore(t *testing.T, seen *[]string) (paths.Home, func()) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			http.Error(w, "want PATCH", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		*seen = append(*seen, string(body))
		w.WriteHeader(http.StatusNoContent)
	})
	return newStubCore(t, mux)
}

func TestModeShowCurrent(t *testing.T) {
	home := paths.Home(t.TempDir())
	settings := store.DefaultSettings()
	settings.Mode = "global"
	if err := store.SaveSettings(home, settings); err != nil {
		t.Fatal(err)
	}
	if err := Mode(context.Background(), home, ""); err != nil {
		t.Fatalf("Mode(show): %v", err)
	}
}

func TestModeSwitchRunningPatchesHot(t *testing.T) {
	// M2-plan §6：settings 回写 + 重合成 + PATCH 热生效，三方一致
	var seen []string
	home, cleanup := modeStubCore(t, &seen)
	defer cleanup()
	seedRuntimeInputs(t, home)

	if err := Mode(context.Background(), home, "global"); err != nil {
		t.Fatalf("Mode(switch): %v", err)
	}
	if len(seen) != 1 || !strings.Contains(seen[0], `"mode":"global"`) {
		t.Fatalf("PATCH bodies = %v, want one {\"mode\":\"global\"}", seen)
	}
	settings, err := store.LoadSettings(home)
	if err != nil || settings.Mode != "global" {
		t.Fatalf("settings.Mode = %q err %v, want global", settings.Mode, err)
	}
	data, err := os.ReadFile(home.RuntimeFile())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("mode: global")) {
		t.Fatalf("runtime.yaml missing mode: global after resynthesize:\n%s", data)
	}
}

func TestModeSwitchNotRunningPersistsOnly(t *testing.T) {
	// 未运行：仅落盘（settings + 重合成），不报错——下次 start 生效
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	if err := Mode(context.Background(), home, "direct"); err != nil {
		t.Fatalf("Mode(not running): %v", err)
	}
	settings, err := store.LoadSettings(home)
	if err != nil || settings.Mode != "direct" {
		t.Fatalf("settings.Mode = %q err %v, want direct", settings.Mode, err)
	}
	data, err := os.ReadFile(home.RuntimeFile())
	if err != nil || !bytes.Contains(data, []byte("mode: direct")) {
		t.Fatalf("runtime.yaml = %s err %v, want synthesized with direct", data, err)
	}
}

func TestModeInvalidRejectedWithoutWrites(t *testing.T) {
	home := paths.Home(stubSocketDir(t))
	seedRuntimeInputs(t, home)
	if err := Mode(context.Background(), home, "turbo"); err == nil {
		t.Fatal("invalid mode: want error")
	}
	settings, err := store.LoadSettings(home)
	if err != nil || settings.Mode != store.DefaultMode {
		t.Fatalf("invalid mode must not touch settings; got %q err %v", settings.Mode, err)
	}
}

func TestModePatchFailureIsError(t *testing.T) {
	// PATCH 失败必须显式报错——但 settings 已落盘（崩溃重启后模式仍为新值，
	// 与 §2.1 顺序语义一致：文件先行，运行态由重试收敛）
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"version":"stub"}`))
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	home, cleanup := newStubCore(t, mux)
	defer cleanup()
	seedRuntimeInputs(t, home)

	if err := Mode(context.Background(), home, "rule"); err == nil {
		t.Fatal("PATCH failure: want error")
	}
	settings, _ := store.LoadSettings(home)
	if settings.Mode != "rule" {
		t.Fatalf("settings.Mode = %q, want rule persisted despite PATCH failure", settings.Mode)
	}
}

// seedRuntimeInputs 造可合成的最小输入（模板合成无订阅也能成功，这里给 secret 即可）。
func seedRuntimeInputs(t *testing.T, home paths.Home) {
	t.Helper()
	if _, err := store.EnsureSecret(home); err != nil {
		t.Fatal(err)
	}
}
