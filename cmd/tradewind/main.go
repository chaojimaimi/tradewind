// tradewind 是 Tradewind 控制面 CLI（A-lite，stdlib 子命令分发，不引 cobra）。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/chaojimaimi/tradewind/internal/app"
	"github.com/chaojimaimi/tradewind/internal/paths"
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
  tradewind list [--json]                             list subscriptions (uid/name/type/updated/traffic/expire)
  tradewind use <UID|NAME>                            switch the current subscription (snapshot + hot reload)
  tradewind subs delete <UID|NAME> [--force]          delete a subscription (current one needs --force)
  tradewind subs interval <UID|NAME> <off|1|12|24>    per-subscription auto update interval (off disables it)
  tradewind subs rename <UID|NAME> <NEW_NAME>         rename a subscription alias
  tradewind settings [get [--json]]                   show app settings (--json single-line for shells)
  tradewind settings set <key> <value>                set mixed_port/gateway_port/panel_enabled/allow_lan/log_retention_days
  tradewind settings reset                            restore default settings (hot reload + sysproxy retarget)
  tradewind autostart on|off|status [--json]          manage the launchd LaunchAgent (starts on login, crash-restart)
  tradewind panel [--url]                             open metacubexd via the panel gateway (--url prints URL only)
  tradewind version                                   show app and core version

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
	case "list":
		err = cmdList(home, args[1:])
	case "use":
		err = cmdUse(home, args[1:])
	case "subs":
		err = cmdSubs(home, args[1:])
	case "settings":
		err = cmdSettings(home, args[1:])
	case "autostart":
		err = cmdAutostart(home, args[1:])
	case "panel":
		err = cmdPanel(home, args[1:])
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	fmt.Print(app.VersionReport(home))
	return nil
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
