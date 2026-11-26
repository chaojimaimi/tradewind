// rootstate.go：root 专属状态的三件事（批⑥b §4.1）。
//
//   - verifyCoreHomeTrust：内核工作目录 <core_home> 的信任链复核（I2）——祖先链逐级
//     root 属主且 group/other 不可写，CoreHome 自身 0700，<anchor>/logs 与 CoreLog
//     非符号链接。服务启动时与每次 spawn 前各复核一次，不达标 fail-closed。
//   - loadNormalized / writeRuntime：归一化输入与产物落盘的一对原语（c3 形态，I3
//     ——内核只加载服务归一化过的配置）：用户 runtime.yaml 经 readSafe 读取后过
//     当前归一化器，产物原子写入 <core_home>/runtime.yaml。快照生命周期（L1–L5）
//     在 snapshot.go。
//   - syncGeo：把用户目录里的 geo 数据同步进 <core_home>（best-effort，不阻断 spawn）。
package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// verifyCoreHomeTrust 复核 root 专属内核工作目录（生产实现；测试经
// Server.verifyHomeTrust 缝注入桩——夹具在 /tmp 下祖先含符号链接与全局可写目录，
// 真 Lstat 必拒，与 verifyExecTrust 的缝同理）。
func verifyCoreHomeTrust(c ConfigFile) error {
	return verifyCoreHomeTrustWith(c, lstatOwner)
}

// verifyCoreHomeTrustWith 是 verifyCoreHomeTrust 的可注入形态（与 ownership.go 的
// 既有写法同构）。
func verifyCoreHomeTrustWith(c ConfigFile, lstat func(string) (ownerStat, error)) error {
	// CoreHome 全部祖先 + 自身逐级判定，复用执行目录的同款判据（非符号链接、uid 0、
	// group/other 不可写）——祖先若用户/admin 可写，整个工作目录都能被换成符号链接。
	if c.CoreHome == "" {
		// 生产路径有 validate 挡在前面；这是库函数自己的下界——空路径会让下面的
		// 链为空
		return errors.New("service: core home is not configured")
	}
	chain := trustChain(c.CoreHome)
	coreHome := chain[len(chain)-1] // trustChain 末项是路径自身
	for _, path := range chain {
		st, err := lstat(path)
		// 复用的判据报错文案带 "exec dir entry" 前缀；包一层点明这是内核工作目录的
		// 信任链，读日志的人不必从路径里猜
		if err := checkExecDirEntry(path, st, err); err != nil {
			return fmt.Errorf("service: core home trust chain: %w", err)
		}
		if path == coreHome {
			// 自身追加两道：必须是目录；目录本身对 group/other 零权限——内核会把自己
			// 的 socket 置 0666，目录 0700 是第一道屏障，握手后压回 0600 是第二道
			//（批⑥b §4.1）。
			if !st.mode.IsDir() {
				return fmt.Errorf("service: core home %s is not a directory", path)
			}
			if st.mode.Perm()&0o077 != 0 {
				return fmt.Errorf("service: core home %s is group/other-accessible (%04o)", path, st.mode.Perm())
			}
		}
	}
	// <anchor>/logs：O_NOFOLLOW 只挡路径末段，logs 本身若成了符号链接，root 会经它
	// 向别处追加内核日志——按链上条目判（非符号链接、uid 0、group/other 不可写）再
	// 加"必须是目录"。
	logsDir := LogsDirIn(filepath.Dir(c.BrokerSock))
	st, err := lstat(logsDir)
	if err := checkExecDirEntry(logsDir, st, err); err != nil {
		return err
	}
	if !st.mode.IsDir() {
		return fmt.Errorf("service: logs dir %s is not a directory", logsDir)
	}
	// CoreLog：不存在则通过（Manager 会创建）；存在则必须是非符号链接的普通文件。
	// 不校验属主与权限位——该文件按设计属主是 data_uid，用户可改自己那份日志的内容
	// 与权限（批⑥b §4.1）。
	lst, err := lstat(c.CoreLog)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("service: core log %s: %w", c.CoreLog, err)
	case lst.mode&os.ModeSymlink != 0:
		return fmt.Errorf("service: core log %s is a symbolic link (refusing)", c.CoreLog)
	case !lst.mode.IsRegular():
		return fmt.Errorf("service: core log %s is not a regular file", c.CoreLog)
	}
	return nil
}

