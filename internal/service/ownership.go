// ownership.go：执行目录的归属校验（batch6 真机 fold-in 2）。
//
// 特权服务以 root 执行执行目录里的内核副本，launchd 以 root 常驻拉起 helper 副本
// ——这些路径若归非 root 所有，或对 group/other 可写，任何用户都能替换它们后换取
// root 代码执行。--exec-dir 此前只拦"落在 data_dir 之内"，指向别处的用户可写目录
// 照样通过；本文件在 Install 的生产入口补上"必须归 root 所有"的判定。
package service

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ownerStat 是归属校验需要的最小 Lstat 投影：类型/权限位与属主 uid。抽成值类型
// 使判定成为纯函数——单测直接构造，不必伪造 os.FileInfo。
type ownerStat struct {
	mode os.FileMode // Lstat 语义：符号链接呈现 ModeSymlink
	uid  int
}

// lstatOwner 取条目的 Lstat 投影（不跟随符号链接）。
func lstatOwner(path string) (ownerStat, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ownerStat{}, err
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ownerStat{}, fmt.Errorf("service: cannot determine owner of %s", path)
	}
	return ownerStat{mode: info.Mode(), uid: int(sys.Uid)}, nil
}

// execDirEntries 是执行目录内须归 root 所有的五项：目录本身、bin 子目录、三个文件。
func execDirEntries(execDir string) []string {
	return []string{
		execDir,
		filepath.Join(execDir, "bin"),
		HelperPathIn(execDir),
		CorePathIn(execDir),
		SidecarPathIn(execDir),
	}
}

// checkExecDirEntry 单项归属判定（纯函数）：非符号链接、uid==0、group/other 不可写
// （mode&0o022==0）。statErr 非空 = 条目不存在/不可查，同样拒绝。
func checkExecDirEntry(path string, st ownerStat, statErr error) error {
	switch {
	case statErr != nil:
		return fmt.Errorf("service: exec dir entry %s: %w", path, statErr)
	case st.mode&os.ModeSymlink != 0:
		return fmt.Errorf("service: exec dir entry %s is a symbolic link (refusing)", path)
	case st.uid != 0:
		return fmt.Errorf("service: exec dir entry %s is owned by uid %d (must be root)", path, st.uid)
	case st.mode.Perm()&0o022 != 0:
		return fmt.Errorf("service: exec dir entry %s is group/other-writable (%04o)", path, st.mode.Perm())
	}
	return nil
}

// verifyExecDirOwnership 逐项 Lstat 并判定；任一项不达标即拒绝。
func verifyExecDirOwnership(execDir string) error {
	return verifyExecDirOwnershipWith(execDir, lstatOwner)
}

// verifyExecDirOwnershipWith 是 verifyExecDirOwnership 的可注入形态（lstat 缝隙——
// 单测用构造的归属结果走完整五项遍历，非 root 环境造不出 root 属主文件）。
func verifyExecDirOwnershipWith(execDir string, lstat func(string) (ownerStat, error)) error {
	for _, path := range execDirEntries(execDir) {
		st, err := lstat(path)
		if err := checkExecDirEntry(path, st, err); err != nil {
			return err
		}
	}
	return nil
}
