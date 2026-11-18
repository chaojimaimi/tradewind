package app

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/chaojimaimi/tradewind/internal/core"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
)

// healthProbeTimeout 是 status 探测内核 /version 的超时。
const healthProbeTimeout = 2 * time.Second

// Report 是 status 概览。Zero 值安全。
type Report struct {
	Desired     string
	PID         int
	CoreVersion string
	CoreHealthy bool
	SysProxy    string
	Profile     string
	Traffic     string
}

// ExitCode 供脚本判断（M0-plan §4）：0 正常 / 1 内核异常（desired=running 但不可达）。
func (r Report) ExitCode() int {
	if r.Desired == store.DesiredRunning && !r.CoreHealthy {
		return 1
	}
	return 0
}

// Status 汇总内核 / 系统代理 / 当前订阅概览。只读，不改任何状态。
func Status(home paths.Home) (Report, error) {
	st, err := store.LoadState(home)
	if err != nil {
		return Report{}, err
	}
	r := Report{
		Desired:  st.Core.Desired,
		PID:      st.Core.PID,
		SysProxy: map[bool]string{true: "applied", false: "not applied"}[st.SysProxy.Applied],
	}
	if st.SysProxy.Applied {
		r.SysProxy = fmt.Sprintf("applied (%d services backed up)", len(st.SysProxy.Services))
	}
	r.CoreVersion, r.CoreHealthy = probeCore(home)
	r.Profile, r.Traffic = currentProfile(home)
	return r, nil
}

// probeCore 经 unix socket 探测内核健康；sock 不存在或请求失败即不健康。
func probeCore(home paths.Home) (version string, healthy bool) {
	if _, err := os.Stat(home.SockFile()); err != nil {
		return "", false
	}
	client := core.NewClient(home.SockFile(), store.ReadSecret(home))
	ctx, cancel := context.WithTimeout(context.Background(), healthProbeTimeout)
	defer cancel()
	v, err := client.Version(ctx)
	if err != nil {
		return "", false
	}
	return v, true
}

// currentProfile 返回当前订阅与流量摘要行。
func currentProfile(home paths.Home) (profile, traffic string) {
	idx, err := store.LoadIndex(home)
	if err != nil {
		return "(index unreadable)", ""
	}
	item, ok := idx.CurrentItem()
	if !ok {
		return "(none)", ""
	}
	profile = fmt.Sprintf("%s (uid %s, %s, updated %s)",
		item.Name, item.UID, item.Type, time.Unix(item.Updated, 0).Format("2006-01-02 15:04"))
	if item.Extra.Total > 0 || item.Extra.Expire > 0 {
		traffic = fmt.Sprintf("up %s / down %s / total %s",
			humanBytes(item.Extra.Upload), humanBytes(item.Extra.Download), humanBytes(item.Extra.Total))
		if item.Extra.Expire > 0 {
			traffic += ", expire " + time.Unix(item.Extra.Expire, 0).Format("2006-01-02")
		}
	}
	return profile, traffic
}

// Print 输出 status 概览。
func (r Report) Print() {
	if r.CoreHealthy {
		fmt.Printf("core:      running (pid %d, %s)\n", r.PID, r.CoreVersion)
	} else {
		fmt.Printf("core:      not reachable (desired=%s)\n", r.Desired)
	}
	fmt.Printf("sys_proxy: %s\n", r.SysProxy)
	fmt.Printf("profile:   %s\n", r.Profile)
	if r.Traffic != "" {
		fmt.Printf("traffic:   %s\n", r.Traffic)
	}
}

// humanBytes 人类可读字节数。
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
