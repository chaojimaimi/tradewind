// fakecore 是 core 包测试用的桩内核：模仿 mihomo 的 unix socket 控制器行为。
// 仅模拟行为语义（listen 前 unlink sock、listen 后自 chmod 666、/version、
// PUT /configs），不含任何 mihomo 代码（MIT 守则②）。
//
// 行为由环境变量控制：
//
//	FAKE_CORE_MODE=normal            正常服务 /version 与 PUT /configs（默认）
//	FAKE_CORE_MODE=exit-immediately  打印 fatal 日志后立即退出（握手 fail-fast 路径）
//	FAKE_CORE_MODE=crash-after-ms    服务 N 毫秒后异常退出（崩溃重启路径）
//	FAKE_CORE_MODE=no-sock           挂起但不建 sock（握手超时路径）
//	FAKE_CORE_MODE=dump-args         把 argv 与环境变量写入 FAKE_CORE_DUMP 指定的
//	                                 文件后照常服务（服务编排测试断言内核参数形态）
//	FAKE_CORE_MODE=reject-config     /version 照常应答（握手成功），PUT /configs 一律
//	                                 400 + {"message":"fakecore: config rejected"}
//	                                 （reload 被内核拒收的路径）
//	FAKE_CORE_PUT_RECORD=<path>      每个 /configs 请求向该文件追加一行 JSON（O_APPEND）：
//	                                 {"method":...,"query":...,"body":...}——恰一行每
//	                                 请求；其余路径（如 /version）不写。reload 推送
//	                                 测试按行数与内容断言
//
// sock 路径取自命令行 -ext-ctl-unix 的值（与 mihomo 参数形态一致）。
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// putRecord 是 FAKE_CORE_PUT_RECORD 单行 JSON 的形状（字段序即文档序——struct 而非
// map，键序稳定，测试可直接前缀断言）。
type putRecord struct {
	Method string `json:"method"`
	Query  string `json:"query"`
	Body   string `json:"body"`
}

func main() {
	sock := sockPathFromArgs()
	mode := os.Getenv("FAKE_CORE_MODE")
	switch mode {
	case "exit-immediately":
		fmt.Println(`level=fatal msg="listen tcp: address already in use"`)
		os.Exit(1)
	case "no-sock":
		time.Sleep(10 * time.Minute) // 由测试方超时判定后 kill
	case "crash-after-ms":
		n, _ := strconv.Atoi(os.Getenv("FAKE_CORE_CRASH_MS"))
		serve(sock, mode)
		time.Sleep(time.Duration(n) * time.Millisecond)
		os.Exit(1) // 模拟内核崩溃（非 0 退出）
	case "dump-args":
		dumpArgsEnv(os.Getenv("FAKE_CORE_DUMP"))
		serve(sock, mode)
		waitSignal()
	default:
		serve(sock, mode)
		waitSignal()
	}
}

// dumpArgsEnv 把 argv 与全部环境变量写入 path（服务编排测试断言：内核 argv 形态、
// 环境恰好是服务传入的那几个变量）。
func dumpArgsEnv(path string) {
	if path == "" {
		return
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "argv:\n")
	for _, a := range os.Args {
		fmt.Fprintf(&b, "  %s\n", a)
	}
	fmt.Fprintf(&b, "env:\n")
	for _, e := range os.Environ() {
		fmt.Fprintf(&b, "  %s\n", e)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		fmt.Printf("fakecore: dump args: %v\n", err)
	}
}

// sockPathFromArgs 手工扫描 -ext-ctl-unix 参数（不用 flag 包：-d/-f 等未知旗标会使其报错退出）。
func sockPathFromArgs() string {
	for i, arg := range os.Args {
		if arg == "-ext-ctl-unix" && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

// serve 在 unix socket 上提供假控制器；listen 前先 unlink（模仿 mihomo 自清理残留
// sock），listen 成功后自 chmod 666（对齐 mihomo：内核建 listener 时自行放权，M1
// ——服务的 WaitReady 才把它压回收敛权限）。
func serve(sock, mode string) {
	if sock == "" {
		fmt.Println("fakecore: no -ext-ctl-unix given")
		os.Exit(2)
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		fmt.Printf("fakecore: listen: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(sock, 0o666); err != nil {
		fmt.Printf("fakecore: chmod sock: %v\n", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"meta":true,"version":"fake-v1.0.0"}`)
	})
	mux.HandleFunc("/configs", func(w http.ResponseWriter, r *http.Request) {
		// 记录先行（请求处理本身不产生分支差异）；仅 /configs 写，其它路径零写入
		recordPut(r)
		if mode == "reject-config" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"message":"fakecore: config rejected"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	go http.Serve(ln, mux)
}

// recordPut 把单个 /configs 请求追加为记录文件的一行 JSON（O_APPEND：恰一行每请求，
// 并发写互不覆盖）。记录失败只打日志不失败请求——记录是测试观测面，不是行为闸。
func recordPut(r *http.Request) {
	path := os.Getenv("FAKE_CORE_PUT_RECORD")
	if path == "" {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		fmt.Printf("fakecore: record read body: %v\n", err)
		return
	}
	line, err := json.Marshal(putRecord{Method: r.Method, Query: r.URL.RawQuery, Body: string(body)})
	if err != nil {
		fmt.Printf("fakecore: record marshal: %v\n", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Printf("fakecore: record open: %v\n", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Printf("fakecore: record write: %v\n", err)
	}
}

func waitSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
}
