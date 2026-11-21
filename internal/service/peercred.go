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

// darwin sys/un.h 常量与 xucred 布局（arm64/amd64 同为小端）：
//
//	struct xucred {
//	    u_int  cr_version;   // 偏移 0
//	    u_short cr_uid;      // 偏移 4（本实现取的字段）
//	    short  cr_ngroups;
//	    gid_t  cr_groups[16];
//	    void  *_cr_unused1;
//	};
const (
	solLocal      = 0x0 // SOL_LOCAL
	localPeercrd  = 0x1 // LOCAL_PEERCRED
	xucredSize    = 80  // xucred 全长（getsockopt 要求缓冲 ≥ 结构体大小）
	xucredUIDOff  = 4   // cr_uid 偏移（cr_version 之后）
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
	if length < xucredUIDOff+2 {
		return 0, fmt.Errorf("service: xucred too short (%d bytes)", length)
	}
	if version := binary.LittleEndian.Uint32(buf[0:4]); version != xucredVersion {
		return 0, fmt.Errorf("service: unexpected xucred version %d", version)
	}
	return int(binary.LittleEndian.Uint16(buf[xucredUIDOff : xucredUIDOff+2])), nil
}
