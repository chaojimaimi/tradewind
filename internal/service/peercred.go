// peercred.go：macOS peer credentials（getpeereid(2) 等价实现）。
// Go 标准库在 darwin 不导出 getpeereid，也不提供 SO_PEERCRED（Linux 专属）——
// 经 SOL_LOCAL/LOCAL_PEERCRED getsockopt 取 xucred.cr_uid。白名单是特权服务
// 唯一认证闸（批⑥ §3.1）。
package service

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"
)

// darwin sys/un.h 常量与 xucred 布局（arm64/amd64 同为小端；现行 XNU 布局）：
//
//	struct xucred {
//	    u_int  cr_version;   // 偏移 0
//	    uid_t  cr_uid;       // 偏移 4（本实现取的字段；uid_t=__uint32_t——
//	                         // 旧实现误按 u_short 解析：uid>65535 误拒 +
//	                         // 低 16 位碰撞误放行，batch6 review P1-2）
//	    short  cr_ngroups;
//	    gid_t  cr_groups[16];
//	    void  *_cr_unused1;
//	};
const (
	solLocal      = 0x0 // SOL_LOCAL
	localPeercrd  = 0x1 // LOCAL_PEERCRED
	xucredSize    = 88  // xucred 全长（cr_uid 为 uid_t=4 字节后的布局；getsockopt 要求缓冲 ≥ 结构体大小）
	xucredUIDOff  = 4   // cr_uid 偏移（cr_version 之后）
	xucredUIDSize = 4   // cr_uid 宽度（uid_t=__uint32_t）
	xucredVersion = 0   // XUCRED_VERSION
)

// peerUID 返回连接对端的 effective uid。
func peerUID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("service: peer cred: conn is not a unix conn")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, fmt.Errorf("service: peer cred: syscall conn: %w", err)
	}
	var uid int
	var ctlErr error
	if err := raw.Control(func(fd uintptr) {
		uid, ctlErr = peerUIDFD(int(fd))
	}); err != nil {
		return 0, fmt.Errorf("service: peer cred: control: %w", err)
	}
	if ctlErr != nil {
		return 0, ctlErr
	}
	return uid, nil
}

// peerUIDFD 在原始 fd 上执行 getsockopt(SOL_LOCAL, LOCAL_PEERCRED)。
func peerUIDFD(fd int) (int, error) {
	buf := make([]byte, xucredSize)
	length := uint32(len(buf))
	_, _, errno := syscall.Syscall6(syscall.SYS_GETSOCKOPT,
		uintptr(fd), uintptr(solLocal), uintptr(localPeercrd),
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&length)), 0)
	if errno != 0 {
		return 0, fmt.Errorf("service: getsockopt LOCAL_PEERCRED: %w", errno)
	}
	// 防御性钳制：内核返回长度异常大于缓冲时按缓冲截取（避免 buf[:length] 越界）
	if length > uint32(len(buf)) {
		length = uint32(len(buf))
	}
	return xucredUID(buf[:length])
}

// xucredUID 从 getsockopt 缓冲解析 cr_uid（uid_t=__uint32_t；可注入缓冲单测——
// 真连接只返回本进程 uid，>65535 的解析差异无法在非 root 测试中端到端覆盖）。
func xucredUID(buf []byte) (int, error) {
	if len(buf) < xucredUIDOff+xucredUIDSize {
		return 0, fmt.Errorf("service: xucred too short (%d bytes)", len(buf))
	}
	if version := binary.LittleEndian.Uint32(buf[0:4]); version != xucredVersion {
		return 0, fmt.Errorf("service: unexpected xucred version %d", version)
	}
	return int(binary.LittleEndian.Uint32(buf[xucredUIDOff : xucredUIDOff+xucredUIDSize])), nil
}
