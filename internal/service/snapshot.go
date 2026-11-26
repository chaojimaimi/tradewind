// snapshot.go：归一化产物快照与其生命周期规则（批⑥b §4.4 L1–L5，I3——特权内核
// 只加载服务归一化过的配置）。
//
// 记号：snapshot = 服务内存里"最近一份归一化产物"（nil = 无）；R = <core_home>/runtime.yaml。
//
//   - L1 种子（seedSnapshot）：Run 启动时把磁盘上的 R 读回、过当前归一化器复验后
//     采纳——种子不是被"信任"的，服务升级收紧策略对旧产物同样生效；拒绝/缺失/
//     不安全 → snapshot = nil。
//   - L5 PreSpawn（restoreRuntime）：每次 spawn（初始 spawn 与 Monitor 自愈 respawn
//     共用）前核对 R == snapshot，漂移即按快照回写；snapshot 为 nil → 拒绝 spawn
//     （内核对缺失配置会自建默认配置以 root 跑起来，M17；绝不写空文件）。
//   - promote 先、落盘后（commitRuntime 的固定次序）：两步之间若自愈 respawn 插入，
//     它看到"新快照、旧磁盘"会把磁盘向前收敛，与随后 reload 自己的落盘结果一致；
//     反过来（先落盘、后提升）它会把磁盘改回旧值，留下一段漂移。
//
// 锁纪律：快照有独立的 snapMu。restoreRuntime 只取 snapMu（start 路径调用它时调用
// 方已持 s.mu，Monitor 协程则不持——在已持 s.mu 的调用方里再取 s.mu 会自死锁）。
// 取锁顺序恒为 s.mu → snapMu。
package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/chaojimaimi/tradewind/internal/core"
)

// logLineMaxBytes 是自由文本进日志/错误响应前的字节上限（防超长刷屏；截断不劈开
// UTF-8 字符）。
const logLineMaxBytes = 512

// setSnapshot 记录一份归一化产物（存私有副本——调用方的字节随时可能被复用或改写）。
func (s *Server) setSnapshot(data []byte) {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	s.snapshot = append([]byte(nil), data...)
}

// snapshotCopy 返回快照的副本（空/无快照返回 nil）。
func (s *Server) snapshotCopy() []byte {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	if len(s.snapshot) == 0 {
		return nil
	}
	return append([]byte(nil), s.snapshot...)
}

// commitRuntime 提交一份归一化产物：提升快照（先）+ 落盘 R（后）——次序见文件头。
func (s *Server) commitRuntime(c ConfigFile, data []byte) error {
	s.setSnapshot(data)
	return writeRuntime(c, data)
}

// seedSnapshot 是 L1 种子：Run 启动（先于任何请求协程与孤儿收编）时执行。R 按
// safeopen 语义读回（属主须为服务自身 euid、普通文件、大小上限——种子与用户输入
// 过同一道闸），采纳前必须过当前归一化器；拒绝/缺失/不安全一律 snapshot = nil 并
// 记一行日志。产物与磁盘字节不同则 best-effort 覆盖 R（归一化幂等，正常不发生；
// 失败只记日志——快照已持有产物，下次 spawn 的 L5 会重试补齐）。
func (s *Server) seedSnapshot(c ConfigFile) {
	data, err := readSafe(RuntimeIn(c.CoreHome), os.Geteuid(), runtimeMaxBytes)
	if errors.Is(err, os.ErrNotExist) {
		svcOutf("service: no runtime.yaml in core home yet; config snapshot stays empty until the first start")
		return
	}
	if err != nil {
		svcErrf("service: runtime seed skipped: %v", err)
		return
	}
	res, rej := NormalizeRuntime(data)
	if rej != nil {
		svcErrf("service: runtime seed skipped: %s", rejectionText(rej))
		return
	}
	s.setSnapshot(res.Bytes)
	if !bytes.Equal(data, res.Bytes) {
		if werr := writeRuntime(c, res.Bytes); werr != nil {
			svcErrf("service: runtime seed rewrite failed: %v", werr)
			return
		}
		svcErrf("service: runtime seed rewrote disk (%d -> %d bytes)", len(data), len(res.Bytes))
	}
}

