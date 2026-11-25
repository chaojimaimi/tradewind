// manager_log.go：内核日志文件的打开/轮转/读取（从 manager.go 拆出，行为不变）。
//
// 三处都带 O_NOFOLLOW / Lstat 的符号链接防线：特权服务以 root 打开日志路径，该
// 路径若被换成指向系统文件的符号链接，跟随打开 = root 往目标追加日志（batch6
// 真机 fold-in 2）；批⑥b 起日志已在 root 属主的 <anchor>/logs/ 下，防线作为
// 纵深防御原样保留。
package core

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// openLogFile 打开内核日志（O_APPEND 追加写）。
// WHY O_NOFOLLOW（batch6 真机 fold-in 2）：特权服务以 root 打开日志路径（批⑥b 起
// 是 root 属主目录下的 <anchor>/logs/core.log，纵深防御原样保留）——该路径若被换成
// 指向系统文件的符号链接，跟随打开 = root 往目标追加日志。O_NOFOLLOW 由内核拒绝
// （ELOOP），不依赖调用前的 Lstat（那有 TOCTOU 窗口）。只作用于路径末段，普通
// 文件零回归。
func openLogFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, fmt.Errorf("core: open log: %s is a symbolic link (refusing to follow)", path)
		}
		return nil, fmt.Errorf("core: open log: %w", err)
	}
	return f, nil
}

// rotateLog 超过 logRotateSize 时将日志轮转为 .1（保留 1 份，直接覆盖旧 .1）。
// Lstat 判符号链接并直接拒绝：跟随 Stat 会读到目标的大小，随后 Rename 移走的是
// 链接本身——以 root 对用户可控路径做这些动作都不该发生。
func rotateLog(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("core: stat log: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("core: log path %s is a symbolic link (refusing to rotate)", path)
	}
	if info.Size() <= logRotateSize {
		return nil
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return fmt.Errorf("core: rotate log: %w", err)
	}
	return nil
}

// tailFile 读文件末尾 n 字节。O_NOFOLLOW：日志内容会进错误信息（fail-fast /
// giveup 的 log tail）——特权服务下若跟随符号链接，root 专读文件的内容会被带进
// 返回给用户态的错误里。读不了只表现为"日志不可用"，不影响主流程。
func tailFile(path string, n int64) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	offset := info.Size() - n
	if offset < 0 {
		offset = 0
	}
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Sprintf("(log read failed: %v)", err)
	}
	return string(buf)
}
