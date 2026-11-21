package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallFilesWritesConfigAndPlist helper install 自写锚（批⑥ §3.3）：service.json
// 派生路径/白名单/属主正确（0644），plist ProgramArguments=[helper, service] +
// KeepAlive=true + 日志落 /Library/Logs。落点全部指向临时目录——绝不触碰真实系统目录。
func TestInstallFilesWritesConfigAndPlist(t *testing.T) {
	dir := shortTempDir(t)
	configDir := filepath.Join(dir, "support")
	launchDir := filepath.Join(dir, "LaunchDaemons")
	logsDir := filepath.Join(dir, "Logs")
	dataUID := 501

	if err := installFiles(filepath.Join(dir, "data"), dataUID, configDir, launchDir, logsDir); err != nil {
		t.Fatalf("installFiles: %v", err)
	}
	dataDir := filepath.Join(dir, "data")
	cfgPath := filepath.Join(configDir, "service.json")
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatalf("stat service.json: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("service.json perm = %o, want 644", info.Mode().Perm())
	}
	var got ConfigFile
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse service.json: %v", err)
	}
	want := ConfigFile{
		AllowedUIDs: []int{dataUID},
		DataDir:     dataDir,
		CoreBin:     filepath.Join(dataDir, "bin", "mihomo"),
		Sidecar:     filepath.Join(dataDir, "bin", "mihomo.sha256"),
		Runtime:     filepath.Join(dataDir, "runtime.yaml"),
		Sock:        filepath.Join(dataDir, "core.sock"),
		Log:         filepath.Join(dataDir, "logs", "core-service.log"),
		DataUID:     dataUID,
	}
	if got.AllowedUIDs == nil || len(got.AllowedUIDs) != 1 || got.AllowedUIDs[0] != want.AllowedUIDs[0] {
		t.Fatalf("allowed_uids = %v, want [%d]", got.AllowedUIDs, dataUID)
	}
	if got.DataDir != want.DataDir || got.CoreBin != want.CoreBin || got.Sidecar != want.Sidecar ||
		got.Runtime != want.Runtime || got.Sock != want.Sock || got.Log != want.Log || got.DataUID != want.DataUID {
		t.Fatalf("service.json = %+v, want %+v", got, want)
	}

	plistPath := filepath.Join(launchDir, DaemonLabel+".plist")
	plistData, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	plist := string(plistData)
	for _, want := range []string{
		"<string>" + DaemonLabel + "</string>",
		"<string>" + HelperInstallPath + "</string>",
		"<string>service</string>",
		"<key>KeepAlive</key>",
		"<string>" + filepath.Join(logsDir, "tradewind-service.log") + "</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Fatalf("plist missing %q:\n%s", want, plist)
		}
	}
}

// TestDaemonPlistRejectsXMLChars plist 注入面防御：含 XML 特殊字符的路径拒绝
// 生成（autostart.launchdPlist 同款守卫先例）。
func TestDaemonPlistRejectsXMLChars(t *testing.T) {
	if _, err := daemonPlist("/evil<bin>", "/Library/Logs/tradewind-service.log"); err == nil {
		t.Fatal("xml-special helper path: want error")
	}
	if _, err := daemonPlist(HelperInstallPath, "/Library/Logs/a&b.log"); err == nil {
		t.Fatal("xml-special log path: want error")
	}
}
