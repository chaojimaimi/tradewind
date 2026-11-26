package store

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

// ProfileTypeFile / ProfileTypeRemote 是订阅条目的两种类型。
// file 型来自本地文件（无更新语义），remote 型可手动/定时更新。
const (
	ProfileTypeFile   = "file"
	ProfileTypeRemote = "remote"
)

// ExtraInfo 是订阅响应头 subscription-userinfo 解析出的流量/到期信息；无响应头时全 0。
type ExtraInfo struct {
	Upload   int64 `yaml:"upload" json:"upload"`
	Download int64 `yaml:"download" json:"download"`
	Total    int64 `yaml:"total" json:"total"`
	Expire   int64 `yaml:"expire" json:"expire"`
}

// ProxySelection 是 profiles.yaml 里单个策略组的选择记录（M1 启用，对齐 DESIGN §5.2）。
type ProxySelection struct {
	Group string `yaml:"group" json:"group"`
	Node  string `yaml:"node" json:"node"`
	// Pinned：该记录来自一个被手动固定过的自动组（URLTest/Fallback，内核 fixed 非空，
	// M16）。只有带它的记录，跨形态重放才会对自动组下 PUT——未固定的自动组重放
	// now 会把自动测速钉死在快照那一刻的节点上（批⑥b §4.8）。
	// 向后兼容：旧文件无此键 = 未固定。
	Pinned bool `yaml:"pinned,omitempty" json:"pinned,omitempty"`
}

// ProfileItem 是 profiles.yaml.items[] 的单个订阅条目。
type ProfileItem struct {
	UID string `yaml:"uid" json:"uid"`
	// Type: remote | file
	Type string `yaml:"type" json:"type"`
	Name string `yaml:"name" json:"name"`
	// File 是 profiles/ 目录下的文件名（<uid>.yaml）
	File string `yaml:"file" json:"file"`
	URL  string `yaml:"url,omitempty" json:"url,omitempty"`
	// Updated 是最近一次导入/更新的 unix 时间戳（秒）
	Updated int64 `yaml:"updated" json:"updated"`
	// 以下三个字段 M0 仅预留结构，M1 启用（allow_auto_update 为指针以便区分未设置）
	AllowAutoUpdate *bool            `yaml:"allow_auto_update,omitempty" json:"allow_auto_update,omitempty"`
	UpdateInterval  int              `yaml:"update_interval,omitempty" json:"update_interval,omitempty"`
	Selected        []ProxySelection `yaml:"selected,omitempty" json:"selected,omitempty"`

	Extra ExtraInfo `yaml:"extra" json:"extra"`
}

// Index 是 profiles.yaml：订阅索引 + 当前选中。
type Index struct {
	Current string        `yaml:"current,omitempty" json:"current,omitempty"`
	Items   []ProfileItem `yaml:"items" json:"items"`
}

// ByUID 按 uid 查找订阅条目。
func (idx *Index) ByUID(uid string) (*ProfileItem, bool) {
	for i := range idx.Items {
		if idx.Items[i].UID == uid {
			return &idx.Items[i], true
		}
	}
	return nil, false
}

// CurrentItem 返回当前选中的订阅条目。
func (idx *Index) CurrentItem() (*ProfileItem, bool) {
	return idx.ByUID(idx.Current)
}

// LoadIndex 读取订阅索引；文件不存在时返回空索引（首次导入场景）。
func LoadIndex(h paths.Home) (Index, error) {
	var idx Index
	if err := ReadYAML(h.IndexFile(), &idx); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Index{}, nil
		}
		return Index{}, err
	}
	return idx, nil
}

// SaveIndex 原子写入订阅索引。
func SaveIndex(h paths.Home, idx Index) error {
	return WriteYAML(h.IndexFile(), idx)
}

// SaveProfileFile 原子写入订阅原文件（profiles/<uid>.yaml，600）。
func SaveProfileFile(h paths.Home, file string, data []byte) error {
	return WriteFileRaw(h.ProfileFile(file), data)
}

// NewUID 生成 8 位随机 hex 订阅 uid。
// WHY crypto/rand：uid 虽非安全凭据，但避免引入 math/rand 全局种子的并发问题。
func NewUID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate uid: %w", err)
	}
	return hex.EncodeToString(buf), nil
}
