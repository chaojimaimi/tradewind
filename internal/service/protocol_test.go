package service

// reload 动作的协议编解码测试：DecodeRequest 白名单、Client.Reload 的线上请求形态
// 与应答解码、Response 新增键（rejected/ignored）的 omitempty 两个方向。拆出自
// reload_test.go（单文件 ≤400 行）。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDecodeRequestAcceptsReload 协议层：reload 进白名单；请求零路径照旧成立
// （携带未知字段仍拒收）。
func TestDecodeRequestAcceptsReload(t *testing.T) {
	req, err := DecodeRequest([]byte(`{"action":"reload"}` + "\n"))
	if err != nil || req.Action != ActionReload {
		t.Fatalf("DecodeRequest(reload) = %+v err %v, want accepted", req, err)
	}
	if _, err := DecodeRequest([]byte(`{"action":"reload","payload":"x"}`)); err == nil {
		t.Fatal("reload must stay zero-path (unknown fields rejected)")
	}
}

// TestClientReloadRoundTrip Client.Reload：线上请求恰为 {"action":"reload"}，应答
// 的 rejected/ignored 正确解码。
func TestClientReloadRoundTrip(t *testing.T) {
	dir := shortTempDir(t)
	sock := filepath.Join(dir, "proto.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	gotLine := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			close(gotLine)
			return
		}
		gotLine <- strings.TrimSpace(line)
		fmt.Fprint(conn, `{"ok":true,"ignored":["a.b.path"]}`+"\n")
	}()

	cl := NewClient(sock, ReloadTimeout)
	resp, err := cl.Reload()
	if err != nil || !resp.OK {
		t.Fatalf("Reload = %+v err %v, want ok", resp, err)
	}
	if got, want := resp.Ignored, []string{"a.b.path"}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("ignored = %v, want %v", got, want)
	}
	select {
	case line := <-gotLine:
		if line != `{"action":"reload"}` {
			t.Fatalf("wire request = %s, want {\"action\":\"reload\"} (zero-path)", line)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stub listener never saw the reload request")
	}
}

// TestResponseJSONShape 应答编码 golden：零值不带 rejected/ignored 键；带值时两键
// 都出现（omitempty 的两个方向都要钉住）。
func TestResponseJSONShape(t *testing.T) {
	zero, err := json.Marshal(Response{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(zero), `{"ok":false}`; got != want {
		t.Fatalf("zero Response = %s, want %s (no rejected/ignored keys)", got, want)
	}
	full, err := json.Marshal(Response{OK: false, Rejected: true, Error: "e", Ignored: []string{"a.b"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(full), `{"ok":false,"error":"e","rejected":true,"ignored":["a.b"]}`; got != want {
		t.Fatalf("full Response = %s, want %s", got, want)
	}
}
