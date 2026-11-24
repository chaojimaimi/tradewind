package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rootstate（批⑥b §4.1）：<core_home> 信任链复核（I2）与 runtime 暂存。geo 同步的
// 用例在 geo_test.go。

// homeTrustFixture 组装一个"全部达标"的 v3 锚形态（路径只是词法——lstat 注入，
// 不需要真实存在），返回配置与可达的 lstat 结果表。
func homeTrustFixture() (ConfigFile, map[string]ownerStat) {
	anchor := "/Library/Application Support/Tradewind"
	coreHome := filepath.Join(anchor, "service-data")
	c := ConfigFile{
		Layout:      3,
		AllowedUIDs: []int{501},
		DataUID:     501,
		DataDir:     "/Users/someone/Library/Application Support/tradewind",
		CoreHome:    coreHome,
		KernelSock:  KernelSockIn(coreHome),
		BrokerSock:  filepath.Join(anchor, "core.sock"),
		CoreLog:     filepath.Join(anchor, "logs", CoreLogName),
	}
	table := map[string]ownerStat{}
	for _, dir := range ancestorDirs(coreHome) { // 全部祖先（含锚目录）达标
		table[dir] = rootOwned(os.ModeDir | 0o755)
	}
	table[coreHome] = rootOwned(os.ModeDir | 0o700) // 自身 0700（group/other 零权限）
	table[filepath.Join(anchor, "logs")] = rootOwned(os.ModeDir | 0o755)
	// CoreLog 缺省不入表 → lstat 报 ErrNotExist → 通过（Manager 会创建）
	return c, table
}

