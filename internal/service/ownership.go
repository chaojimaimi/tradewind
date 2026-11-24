// ownership.go：执行目录的信任链校验（batch6 真机 fold-in 2；review P2 扩到全部祖先）。
//
// 特权服务以 root 执行执行目录里的内核副本，launchd 以 root 常驻拉起 helper 副本
// ——这些路径若归非 root 所有，或对 group/other 可写，任何用户都能替换它们后换取
// root 代码执行。只看执行目录自身不够：祖先目录若用户/admin 可写（如 Intel
// Homebrew 的 /usr/local 属主是用户），整个执行目录都能被换成符号链接，root 下次
// 经链接解析执行任意二进制。故信任链从 / 逐级向下到执行目录，每级都必须归 root
// 所有且对 group/other 不可写。
//
// 三个使用点：Install（root，落盘前）、service.Run 与每次 spawn（root，运行期
// 复核）、CLI 生成安装脚本前过滤候选（用户态 stat）。
package service

import (
	"errors"
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

// checkExecDirEntry 单项链上条目的归属判定（纯函数）：非符号链接、uid==0、
// group/other 不可写（mode&0o022==0）。statErr 非空 = 条目不存在/不可查，同样拒绝。
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

// --- 信任链（纯函数：只做字符串推导，便于单测）---

// ancestorDirs 返回 path 的全部祖先目录，从 / 逐级向下到其直接父目录（不含 path
// 自身）。相对路径向上收到 "." 为止。
func ancestorDirs(path string) []string {
	var upward []string // 自下而上
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		upward = append(upward, dir)
		if parent := filepath.Dir(dir); parent == dir {
			break // 到根（绝对路径为 /，相对路径为 .）
		}
	}
	for i, j := 0, len(upward)-1; i < j; i, j = i+1, j-1 {
		upward[i], upward[j] = upward[j], upward[i]
	}
	return upward
}

// trustChain 把一组可执行文件路径展开成信任链：每个路径的全部祖先目录（从 / 逐级
// 向下）再接路径自身；多条路径共享的祖先只保留首次出现的位置（自上而下的次序
// 因此成立）。空路径整条跳过——既不出现在链上，也不贡献任何祖先（否则会把 "."
// 即进程当前目录塞进链里）。
func trustChain(paths ...string) []string {
	var chain []string
	seen := make(map[string]bool)
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		chain = append(chain, p)
	}
	for _, file := range paths {
		if file == "" {
			continue
		}
		for _, dir := range ancestorDirs(file) {
			add(dir)
		}
		add(file)
	}
	return chain
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

// execDirChain 执行目录的完整信任链：/ → … → 执行目录 → helper 副本 → bin →
// 内核副本 → 校验文件。
func execDirChain(execDir string) []string {
	return trustChain(HelperPathIn(execDir), CorePathIn(execDir), SidecarPathIn(execDir))
}

// anchorChain 锚所指可执行路径的信任链：helper_path、core_bin、sidecar 三者及其
// 全部祖先（按锚里记录的实际路径展开，不假定制式布局）。
func anchorChain(c ConfigFile) []string {
	return trustChain(c.HelperPath, c.CoreBin, c.Sidecar)
}

// verifyTrustChainWith 逐项 Lstat 并判定（严格：链上任一条目缺失即拒）；首个不
// 达标即拒并短路。
func verifyTrustChainWith(chain []string, lstat func(string) (ownerStat, error)) error {
	for _, path := range chain {
		st, err := lstat(path)
		if err := checkExecDirEntry(path, st, err); err != nil {
			return err
		}
	}
	return nil
}

// --- 使用点一：Install（root，落盘前）---

// verifyExecDirOwnership 复核执行目录的完整信任链归 root 所有。
func verifyExecDirOwnership(execDir string) error {
	return verifyExecDirOwnershipWith(execDir, lstatOwner)
}

// verifyExecDirOwnershipWith 是 verifyExecDirOwnership 的可注入形态（lstat 缝隙——
// 非 root 环境造不出 root 属主文件）。
func verifyExecDirOwnershipWith(execDir string, lstat func(string) (ownerStat, error)) error {
	return verifyTrustChainWith(execDirChain(execDir), lstat)
}

// --- 使用点二：服务运行期复核（root）---

// verifyAnchorExecTrust 服务启动时与每次 spawn 前的复核：锚所指的 helper 副本、
// 内核副本、校验文件，连同它们各自的全部祖先目录，必须仍归 root 所有。
func verifyAnchorExecTrust(c ConfigFile) error {
	return verifyAnchorExecTrustWith(c, lstatOwner)
}

// verifyAnchorExecTrustWith 是 verifyAnchorExecTrust 的可注入形态。
func verifyAnchorExecTrustWith(c ConfigFile, lstat func(string) (ownerStat, error)) error {
	return verifyTrustChainWith(anchorChain(c), lstat)
}

// --- 使用点三：CLI 生成安装脚本前过滤候选（用户态 stat）---

// ExecDirCandidateRejection 是一个执行目录候选被排除的原因。
type ExecDirCandidateRejection struct {
	Candidate string
	Reason    error
}

// SafeExecDirCandidates 过滤出信任链安全的执行目录候选（保持原序）。自上而下
// 逐级判定每个候选的祖先目录与候选自身：已存在的须是非符号链接、root 属主、
// group/other 不可写；遇到尚不存在的一级即视为通过——其下各级必然都还不存在，
// 将由 root 以 umask 022 新建，安全与否取决于最近的已存在祖先（已在前面判过）。
// 不安全的候选连同原因一并返回，供调用方在"一个不剩"时说明。
func SafeExecDirCandidates() (safe []string, rejected []ExecDirCandidateRejection) {
	return safeExecDirCandidatesWith(ExecDirCandidates(), lstatOwner)
}

// safeExecDirCandidatesWith 是 SafeExecDirCandidates 的纯函数形态。
func safeExecDirCandidatesWith(cands []string, lstat func(string) (ownerStat, error)) (safe []string, rejected []ExecDirCandidateRejection) {
	for _, cand := range cands {
		if err := checkExistingChain(trustChain(cand), lstat); err != nil {
			rejected = append(rejected, ExecDirCandidateRejection{Candidate: cand, Reason: err})
			continue
		}
		safe = append(safe, cand)
	}
	return safe, rejected
}

// checkExistingChain 只判信任链上已存在的条目：遇尚不存在的一级即停（其下必然
// 都不存在）。
func checkExistingChain(chain []string, lstat func(string) (ownerStat, error)) error {
	for _, path := range chain {
		st, err := lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err := checkExecDirEntry(path, st, err); err != nil {
			return err
		}
	}
	return nil
}