// normalizeGate 串行化配置归一化：全服务同一时刻最多一份在跑（容量 1 的信号量，
// 排队方不占内存）。WHY：节点数上限要等 yaml 解析完才生效，8 MiB 的敌意输入在
// 被拒之前先建出约四百万个节点（实测单次峰值约 731 MiB）；reload 的归一化在
// s.mu 之外做（R15），而每连接一个协程——不限并发的话，同一账号开 N 个连接并发
// reload 就是 N 倍内存。root 守护进程被 OOM 杀掉会连带停掉 TUN 内核，并让 launchd
// 进入节流重启（批⑥b c3b 审查 P1）。start 的归一化在 s.mu 之内、彼此本就串行，但
// 它与 reload 的那份仍然并发，所以闸放在本函数里，两条路径都由构造覆盖。
//
// 锁序恒为 s.mu → normalizeGate：闸只在 loadNormalized 内部持有、随它返回即
// 释放，绝不跨到获取 s.mu 之上——反过来会与"持 s.mu 等闸"的 start 互锁。
var normalizeGate = make(chan struct{}, 1)

// loadNormalized 读取用户 runtime.yaml（readSafe：O_NOFOLLOW + 属主 + 8 MiB 上限）
// 并过当前归一化器（c3 形态的 I3）；全程持有 normalizeGate（见上）。三种结局由
// 返回值区分：
//   - 读失败 → err != nil（服务侧失败，非配置拒绝——调用方按 {ok:false} 处理）；
//   - 归一化拒绝 → rej != nil 且 err == nil（调用方回 {ok:false, rejected:true}）；
//   - 接受 → 返回产物（Bytes = 归一化产物，Ignored = 被剥离的用户键）。
func loadNormalized(c ConfigFile) (Result, *Rejection, error) {
	normalizeGate <- struct{}{}
	defer func() { <-normalizeGate }()
	data, err := readSafe(c.UserRuntime, c.DataUID, runtimeMaxBytes)
	if err != nil {
		return Result{}, nil, fmt.Errorf("service: runtime: %w", err)
	}
	res, rej := NormalizeRuntime(data)
	if rej != nil {
		return Result{}, rej, nil
	}
	return res, nil, nil
}

// writeRuntime 把归一化产物原子写入 <core_home>/runtime.yaml（内核 -f 指向的文件，
// 服务的唯一写点）。绝不留下空文件或半成品：内核对缺失的配置会自建默认配置以
// root 跑起来（M17）。临时文件固定在 <core_home> 内（root 独占目录，I1 不涉及）。
func writeRuntime(c ConfigFile, data []byte) error {
	tmp := filepath.Join(c.CoreHome, ".runtime.tmp")
	return writeAtomic(tmp, RuntimeIn(c.CoreHome), data)
}

// writeAtomic 原子落盘：先移除可能残留的临时文件，再 O_CREATE|O_EXCL 0600 创建 →
// 写入 → fsync → rename 覆盖目标（rename 同目录内原子）。
func writeAtomic(tmp, target string, data []byte) error {
	_ = os.Remove(tmp) // 残留的半成品清掉，O_EXCL 才能创建
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", target, err)
	}
	return nil
}

// geoSyncNames 是 geo 同步的固定名单（批⑥b §4.4）：mihomo 按这些固定文件名在工作
// 目录读取 geo 数据（不带路径参数的固定写入点）。
var geoSyncNames = []string{
	"geoip.metadb", "geosite.dat", "GeoSite.dat", "GeoIP.dat", "geoip.db", "Country.mmdb", "ASN.mmdb",
}

// geoSyncMaxBytes 是单个 geo 文件的上限（官方 geoip.metadb ~10 MiB 量级，256 MiB
// 留足余量；流式拷贝不占内存）。
const geoSyncMaxBytes = 256 << 20

