// tradewind 是 Tradewind 控制面 CLI（A-lite，stdlib 子命令分发，不引 cobra）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/app"
	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/service"
)

func usage() {
	fmt.Print(`tradewind — personal proxy manager (mihomo control plane)

Usage:
  tradewind add <url> | --file <path> [--name NAME]   import subscription (validated), becomes current
  tradewind update [UID|current|all]                  manual update (remote only); hot reload if running
  tradewind start [--no-sysproxy]                     foreground run; Ctrl+C for graceful exit
  tradewind stop                                      SIGTERM the running instance
  tradewind status [--json]                           core / system proxy / subscription overview (--json for shells)
  tradewind nodes [GROUP]                             list policy groups, or GROUP's nodes with last delay
  tradewind select <GROUP> <NODE>                     switch node (requires core running)
  tradewind unpin <GROUP>                             clear URLTest fixed node (resume auto selection)
  tradewind delay [GROUP]                             latency test, default group from last selection
  tradewind mode [rule|global|direct]                 show or switch outbound mode (hot-applied when running)
  tradewind sysproxy on|off                           runtime system proxy toggle (idempotent; on requires core running)
  tradewind tun on|off|status                         TUN mode via the privileged service (on installs it on first use; 45s readiness window)
  tradewind tun install|uninstall|reinstall           manage the root helper/launchd daemon explicitly
  tradewind list [--json]                             list subscriptions (uid/name/type/updated/traffic/expire)
  tradewind use <UID|NAME>                            switch the current subscription (snapshot + hot reload)
  tradewind subs delete <UID|NAME> [--force]          delete a subscription (current one needs --force)
  tradewind subs interval <UID|NAME> <off|1|12|24>    per-subscription auto update interval (off disables it)
  tradewind subs rename <UID|NAME> <NEW_NAME>         rename a subscription alias
  tradewind settings [get [--json]]                   show app settings (--json single-line for shells)
  tradewind enhance show|set|clear <slot> [--json]    enhance chain slots (global-merge|rules|proxies|groups)
  tradewind settings set <key> <value>                set mixed_port/gateway_port/panel_enabled/allow_lan/log_retention_days
  tradewind settings reset                            restore default settings (hot reload + sysproxy retarget)
  tradewind autostart on|off|status [--json]          manage the launchd LaunchAgent (starts on login, crash-restart)
  tradewind panel [--url]                             open metacubexd via the panel gateway (--url prints URL only)
  tradewind version                                   show app and core version
  tradewind version --check [--json]                  compare with the latest GitHub release
  tradewind self-update [--core] [--json]             self-upgrade the CLI (--core: bundled mihomo, auto-rollback)

Data directory defaults to ~/Library/Application Support/tradewind,
override with ` + paths.EnvHome + ` (used by tests and smoke).
`)
}

