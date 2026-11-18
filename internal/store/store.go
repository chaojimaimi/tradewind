// Package store 负责 Tradewind 数据目录内各状态文件（yaml/json）的读写。
//
// 所有文件权限 600（订阅 URL 含 token、secret 为凭据，目录 700 + 文件 600 是凭据最小暴露的落地）。
// 写入采用 temp+rename 原子替换，避免进程崩溃留下半截文件。
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// filePerm 是所有状态文件的标准权限。
const filePerm = 0o600

// ReadYAML 读取并反序列化 yaml 文件。文件不存在时返回可被 errors.Is(err, os.ErrNotExist) 识别的错误。
func ReadYAML(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("store: read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("store: parse %s: %w", path, err)
	}
	return nil
}

// WriteYAML 序列化并原子写入 yaml 文件（600 权限）。
func WriteYAML(path string, in any) error {
	data, err := yaml.Marshal(in)
	if err != nil {
		return fmt.Errorf("store: marshal %s: %w", path, err)
	}
	return writeFileAtomic(path, data)
}

// ReadJSON 读取并反序列化 json 文件。文件不存在时返回可被 errors.Is(err, os.ErrNotExist) 识别的错误。
func ReadJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("store: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("store: parse %s: %w", path, err)
	}
	return nil
}

// WriteJSON 序列化并原子写入 json 文件（600 权限）。
func WriteJSON(path string, in any) error {
	data, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("store: marshal %s: %w", path, err)
	}
	return writeFileAtomic(path, data)
}

// writeFileAtomic 先写同目录临时文件再 rename，保证读者要么看到旧完整内容、要么看到新完整内容。
func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("store: create %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("store: write %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: close %s: %w", tmp, err)
	}
	// 显式 chmod：OpenFile 的 perm 受 umask 影响，这里强制收敛到 600
	if err := os.Chmod(tmp, filePerm); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: chmod %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: rename %s: %w", path, err)
	}
	return nil
}

// WriteFileRaw 原子写入原始字节（600 权限），用于订阅原文件等非 yaml 编码产物。
func WriteFileRaw(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", filepath.Dir(path), err)
	}
	return writeFileAtomic(path, data)
}

// SaveFileRaw 直接写入原始字节（600 权限），用于 secret 等启动期一次性生成文件。
func SaveFileRaw(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePerm)
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("store: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", path, err)
	}
	return os.Chmod(path, filePerm)
}