// syncGeo 在每次 spawn 前把用户目录里的 geo 数据同步进 <core_home>。best-effort：
// 源不存在静默跳过；源被 openSafe 拒绝或拷贝失败只打一行 stderr 后跳过，不阻断
// spawn——geo 缺失只影响内核的域名解析分流质量，不值得让整个会话起不来。
func syncGeo(c ConfigFile) {
	for _, name := range geoSyncNames {
		if err := syncOneGeo(c, name); err != nil {
			svcErrf("service: geo sync skipped %s: %v", name, err)
		}
	}
}

// syncOneGeo 同步单个 geo 文件：源（用户目录，经 openSafe）与目标（<core_home>）
// (size, mtime) 相同则跳过；否则流式拷贝到临时文件后 rename 覆盖，并把目标的
// atime/mtime 设成源的值——下次未变化才能继续跳过。
func syncOneGeo(c ConfigFile, name string) error {
	src, err := openSafe(filepath.Join(c.DataDir, name), c.DataUID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // 源不存在：静默跳过（用户没放 geo 文件是常态）
		}
		return err // 符号链接/属主不符/不是普通文件：跳过不阻断
	}
	defer src.Close()
	sinfo, err := src.Stat()
	if err != nil {
		return fmt.Errorf("stat source: %w", err)
	}
	// 单个 geo 文件上限（批⑥b §4.4）：超限跳过——上限防的是 root 进程把超大文件
	// 搬进 <core_home> 的磁盘面，不需要整体读入内存即可判
	if sinfo.Size() > geoSyncMaxBytes {
		return fmt.Errorf("source is %d bytes (limit %d)", sinfo.Size(), geoSyncMaxBytes)
	}
	target := filepath.Join(c.CoreHome, name)
	if dinfo, err := os.Lstat(target); err == nil {
		if dinfo.Mode().IsRegular() && dinfo.Size() == sinfo.Size() && dinfo.ModTime().Equal(sinfo.ModTime()) {
			return nil // (size, mtime) 相同 → 未变化，跳过
		}
		if !dinfo.Mode().IsRegular() {
			// 目录/socket 等先摘除——<core_home> root 独占 0700，异常条目不可能来自
			// 用户，摘除动作落在 root 独占目录内（I1 的"零写入"只约束用户目录）
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("remove non-regular target: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat target: %w", err)
	}
	tmp := filepath.Join(c.CoreHome, ".geo-sync.tmp")
	if err := copyStream(tmp, target, src, sinfo.Size(), sinfo.ModTime()); err != nil {
		_ = os.Remove(tmp) // 拷贝出错清理临时文件，下次重试
		return err
	}
	return nil
}

// copyStream 流式拷贝（不整体读入内存）→ fsync → rename 覆盖 → Chtimes 对齐源
// mtime（rename 会把临时文件自身的时间带过去，不对齐则下次恒判"已变化"）。
// srcSize 是调用方 stat 到的源大小：读取经 LimitReader 限在 srcSize+1，拷到的字节数
// 必须恰等于 srcSize——源在 stat 之后被追加（同账号进程做得到）时本次判失败，
// geoSyncMaxBytes 的上限因此是实的，而不是只管 stat 那一刻。
func copyStream(tmp, target string, src io.Reader, srcSize int64, srcMod time.Time) error {
	// 上一实例在拷贝中途被 kill -9 会留下半成品；不先摘掉，O_EXCL 此后恒失败，
	// 这个文件永远同步不过来
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	written, err := io.Copy(f, io.LimitReader(src, srcSize+1))
	if err != nil {
		f.Close()
		return fmt.Errorf("copy: %w", err)
	}
	if written != srcSize {
		f.Close()
		return fmt.Errorf("source changed during copy (%d of %d bytes)", written, srcSize)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("rename %s: %w", target, err)
	}
	if err := os.Chtimes(target, srcMod, srcMod); err != nil {
		return fmt.Errorf("chtimes %s: %w", target, err)
	}
	return nil
}