// TestVerifyCoreHomeTrustMatrix 表驱动（注入 lstat，仿 ownership_test.go）：链上任
// 一祖先不达标、CoreHome 自身权限过宽或不是目录、logs 异常、CoreLog 是符号链接
// ——一律拒绝；基线形态与"CoreLog 缺失/普通文件（属主随意）"通过。
func TestVerifyCoreHomeTrustMatrix(t *testing.T) {
	base, _ := homeTrustFixture()
	cases := []struct {
		name   string
		mutate func(c ConfigFile, table map[string]ownerStat)
		want   string // 空 = 必须通过
	}{
		{"baseline passes", func(ConfigFile, map[string]ownerStat) {}, ""},
		{"ancestor group-writable", func(_ ConfigFile, table map[string]ownerStat) {
			table["/Library"] = rootOwned(os.ModeDir | 0o775)
		}, "group/other-writable"},
		{"ancestor owned by a user", func(_ ConfigFile, table map[string]ownerStat) {
			table["/Library/Application Support"] = ownerStat{mode: os.ModeDir | 0o755, uid: 501}
		}, "owned by uid 501"},
		{"ancestor is a symlink", func(_ ConfigFile, table map[string]ownerStat) {
			table["/Library/Application Support"] = rootOwned(os.ModeSymlink | 0o777)
		}, "symbolic link"},
		{"core home 0755", func(_ ConfigFile, table map[string]ownerStat) {
			table[base.CoreHome] = rootOwned(os.ModeDir | 0o755)
		}, "group/other-accessible"},
		{"core home 0700 passes", func(_ ConfigFile, table map[string]ownerStat) {
			table[base.CoreHome] = rootOwned(os.ModeDir | 0o700)
		}, ""},
		{"core home is not a directory", func(_ ConfigFile, table map[string]ownerStat) {
			table[base.CoreHome] = rootOwned(0o700)
		}, "not a directory"},
		{"logs is a symlink", func(_ ConfigFile, table map[string]ownerStat) {
			table[filepath.Join(filepath.Dir(base.BrokerSock), "logs")] = rootOwned(os.ModeSymlink | 0o777)
		}, "symbolic link"},
		{"logs group-writable", func(_ ConfigFile, table map[string]ownerStat) {
			table[filepath.Join(filepath.Dir(base.BrokerSock), "logs")] = rootOwned(os.ModeDir | 0o775)
		}, "group/other-writable"},
		{"logs is not a directory", func(_ ConfigFile, table map[string]ownerStat) {
			table[filepath.Join(filepath.Dir(base.BrokerSock), "logs")] = rootOwned(0o755)
		}, "not a directory"},
		{"core log is a symlink", func(_ ConfigFile, table map[string]ownerStat) {
			table[base.CoreLog] = ownerStat{mode: os.ModeSymlink | 0o777, uid: 501}
		}, "core log .* symbolic link"},
		{"core log missing passes", func(ConfigFile, map[string]ownerStat) {}, ""},
		// 属主与权限不校验——该文件按设计属主是 data_uid，用户可改自己那份日志的内容
		// 与权限（批⑥b §4.1）
		{"core log regular file of any owner passes", func(_ ConfigFile, table map[string]ownerStat) {
			table[base.CoreLog] = ownerStat{mode: 0o666, uid: 501}
		}, ""},
		{"core log is not a regular file", func(_ ConfigFile, table map[string]ownerStat) {
			table[base.CoreLog] = rootOwned(os.ModeDir | 0o700)
		}, "not a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, table := homeTrustFixture()
			tc.mutate(c, table)
			err := verifyCoreHomeTrustWith(c, tableLstat(table))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("verifyCoreHomeTrustWith = %v, want pass", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want rejection, got pass")
			}
			if tc.want == "core log .* symbolic link" {
				if !strings.Contains(err.Error(), "core log") || !strings.Contains(err.Error(), "symbolic link") {
					t.Fatalf("err = %v, want core-log symlink refusal", err)
				}
				return
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestStageRuntimeMovesUserRuntime stageRuntime 字节级搬运（c1 形态）：目标内容 ==
// 用户文件；临时文件不残留。
func TestStageRuntimeMovesUserRuntime(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtime := filepath.Join(dataDir, "runtime.yaml")
	if err := os.WriteFile(runtime, []byte("proxies: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: coreHome, UserRuntime: runtime}

	if err := stageRuntime(c); err != nil {
		t.Fatalf("stageRuntime: %v", err)
	}
	staged, err := os.ReadFile(RuntimeIn(coreHome))
	if err != nil {
		t.Fatalf("read staged runtime: %v", err)
	}
	if string(staged) != "proxies: []\n" {
		t.Fatalf("staged runtime = %q", staged)
	}
	if _, err := os.Lstat(filepath.Join(coreHome, ".runtime.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file must not survive (err = %v)", err)
	}
	info, err := os.Stat(RuntimeIn(coreHome))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("staged runtime perm = %v err = %v, want 0600", info.Mode().Perm(), err)
	}
}

// TestStageRuntimeRefuses 缺失 / 符号链接 → 报错，绝不写空文件（M17：内核对缺失
// 配置会自建默认配置以 root 跑起来）。
func TestStageRuntimeRefuses(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	runtime := filepath.Join(dataDir, "runtime.yaml")
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: coreHome, UserRuntime: runtime}

	// 缺失
	if err := stageRuntime(c); err == nil || !strings.Contains(err.Error(), "refusing to spawn, runtime") {
		t.Fatalf("stageRuntime(missing) = %v, want refusal", err)
	}
	// 符号链接
	target := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, runtime); err != nil {
		t.Fatal(err)
	}
	if err := stageRuntime(c); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("stageRuntime(symlink) = %v, want refusal", err)
	}
	// 两种失败形态下 <core_home>/runtime.yaml 都不曾被写出
	if _, err := os.Stat(RuntimeIn(coreHome)); !os.IsNotExist(err) {
		t.Fatalf("staged runtime must not exist after refusals (err = %v)", err)
	}
}

// TestStageRuntimeMissingCoreHomeIsError CoreHome 不存在 → 暂存失败（调用方拒绝
// spawn）——信任链复核先行，这里只验证错误路径不 panic。
func TestStageRuntimeMissingCoreHomeIsError(t *testing.T) {
	dir := shortTempDir(t)
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := filepath.Join(dataDir, "runtime.yaml")
	if err := os.WriteFile(runtime, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := ConfigFile{DataDir: dataDir, DataUID: os.Getuid(), CoreHome: filepath.Join(dir, "absent"), UserRuntime: runtime}
	if err := stageRuntime(c); err == nil {
		t.Fatal("stageRuntime into a missing core home must fail")
	}
}

// TestVerifyCoreHomeTrustEmptyCoreHome CoreHome 为空 → 报错而非 panic（trustChain
// 对空路径整条跳过，链为空时取末项会越界；生产有 validate 挡在前面，这是库函数
// 自己的下界）。
func TestVerifyCoreHomeTrustEmptyCoreHome(t *testing.T) {
	c, table := homeTrustFixture()
	c.CoreHome = ""
	err := verifyCoreHomeTrustWith(c, tableLstat(table))
	if err == nil || !strings.Contains(err.Error(), "core home is not configured") {
		t.Fatalf("err = %v, want the unconfigured-core-home refusal", err)
	}
}

// TestVerifyCoreHomeTrustChainErrorNamesCoreHome 链上条目不达标时错误点明"内核工作
// 目录的信任链"——复用的判据文案带 "exec dir entry" 前缀，单独出现会误导排查。
func TestVerifyCoreHomeTrustChainErrorNamesCoreHome(t *testing.T) {
	c, table := homeTrustFixture()
	table[filepath.Dir(c.CoreHome)] = rootOwned(os.ModeDir | 0o775) // 锚目录组可写 → 链上先拒
	err := verifyCoreHomeTrustWith(c, tableLstat(table))
	if err == nil || !strings.Contains(err.Error(), "core home trust chain") {
		t.Fatalf("err = %v, want a core home trust chain error", err)
	}
}
