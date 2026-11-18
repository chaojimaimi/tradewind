package sysproxy

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/store"
)

// Runner 抽象 networksetup 命令执行。测试注入 fake runner，绝不触碰真实系统代理。
type Runner interface {
	Run(args ...string) (stdout string, err error)
}

// SystemRunner 返回真实 networksetup 执行器。
func SystemRunner() Runner { return realRunner{} }

type realRunner struct{}

func (realRunner) Run(args ...string) (string, error) {
	cmd := exec.Command("networksetup", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("sysproxy: networksetup %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// ActiveServices 返回"启用且已连接"的网络服务（写入与备份仅针对这些服务，M0-plan §5.1 步骤 7）。
func ActiveServices(r Runner) ([]string, error) {
	out, err := r.Run("-listallnetworkservices")
	if err != nil {
		return nil, err
	}
	var active []string
	for _, svc := range ParseEnabledServices(out) {
		info, err := r.Run("-getinfo", svc)
		if err != nil {
			return nil, fmt.Errorf("sysproxy: getinfo %s: %w", svc, err)
		}
		if ParseConnected(info) {
			active = append(active, svc)
		}
	}
	return active, nil
}

// Snapshot 读取各服务的 web/secure/socks 三项代理与 bypass 列表（写入前备份）。
// 备份是精确还原的唯一依据（还原优先于设置，DESIGN §7.2）。
func Snapshot(r Runner, services []string) ([]store.ServiceBackup, error) {
	var backups []store.ServiceBackup
	for _, svc := range services {
		web, err := getProxyState(r, "-getwebproxy", svc)
		if err != nil {
			return nil, err
		}
		secure, err := getProxyState(r, "-getsecurewebproxy", svc)
		if err != nil {
			return nil, err
		}
		socks, err := getProxyState(r, "-getsocksfirewallproxy", svc)
		if err != nil {
			return nil, err
		}
		bypass, err := getBypassDomains(r, svc)
		if err != nil {
			return nil, err
		}
		backups = append(backups, store.ServiceBackup{
			Service: svc, Web: web, Secure: secure, Socks: socks, Bypass: bypass,
		})
	}
	return backups, nil
}

func getProxyState(r Runner, action, svc string) (store.ProxyState, error) {
	out, err := r.Run(action, svc)
	if err != nil {
		return store.ProxyState{}, fmt.Errorf("sysproxy: %s %s: %w", action, svc, err)
	}
	st, err := ParseProxyState(out)
	if err != nil {
		return store.ProxyState{}, fmt.Errorf("sysproxy: %s %s: %w", action, svc, err)
	}
	return st, nil
}

func getBypassDomains(r Runner, svc string) ([]string, error) {
	out, err := r.Run("-getproxybypassdomains", svc)
	if err != nil {
		return nil, fmt.Errorf("sysproxy: -getproxybypassdomains %s: %w", svc, err)
	}
	return ParseBypassDomains(out), nil
}
