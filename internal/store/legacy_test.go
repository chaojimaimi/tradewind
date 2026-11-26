// legacy_test.go：批⑥b §4.8 新字段的向后兼容——旧 profiles.yaml（无 pinned）与旧
// state.json（无 last_form）反序列化不报错、新字段取零值；写回时 omitempty 不产生
// 多余键（旧形态的产物逐字节不变）。
package store

import (
	"os"
	"strings"
	"testing"

	"github.com/chaojimaimi/tradewind/internal/paths"
)

func TestLegacyProfilesAndStateLoadCleanly(t *testing.T) {
	home := paths.Home(t.TempDir())

	// 旧 profiles.yaml：selected 无 pinned 键
	legacyProfiles := `current: old01
items:
    - uid: old01
      type: file
      name: old
      file: old01.yaml
      updated: 1700000000
      selected:
        - group: 组A
          node: 节点1
`
	if err := os.WriteFile(home.IndexFile(), []byte(legacyProfiles), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := LoadIndex(home)
	if err != nil {
		t.Fatalf("LoadIndex legacy: %v", err)
	}
	item, ok := idx.CurrentItem()
	if !ok || len(item.Selected) != 1 {
		t.Fatalf("legacy index = %+v, want one current item with one selection", idx)
	}
	if item.Selected[0].Group != "组A" || item.Selected[0].Node != "节点1" || item.Selected[0].Pinned {
		t.Fatalf("legacy selection = %+v, want values kept and pinned zero", item.Selected[0])
	}
	if err := SaveIndex(home, idx); err != nil {
		t.Fatalf("SaveIndex: %v", err)
	}
	savedProfiles, err := os.ReadFile(home.IndexFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(savedProfiles), "pinned") {
		t.Fatalf("resaved profiles.yaml must not gain a pinned key:\n%s", savedProfiles)
	}
	// 逐字节回到旧形态产物（pinned 不出现在序列化里）
	wantProfiles := `current: old01
items:
    - uid: old01
      type: file
      name: old
      file: old01.yaml
      updated: 1700000000
      selected:
        - group: 组A
          node: 节点1
      extra:
        upload: 0
        download: 0
        total: 0
        expire: 0
`
	if string(savedProfiles) != wantProfiles {
		t.Fatalf("resaved profiles.yaml differs from the legacy shape:\ngot:\n%s\nwant:\n%s", savedProfiles, wantProfiles)
	}

	// 旧 state.json：顶层无 last_form 键
	legacyState := `{"core":{"desired":"stopped","pid":0,"generation":2},"sys_proxy":{"applied":false}}`
	if err := os.WriteFile(home.StateFile(), []byte(legacyState), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(home)
	if err != nil {
		t.Fatalf("LoadState legacy: %v", err)
	}
	if st.LastForm != "" || st.Core.Generation != 2 {
		t.Fatalf("legacy state = %+v, want values kept and last_form zero", st)
	}
	if err := SaveState(home, st); err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	savedState, err := os.ReadFile(home.StateFile())
	if err != nil {
		t.Fatal(err)
	}
	wantState := `{"core":{"desired":"stopped","pid":0,"generation":2},"sys_proxy":{"applied":false,"services":null}}`
	if string(savedState) != wantState {
		t.Fatalf("resaved state.json = %s, want %s (byte-identical to the legacy shape)", savedState, wantState)
	}
	if strings.Contains(string(savedState), "last_form") {
		t.Fatalf("resaved state.json must not gain a last_form key:\n%s", savedState)
	}
}