// restoreRuntime 是 L5（PreSpawn 钩子，初始 spawn 与 Monitor 自愈 respawn 共用）：
// snapshot 为 nil → 返回错误拒绝 spawn（绝不写空文件，M17）；否则读回 R 与快照
// 比对，不一致或读不回（只有 root 能造成）即按快照重写。只取 snapMu：取完副本
// 立即释放，文件 I/O 不持锁。
//
// WHY 经 readSafe 而不是裸 ReadFile：读不回的一切形态——缺失、被换成非常规文件、
// 属主不符、超长——都该落到"按快照重写"这同一个结局。裸 ReadFile 碰到 FIFO 会
// 无限阻塞，而这条路径在 start 时是持着 s.mu 跑的：一次阻塞就是整个服务卡死。
// readSafe 带 O_NONBLOCK 并要求普通文件，FIFO 当场判错。
func (s *Server) restoreRuntime(c ConfigFile) error {
	snap := s.snapshotCopy()
	if snap == nil {
		return errors.New("service: refusing to spawn, runtime: no normalized snapshot on record")
	}
	cur, err := readSafe(RuntimeIn(c.CoreHome), os.Geteuid(), runtimeMaxBytes)
	if err == nil && bytes.Equal(cur, snap) {
		return nil
	}
	if err := writeRuntime(c, snap); err != nil {
		return err
	}
	// 一行日志、只带字节计数（配置内容绝不进日志）
	svcErrf("service: runtime restored from snapshot (disk had %d bytes, snapshot %d bytes)",
		len(cur), len(snap))
	return nil
}

// preSpawnHook 返回挂到 core.Config.PreSpawn 的钩子闭包：两道信任链复核 + L5
// restoreRuntime。独立成方法让测试可以直接驱动钩子（Monitor 自愈的 respawn 在
// Manager 内部走同一份闭包，不经 spawnCore）。
func (s *Server) preSpawnHook() func() error {
	return func() error {
		if err := s.verifyExecTrust(s.conf); err != nil {
			return err
		}
		if err := s.verifyHomeTrust(s.conf); err != nil {
			return err
		}
		return s.restoreRuntime(s.conf)
	}
}

// rejectionText 归一化拒绝的用户面文案。Key/Reason 由归一化器保证只含键名/固定
// 原因且有界，这里再过一遍 sanitizeForLog 作纵深防御（同一文本进日志与响应）。
func rejectionText(rej *Rejection) string {
	return "service: 配置被拒绝：" + sanitizeForLog(rej.Key) + "（" + sanitizeForLog(rej.Reason) + "）"
}

// kernelRejectMessage 从内核 400 响应体提取 {"message":...}（非 JSON 时回退原文）
// 并压成单行有界文本。响应体是内核自由文本，进日志/响应前必须过闸。
func kernelRejectMessage(se *core.StatusError) string {
	msg := se.Body
	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(se.Body), &parsed); err == nil && parsed.Message != "" {
		msg = parsed.Message
	}
	return sanitizeForLog(msg)
}

// sanitizeForLog 把自由文本压成单行可打印形态：isEchoable 的否集一律替换 '?'，唯一
// 例外是换行/回车折成空格。WHY 与 normalize.go sanitizeName 同一道闸：双向覆盖符、
// 零宽字符、行/段分隔符能骗过"一行一条"的阅读与 grep，控制字符之外的这些非打印
// 字符同样不该原样进日志；512 字节封顶且不劈开 UTF-8 字符。
func sanitizeForLog(s string) string {
	var b strings.Builder
	size := 0
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r':
			r = ' '
		case !isEchoable(r):
			r = '?'
		}
		n := utf8.RuneLen(r)
		if size+n > logLineMaxBytes {
			break // 封顶：整段在字符边界截断
		}
		b.WriteRune(r)
		size += n
	}
	return b.String()
}
