// safeopen.go：root 进程读取用户目录文件的唯一通道（批⑥b §4.1，I1）。
//
// root 对用户可写目录只有"经安全打开读取白名单文件"一种操作：O_NOFOLLOW 拒绝符号
// 链接（跟随链接读到 root 专读文件后，YAML 语法错误信息会带出内容片段；属主校验
// 同时挡住指向 root 文件的硬链接）、fstat 限定普通文件与属主、大小上限限定内存
// 占用面。单次 open+fstat+read 构成快照语义，没有"先检查后使用"的窗口。
//
// 错误信息可以含路径（用户数据目录路径不是秘密），绝不含文件内容。
package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// openSafe 以 O_NOFOLLOW 打开 path 并经 fstat 复核：必须是 wantUID 属主的普通文件，
// 否则关闭并报错。O_NONBLOCK 让 FIFO 的打开立即返回（否则阻塞到有写者），随后被
// fstat 的"必须普通文件"拦下；O_CLOEXEC 防止 fd 泄漏给子进程。
func openSafe(path string, wantUID int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("service: open %s: is a symbolic link (refusing to follow)", path)
		}
		return nil, fmt.Errorf("service: open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("service: stat %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		f.Close()
		// O_NOFOLLOW 只作用于路径末段；走到这里的非普通文件只会是 FIFO/设备/目录等
		return nil, fmt.Errorf("service: %s is not a regular file (refusing)", path)
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		f.Close()
		return nil, fmt.Errorf("service: cannot determine owner of %s", path)
	}
	if int(sys.Uid) != wantUID {
		f.Close()
		return nil, fmt.Errorf("service: %s is owned by uid %d (want %d)", path, sys.Uid, wantUID)
	}
	return f, nil
}

// readSafe 在 openSafe 之上加大小上限并整体读入（单次快照，无 TOCTOU）。
func readSafe(path string, wantUID int, maxBytes int64) ([]byte, error) {
	f, err := openSafe(path, wantUID)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("service: stat %s: %w", path, err)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("service: %s is %d bytes (limit %d)", path, info.Size(), maxBytes)
	}
	// LimitReader 上限 = maxBytes+1：stat 与 read 之间文件可能变大，读到的字节超过
	// 上限同样拒绝（同一 fd 的 stat 快照挡不住 read 期间的增长）。
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("service: read %s: %w", path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("service: %s grew past the %d byte limit while reading", path, maxBytes)
	}
	return data, nil
}

// runtimeMaxBytes 是归一化输入（用户 runtime.yaml）的字节上限（批⑥b §4.2：
// root 进程里的内存占用面——同账号进程可以递归一份大文件垫高服务内存）。
const runtimeMaxBytes = 8 << 20

// stageRuntime 是 runtimeMaxBytes 的消费方之一（见 rootstate.go）；syncGeo 的
// geoSyncMaxBytes 同理，两个上限互不对齐——字节上限是各条目之外的粗闸。
