// purge.go：隐藏子命令 `tradewind purge`——删除 root 专属内核工作目录（批⑥b
// §4.1）。卸载脚本对每个执行目录候选逐一探测 helper 副本并调用它；内核产物含
// 哈希文件名，脚本无法显式枚举，须由 Go 递归删除。
package service

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// Purge 执行隐藏子命令 `tradewind purge`：删除 root 专属内核工作目录。须 root
// （卸载脚本以 root 调用；非 root 直接报错——不靠"反正删不动"的权限位兜底，
// 与 Install / service 的 root 守卫同一纪律）。
func Purge(stdout io.Writer) error {
	if os.Geteuid() != 0 {
		return errors.New("service: purge must run as root (via `tradewind tun uninstall`)")
	}
	return purgeWith(DefaultConfigDir, os.Lstat, stdout)
}

// purgeWith 是可测形态：configDir 生产=DefaultConfigDir；lstat 生产=os.Lstat。
// 绝不触碰真实系统目录的测试经临时目录注入。
func purgeWith(configDir string, lstat func(string) (os.FileInfo, error), stdout io.Writer) error {
	// ① 先看锚：DefaultCoreHome 的"位于 DefaultConfigDir 内"是编译期常量的词法事实
	// ——锚自身若是符号链接，这个判断恒真，RemoveAll 会落到链接目标。锚不存在 =
	// 从未安装，无事可做；是符号链接或不是目录 = 环境异常，拒绝且不动任何东西。
	info, err := lstat(configDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("service: purge: lstat %s: %w", configDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("service: purge: %s is a symbolic link (refusing to purge)", configDir)
	}
	if !info.IsDir() {
		return fmt.Errorf("service: purge: %s is not a directory (refusing to purge)", configDir)
	}
	// ② 内核工作目录：不存在即无事；是符号链接则只删链接本身（链接目标绝不动——
	// 那不是我们创建的东西）。
	coreHome := CoreHomeIn(configDir)
	cinfo, err := lstat(coreHome)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stdout, "service: purge: %s absent, nothing to do\n", coreHome)
		return nil
	}
	if err != nil {
		return fmt.Errorf("service: purge: lstat %s: %w", coreHome, err)
	}
	if cinfo.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(coreHome); err != nil {
			return fmt.Errorf("service: purge: remove link %s: %w", coreHome, err)
		}
		fmt.Fprintf(stdout, "service: purge: removed symbolic link %s (target untouched)\n", coreHome)
		return nil
	}
	// ③ 是普通目录才递归删除。WHY 走 Go 而不是脚本：内核产物含哈希文件名（provider
	// 默认路径、geo、cache.db），脚本无法显式枚举；红线"禁用 rm -rf"的本意是拒绝
	// "变量拼出来的递归删除"，这里路径是编译期常量并有归属/位置守卫（①②）。
	if !cinfo.IsDir() {
		return fmt.Errorf("service: purge: %s is not a directory (refusing to purge)", coreHome)
	}
	if err := os.RemoveAll(coreHome); err != nil {
		return fmt.Errorf("service: purge: remove %s: %w", coreHome, err)
	}
	fmt.Fprintf(stdout, "service: purged %s\n", coreHome)
	return nil
}
