package service

// I1 行为级断言（root 对用户数据目录零写入）：目录快照工具 + 全流程（含 reload，
// 批⑥b §4.4）前后逐项比对。从 spawn_orchestration_test.go 拆出（单文件 ≤400 行）。

import (
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// dirSnapshot 是目录快照的单条目投影：类型/属主/权限/大小 + inode/mtime（I1 的
// 行为级断言面）。inode 与 mtime 一并记下：原地覆写会动 mtime，"写临时文件再
// rename"会换 inode——只有"条目集合与属主/权限"的话这两种写入都看不出来。
type dirSnapshot struct {
	mode  os.FileMode
	uid   int
	size  int64
	ino   uint64
	mtime int64
}

// snapshotDataDir 对目录做全量快照（路径 → 投影）。
func snapshotDataDir(t *testing.T, dir string) map[string]dirSnapshot {
	t.Helper()
	out := map[string]dirSnapshot{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		uid, ino := -1, uint64(0)
		if sys, ok := info.Sys().(*syscall.Stat_t); ok {
			uid, ino = int(sys.Uid), sys.Ino
		}
		out[path] = dirSnapshot{
			mode: info.Mode(), uid: uid, size: info.Size(),
			ino: ino, mtime: info.ModTime().UnixNano(),
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

// TestI1DataDirUntouchedByFullFlow I1 行为级断言：全流程（start → status → stop →
// 未托管 reload → start → 被内核拒收的 reload）前后对 DataDir 各做一次目录快照
// （条目名、类型、属主、权限、大小、inode、mtime），逐项相同——root 对用户数据
// 目录零写入（没有新增、没有删除、没有属主/权限变化、没有内容被重写）。数据目录
// 里放一份 geo 文件，让 geo 同步的读取路径也落在断言里。WHY 内核用 reject-config
// 模式：被拒 reload 不动数据目录是服务自己的行为（归一化拒绝形态须改用户文件，
// 那是测试本身的写入、不是服务的）。
func TestI1DataDirUntouchedByFullFlow(t *testing.T) {
	dir := shortTempDir(t)
	cfg, conf := writeServiceFixture(t, dir)
	if err := os.WriteFile(filepath.Join(conf.DataDir, "geoip.metadb"), []byte("geo-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	startStubServerTuned(t, cfg, func(s *Server, _ *trustRecorder) {
		s.coreEnv = func(c ConfigFile) []string {
			return append(coreEnvProduction(c), "FAKE_CORE_MODE=reject-config")
		}
	})
	cl := NewClient(cfg.SockPath, 30*time.Second)

	before := snapshotDataDir(t, conf.DataDir)
	if _, err := cl.Start(1); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := cl.Status(); err != nil {
		t.Fatalf("Status: %v", err)
	}
	if _, err := cl.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// 未托管 reload（L4 无推送分支）→ 仅落盘，ok:true
	if r, err := cl.Reload(); err != nil || !r.OK || r.Rejected {
		t.Fatalf("unmanaged Reload = %+v err %v, want ok", r, err)
	}
	if _, err := cl.Start(2); err != nil {
		t.Fatalf("Start(2): %v", err)
	}
	// 托管 reload：内核以 400 拒收 → rejected:true（磁盘/快照不动，数据目录更不动）
	if r, err := cl.Reload(); err != nil || r.OK || !r.Rejected {
		t.Fatalf("managed Reload under reject-config = %+v err %v, want rejected", r, err)
	}
	after := snapshotDataDir(t, conf.DataDir)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("user data dir changed across the full flow:\nbefore=%+v\nafter =%+v", before, after)
	}
}
