package service

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// rootstate（批⑥b §4.1）：<core_home> 信任链复核（I2）与 loadNormalized/writeRuntime
// 一对原语。geo 同步的用例在 geo_test.go。

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

// loadWriteFixture 组装 loadNormalized / writeRuntime 的最小夹具：data 与
// service-data 两个目录，属主 = 当前用户（非 root 下 readSafe 的属主校验才过）。
func loadWriteFixture(t *testing.T, dir string) ConfigFile {
	t.Helper()
	dataDir := filepath.Join(dir, "data")
	coreHome := filepath.Join(dir, "service-data")
	for _, d := range []string{dataDir, coreHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ConfigFile{
		DataDir:     dataDir,
		DataUID:     os.Getuid(),
		CoreHome:    coreHome,
		UserRuntime: filepath.Join(dataDir, "runtime.yaml"),
	}
}

// TestLoadNormalizedAccepted 接受形态：Bytes == 归一化器的产物，Ignored 原样带出
// （provider path 被剥离并记入 Ignored）。
func TestLoadNormalizedAccepted(t *testing.T) {
	dir := shortTempDir(t)
	c := loadWriteFixture(t, dir)
	user := []byte("proxies: []\n")
	if err := os.WriteFile(c.UserRuntime, user, 0o600); err != nil {
		t.Fatal(err)
	}
	res, rej, err := loadNormalized(c)
	if err != nil || rej != nil {
		t.Fatalf("loadNormalized = rej %+v err %v, want accept", rej, err)
	}
	want, _ := NormalizeRuntime(user)
	if string(res.Bytes) != string(want.Bytes) {
		t.Fatalf("Bytes = %q, want the normalizer's product %q", res.Bytes, want.Bytes)
	}

	// Ignored 携带：provider 的 path 被剥离
	provider := []byte("proxy-providers:\n  sub1:\n    type: http\n    url: https://provider.example.invalid/subscription\n    path: ./sub1.yaml\n")
	if err := os.WriteFile(c.UserRuntime, provider, 0o600); err != nil {
		t.Fatal(err)
	}
	res, rej, err = loadNormalized(c)
	if err != nil || rej != nil {
		t.Fatalf("loadNormalized(provider) = rej %+v err %v, want accept", rej, err)
	}
	if got, wantIgnored := res.Ignored, []string{"proxy-providers.sub1.path"}; len(got) != 1 || got[0] != wantIgnored[0] {
		t.Fatalf("Ignored = %v, want %v", got, wantIgnored)
	}
}

// TestLoadNormalizedRejected 拒绝形态：rej != nil 且 err == nil（配置问题与服务侧
// 失败分开表达），Key 指名被拒键。
func TestLoadNormalizedRejected(t *testing.T) {
	dir := shortTempDir(t)
	c := loadWriteFixture(t, dir)
	if err := os.WriteFile(c.UserRuntime, []byte("proxies: []\nexternal-ui: /tmp/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, rej, err := loadNormalized(c)
	if rej == nil || err != nil {
		t.Fatalf("loadNormalized = rej %+v err %v, want rejection with nil error", rej, err)
	}
	if rej.Key != "external-ui" {
		t.Fatalf("Key = %q, want the rejected key", rej.Key)
	}
	if res.Bytes != nil {
		t.Fatalf("Bytes = %q, want empty on rejection", res.Bytes)
	}
}

// TestLoadNormalizedMissing 用户文件缺失 → err != nil 且文案带 "runtime"（服务侧
// 失败，非配置拒绝）。
func TestLoadNormalizedMissing(t *testing.T) {
	dir := shortTempDir(t)
	c := loadWriteFixture(t, dir)
	_, rej, err := loadNormalized(c)
	if err == nil || rej != nil {
		t.Fatalf("loadNormalized(missing) = rej %+v err %v, want error", rej, err)
	}
	if !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("err = %v, want it to name the runtime read", err)
	}
}

// TestLoadNormalizedSymlink 用户文件是符号链接 → readSafe 的 O_NOFOLLOW 闸拒绝。
func TestLoadNormalizedSymlink(t *testing.T) {
	dir := shortTempDir(t)
	c := loadWriteFixture(t, dir)
	target := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("proxies: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, c.UserRuntime); err != nil {
		t.Fatal(err)
	}
	_, rej, err := loadNormalized(c)
	if err == nil || rej != nil {
		t.Fatalf("loadNormalized(symlink) = rej %+v err %v, want error", rej, err)
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
}

// TestWriteRuntime 落盘形态：内容/权限 0600/属主 = euid/临时文件不残留/覆盖既有
// 文件；CoreHome 缺失 → 失败（调用方拒绝 spawn），绝不留半成品。
func TestWriteRuntime(t *testing.T) {
	dir := shortTempDir(t)
	c := loadWriteFixture(t, dir)
	first := []byte("first: product\n")
	if err := writeRuntime(c, first); err != nil {
		t.Fatalf("writeRuntime: %v", err)
	}
	staged, err := os.ReadFile(RuntimeIn(c.CoreHome))
	if err != nil || string(staged) != string(first) {
		t.Fatalf("runtime.yaml = %q err %v, want the written bytes", staged, err)
	}
	info, err := os.Stat(RuntimeIn(c.CoreHome))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %v err = %v, want 0600", info.Mode().Perm(), err)
	}
	if sys, ok := info.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Geteuid() {
		t.Fatalf("owner uid = %v, want euid %d", sys, os.Geteuid())
	}
	if _, err := os.Lstat(filepath.Join(c.CoreHome, ".runtime.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file must not survive (err = %v)", err)
	}
	// 覆盖既有文件（rename 语义：第二次写入完整替换第一次）
	second := []byte("second: product\n")
	if err := writeRuntime(c, second); err != nil {
		t.Fatalf("writeRuntime(second): %v", err)
	}
	staged, err = os.ReadFile(RuntimeIn(c.CoreHome))
	if err != nil || string(staged) != string(second) {
		t.Fatalf("runtime.yaml after rewrite = %q err %v, want fully replaced", staged, err)
	}

	// CoreHome 缺失 → 失败且无半成品
	absent := c
	absent.CoreHome = filepath.Join(dir, "missing-home")
	if err := writeRuntime(absent, second); err == nil {
		t.Fatal("writeRuntime into a missing core home must fail")
	}
	if _, err := os.Stat(RuntimeIn(absent.CoreHome)); !os.IsNotExist(err) {
		t.Fatalf("no target file may appear behind a failed write (err = %v)", err)
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
