package core

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fakeCoreBin 是 TestMain 预编译的桩内核路径。
var fakeCoreBin string

// TestMain 编译一次 testdata/fakecore（子进程方式运行的桩内核，不能作为测试内函数直接监听）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "tw-core-test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: mkdir: %v\n", err)
		os.Exit(1)
	}
	fakeCoreBin = filepath.Join(dir, "fakecore")
	build := exec.Command("go", "build", "-o", fakeCoreBin, "./testdata/fakecore")
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		os.RemoveAll(dir)
		fmt.Fprintf(os.Stderr, "TestMain: build fakecore: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// fakeCoreEnv 构造以指定模式运行桩内核的子进程环境。
func fakeCoreEnv(extra ...string) []string {
	return append(os.Environ(), extra...)
}

// shortTempDir 返回短路径临时目录。
// WHY 不用 t.TempDir()：macOS unix socket sun_path 上限 104 字符，
// 其路径（含测试名）超限导致 bind: invalid argument。
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "twt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func writeTempFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubConfig 组装指向桩内核的管理器配置。
func stubConfig(t *testing.T, dir, mode string, extraEnv ...string) Config {
	t.Helper()
	return Config{
		BinPath:     fakeCoreBin,
		SidecarPath: writeSidecar(t, dir, fakeCoreBin),
		DataDir:     dir,
		RuntimePath: writeTempFile(t, dir, "runtime.yaml", "proxies: []\n"),
		SockPath:    filepath.Join(dir, "core.sock"),
		Secret:      "testsecret",
		LogPath:     filepath.Join(dir, "logs", "core.log"),
		Env:         fakeCoreEnv(append([]string{"FAKE_CORE_MODE=" + mode}, extraEnv...)...),
	}
}

// writeSidecar 为任意二进制生成匹配的 sha256 sidecar（测试桩走 sidecar 路径）。
func writeSidecar(t *testing.T, dir, binPath string) string {
	t.Helper()
	out, err := exec.Command("shasum", "-a", "256", binPath).Output()
	if err != nil {
		t.Fatalf("shasum: %v", err)
	}
	return writeTempFile(t, dir, "mihomo.sha256", string(out))
}
