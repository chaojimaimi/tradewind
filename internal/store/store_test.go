package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type yamlDoc struct {
	Name string `yaml:"name"`
	Port int    `yaml:"port"`
}

func TestYAMLRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.yaml")
	if err := WriteYAML(path, yamlDoc{Name: "tw", Port: 7897}); err != nil {
		t.Fatalf("WriteYAML: %v", err)
	}
	var got yamlDoc
	if err := ReadYAML(path, &got); err != nil {
		t.Fatalf("ReadYAML: %v", err)
	}
	if got.Name != "tw" || got.Port != 7897 {
		t.Fatalf("round trip got %+v", got)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.json")
	if err := WriteJSON(path, yamlDoc{Name: "tw", Port: 1}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got yamlDoc
	if err := ReadJSON(path, &got); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if got.Port != 1 {
		t.Fatalf("round trip got %+v", got)
	}
}

func TestWritePermIs600(t *testing.T) {
	// 订阅 URL 含 token、secret 为凭据——600 是凭据最小暴露的硬性要求（DESIGN §7.4）
	dir := t.TempDir()
	for _, fn := range []func() error{
		func() error { return WriteYAML(filepath.Join(dir, "y.yaml"), yamlDoc{}) },
		func() error { return WriteJSON(filepath.Join(dir, "j.json"), yamlDoc{}) },
		func() error { return WriteFileRaw(filepath.Join(dir, "raw.bin"), []byte{1}) },
		func() error { return SaveFileRaw(filepath.Join(dir, "raw2.bin"), []byte{2}) },
	} {
		if err := fn(); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("%s perm = %o, want 600", e.Name(), perm)
		}
	}
}

func TestReadMissingFileIsNotExist(t *testing.T) {
	var out yamlDoc
	err := ReadYAML(filepath.Join(t.TempDir(), "gone.yaml"), &out)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadYAML missing err = %v, want os.ErrNotExist", err)
	}
	err = ReadJSON(filepath.Join(t.TempDir(), "gone.json"), &out)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadJSON missing err = %v, want os.ErrNotExist", err)
	}
}

func TestCorruptFilesReturnError(t *testing.T) {
	// 损坏文件必须显式报错，绝不静默返回零值（静默零值会让状态机走错分支）
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("bad.yaml", "name: [unclosed")
	write("bad.json", "{not json")
	var out yamlDoc
	if err := ReadYAML(filepath.Join(dir, "bad.yaml"), &out); err == nil {
		t.Fatal("corrupt yaml: want error")
	}
	if err := ReadJSON(filepath.Join(dir, "bad.json"), &out); err == nil {
		t.Fatal("corrupt json: want error")
	}
}

func TestWriteIsAtomicNoTempLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.yaml")
	if err := WriteYAML(path, yamlDoc{Name: "x"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "a.yaml" {
		t.Fatalf("leftover files after atomic write: %v", entries)
	}
}