func main() {
	home := paths.Default()
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	if err := dispatch(home, args); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// dispatch 分发子命令（stdlib switch，不引 cobra）。
func dispatch(home paths.Home, args []string) error {
	var err error
	switch cmd := args[0]; cmd {
	case "add":
		err = cmdAdd(home, args[1:])
	case "update":
		err = cmdUpdate(home, args[1:])
	case "start":
		err = cmdStart(home, args[1:])
	case "stop":
		err = cmdStop(home, args[1:])
	case "status":
		err = cmdStatus(home, args[1:])
	case "nodes":
		err = cmdNodes(home, args[1:])
	case "select":
		err = cmdSelect(home, args[1:])
	case "unpin":
		err = cmdUnpin(home, args[1:])
	case "delay":
		err = cmdDelay(home, args[1:])
	case "mode":
		err = cmdMode(home, args[1:])
	case "sysproxy":
		err = cmdSysProxy(home, args[1:])
	case "tun":
		err = cmdTun(home, args[1:])
	case "list":
		err = cmdList(home, args[1:])
	case "use":
		err = cmdUse(home, args[1:])
	case "subs":
		err = cmdSubs(home, args[1:])
	case "enhance":
		err = cmdEnhance(home, args[1:])
	case "settings":
		err = cmdSettings(home, args[1:])
	case "autostart":
		err = cmdAutostart(home, args[1:])
	case "panel":
		err = cmdPanel(home, args[1:])
	case "self-update":
		err = cmdSelfUpdate(home, args[1:])
	case "service":
		// 隐藏子命令（不出 help）：root 特权服务常驻入口（launchd daemon 拉起，批⑥ §3.1）
		err = cmdService(args[1:])
	case "install":
		// 隐藏子命令（不出 help）：helper 安装子命令——提权脚本以 root 调用，
		// --data-dir/--data-uid/--exec-dir 内联 argv 传入（argv 无秘密，批⑥ §3.3）
		err = cmdServiceInstall(home, args[1:])
	case "version", "--version", "-v":
		// WHY 收编惯用 flag 形式：用户直觉输入 tradewind --version，而非子命令
		err = cmdVersion(home, args[1:])
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	return err
}

func cmdAdd(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	file := fs.String("file", "", "import from local clash yaml file")
	name := fs.String("name", "", "display name (default: file base name or url host)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src := app.AddSource{URL: fs.Arg(0), FilePath: *file, Name: *name}
	return app.Add(context.Background(), home, src)
}

func cmdUpdate(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := fs.Arg(0)
	if target == "" {
		target = "current"
	}
	return app.Update(context.Background(), home, target)
}

func cmdStart(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	noSysProxy := fs.Bool("no-sysproxy", false, "do not touch system proxy (smoke/dev isolation)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Run(context.Background(), home, app.StartOptions{ApplySysProxy: !*noSysProxy})
}

func cmdStop(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Stop(home)
}

func cmdStatus(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable single-line JSON (menubar shell control-plane source)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	r, err := app.Status(home)
	if err != nil {
		return err
	}
	if *asJSON {
		out, err := app.StatusReport(home, r)
		if err != nil {
			return err
		}
		line, err := app.MarshalStatusJSON(out)
		if err != nil {
			return err
		}
		fmt.Println(line)
	} else {
		r.Print()
	}
	if code := r.ExitCode(); code != 0 {
		// 退出码契约（M0-plan §4）：1 = 内核异常，供脚本判断；--json 下契约不变（壳以 stdout 为准）
		os.Exit(code)
	}
	return nil
}

func cmdVersion(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("version", flag.ContinueOnError)
	check := fs.Bool("check", false, "compare the current build with the latest GitHub release")
	asJSON := fs.Bool("json", false, "machine-readable single-line JSON (about pane check-update source)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !*check {
		fmt.Print(app.VersionReport(home))
		return nil
	}
	if *asJSON {
		// --json 恒退出 0 契约（batch3 红线 2）：失败也输出单行 {"error":...} 到 stdout
		fmt.Println(app.CheckUpdateJSONLine(context.Background(), home))
		return nil
	}
	_, err := app.CheckUpdate(context.Background(), home, os.Stdout)
	return err
}

func cmdSelfUpdate(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("self-update", flag.ContinueOnError)
	withCore := fs.Bool("core", false, "also upgrade the bundled mihomo core from upstream latest (auto-rollback on failed handshake)")
	asJSON := fs.Bool("json", false, "machine-readable single-line JSON (about pane self-update source)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("usage: tradewind self-update [--core] [--json]")
	}
	ctx := context.Background()
	if *asJSON {
		// --json 恒退出 0 契约（batch3 红线 2）：结果/错误均为 stdout 单行 JSON，进度走 stderr
		fmt.Println(app.SelfUpdateJSONLine(ctx, home, *withCore))
		return nil
	}
	env := app.ProductionEnv(ctx, home, os.Stdout)
	var err error
	if *withCore {
		_, err = app.SelfUpdateCore(ctx, home, env)
	} else {
		_, err = app.SelfUpdate(ctx, home, env)
	}
	return err
}

func cmdNodes(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("nodes", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Nodes(context.Background(), home, fs.Arg(0))
}

func cmdSelect(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("select", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Select(context.Background(), home, fs.Arg(0), fs.Arg(1))
}

func cmdUnpin(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("unpin", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Unpin(context.Background(), home, fs.Arg(0))
}

func cmdDelay(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("delay", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Delay(context.Background(), home, fs.Arg(0))
}

func cmdMode(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("mode", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Mode(context.Background(), home, fs.Arg(0))
}

func cmdSysProxy(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("sysproxy", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 || (fs.Arg(0) != "on" && fs.Arg(0) != "off") {
		return errors.New("usage: tradewind sysproxy on|off")
	}
	if fs.Arg(0) == "on" {
		return app.SysProxyOn(home)
	}
	return app.SysProxyOff(home)
}

// cmdTun 分发 `tradewind tun on|off|status|install|uninstall|reinstall`（批⑥ §2.2）。
func cmdTun(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("tun", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx := context.Background()
	switch fs.Arg(0) {
	case "on":
		return app.TunOn(ctx, home)
	case "off":
		return app.TunOff(ctx, home)
	case "status":
		return app.TunStatus(home)
	case "install":
		return app.TunInstall(home)
	case "uninstall":
		return app.TunUninstall(home)
	case "reinstall":
		return app.TunReinstall(home)
	default:
		return errors.New("usage: tradewind tun on|off|status|install|uninstall|reinstall")
	}
}

func cmdList(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable single-line JSON (shell subscriptions pane source)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.List(home, *asJSON)
}

func cmdUse(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("use", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return app.Use(context.Background(), home, fs.Arg(0))
}

func cmdSubs(home paths.Home, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tradewind subs delete <uid|name> [--force] | tradewind subs interval <uid|name> <off|1|12|24>" +
			" | tradewind subs rename <uid|name> <new_name>")
	}
	switch args[0] {
	case "delete":
		if len(args) < 2 {
			return errors.New("usage: tradewind subs delete <uid|name> [--force]")
		}
		// 位置参数后可跟 --force：Go flag 在首个非 flag 参数处停止，需先取 target 再解析余下 flag
		fs := flag.NewFlagSet("subs delete", flag.ContinueOnError)
		force := fs.Bool("force", false, "allow deleting the current subscription (bare-core fallback)")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: tradewind subs delete <uid|name> [--force]")
		}
		return app.SubsDelete(context.Background(), home, args[1], *force)
	case "interval":
		fs := flag.NewFlagSet("subs interval", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return errors.New("usage: tradewind subs interval <uid|name> <off|1|12|24>")
		}
		return app.SubsInterval(context.Background(), home, fs.Arg(0), fs.Arg(1))
	case "rename":
		// 无 flag 项：target/newName 位置参数直取，不经 flag.FlagSet（评审 P1-2）——
		// 顺带规避 newName 以 "-" 开头被 flag.Parse 误判 bad flag syntax 的自由文本陷阱
		if len(args) != 3 {
			return errors.New("usage: tradewind subs rename <uid|name> <new_name>")
		}
		return app.SubsRename(context.Background(), home, args[1], args[2])
	default:
		return fmt.Errorf("unknown subs command %q (valid: delete|interval|rename)", args[0])
	}
}

// cmdEnhance 分发 `enhance show|set|clear`（槽名在前、flag 在后分段解析——
// Go flag 在首个非 flag 参数处停止，同 subs delete 先例）。
// --json 契约（batch3 红线 2）：set/clear/show 恒退出 0，stdout 仅最终一行 JSON。
func cmdEnhance(home paths.Home, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: tradewind enhance show <slot> [--uid <uid>] [--json] | " +
			"enhance set <slot> --file <path|-> [--uid] [--json] | enhance clear <slot> [--json]")
	}
	if len(args) < 2 {
		return errors.New("usage: tradewind enhance show <slot> [--uid <uid>] [--json] | " +
			"enhance set <slot> --file <path|-> [--uid] [--json] | enhance clear <slot> [--json]")
	}
	slot, rest := args[1], args[2:]
	ctx := context.Background()
	switch args[0] {
	case "show":
		fs := flag.NewFlagSet("enhance show", flag.ContinueOnError)
		uid := fs.String("uid", "", "target subscription uid (default: current)")
		asJSON := fs.Bool("json", false, "machine-readable single-line JSON")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *asJSON {
			fmt.Println(app.EnhanceShowJSONContractLine(home, slot, *uid))
			return nil
		}
		return app.EnhanceShow(home, slot, *uid, os.Stdout)
	case "set":
		fs := flag.NewFlagSet("enhance set", flag.ContinueOnError)
		file := fs.String("file", "", "content file path, or '-' for stdin")
		uid := fs.String("uid", "", "target subscription uid (default: current)")
		asJSON := fs.Bool("json", false, "machine-readable single-line JSON")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *file == "" {
			return errors.New("usage: tradewind enhance set <slot> --file <path|-> [--uid] [--json]")
		}
		content, err := readEnhanceInput(*file)
		if err != nil {
			return err
		}
		if *asJSON {
			fmt.Println(app.EnhanceSetJSONLine(ctx, home, slot, *uid, content))
			return nil
		}
		return app.EnhanceSet(ctx, home, slot, *uid, content, os.Stdout)
	case "clear":
		fs := flag.NewFlagSet("enhance clear", flag.ContinueOnError)
		uid := fs.String("uid", "", "target subscription uid (default: current)")
		asJSON := fs.Bool("json", false, "machine-readable single-line JSON")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *asJSON {
			fmt.Println(app.EnhanceClearJSONLine(ctx, home, slot, *uid))
			return nil
		}
		return app.EnhanceClear(ctx, home, slot, *uid, os.Stdout)
	default:
		return fmt.Errorf("unknown enhance command %q (valid: show|set|clear)", args[0])
	}
}

// readEnhanceInput 读取槽内容（'-' = stdin；envelope 由调用方校验语法）。
func readEnhanceInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return data, nil
}

func cmdSettings(home paths.Home, args []string) error {
	// get 是默认子命令；flag 前置形态（settings --json）按 get 的 flag 解析
	if len(args) == 0 || args[0] == "get" || strings.HasPrefix(args[0], "-") {
		rest := args
		if len(args) > 0 && args[0] == "get" {
			rest = args[1:]
		}
		fs := flag.NewFlagSet("settings get", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "machine-readable single-line JSON (shell general pane source)")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: tradewind settings [get [--json] | set <key> <value> | reset]")
		}
		return app.SettingsGet(home, *asJSON)
	}
	switch args[0] {
	case "set":
		fs := flag.NewFlagSet("settings set", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 2 {
			return errors.New("usage: tradewind settings set <key> <value>")
		}
		return app.SettingsSet(context.Background(), home, fs.Arg(0), fs.Arg(1))
	case "reset":
		fs := flag.NewFlagSet("settings reset", flag.ContinueOnError)
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 {
			return errors.New("usage: tradewind settings reset")
		}
		return app.SettingsReset(context.Background(), home)
	default:
		return fmt.Errorf("unknown settings command %q (valid: get|set|reset)", args[0])
	}
}

func cmdAutostart(home paths.Home, args []string) error {
	// `autostart status --json`：动作后跟 flag，Go flag 在首个非 flag 参数处停止，需分段解析
	if len(args) > 0 && args[0] == "status" {
		fs := flag.NewFlagSet("autostart status", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "machine-readable single-line JSON (shell general pane source)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *asJSON {
			line, err := app.AutostartStatusJSON(home)
			if err != nil {
				return err
			}
			fmt.Println(line)
			return nil
		}
		return app.Autostart(home, "status")
	}
	fs := flag.NewFlagSet("autostart", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	action := fs.Arg(0)
	if action == "" {
		action = "status"
	}
	return app.Autostart(home, action)
}

func cmdPanel(home paths.Home, args []string) error {
	fs := flag.NewFlagSet("panel", flag.ContinueOnError)
	urlOnly := fs.Bool("url", false, "print the entry URL only, do not open a browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *urlOnly {
		return app.PanelURLOnly(home)
	}
	return app.Panel(home)
}

// cmdService 分发隐藏 `tradewind service`：特权服务常驻主循环（root；SIGINT/SIGTERM
// 优雅收敛）。须 root——launchd daemon 是唯一合法运行形态。
func cmdService(_ []string) error {
	return service.RunCLI()
}

// cmdServiceInstall 分发隐藏 `tradewind install`：helper 安装子命令（提权脚本调用）。
// {data_dir, data_uid, exec_dir} 内联 argv（root 无法自证调用者数据目录，评审二轮
// P2-c）；exec_dir 是脚本经执行自测选定的目录（helper 副本 + 内核 root 副本所在）。
// argv 无秘密，密码红线不涉及。
func cmdServiceInstall(_ paths.Home, args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "user data directory (absolute)")
	dataUID := fs.Int("data-uid", 0, "data directory owner uid")
	execDir := fs.String("exec-dir", "", "execution directory holding the helper and root-owned core copies (absolute)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" || *dataUID <= 0 || *execDir == "" {
		return errors.New("usage: tradewind install --data-dir <abs path> --data-uid <uid> --exec-dir <abs path>")
	}
	return service.Install(*dataDir, *dataUID, *execDir, os.Stdout)
}
