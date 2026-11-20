package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/chaojimaimi/tradewind/internal/paths"
	"github.com/chaojimaimi/tradewind/internal/store"
	"github.com/chaojimaimi/tradewind/internal/synth"
	"gopkg.in/yaml.v3"
)

// 增强链槽名（batch3 §3.1；CLI enhance <slot> 契约）。
const (
	EnhanceSlotGlobal  = "global-merge"
	EnhanceSlotRules   = "rules"
	EnhanceSlotProxies = "proxies"
	EnhanceSlotGroups  = "groups"
)

// enhanceSlots 是合法槽名全集（错误信息用）。
var enhanceSlots = []string{EnhanceSlotGlobal, EnhanceSlotRules, EnhanceSlotProxies, EnhanceSlotGroups}

// mihomoCheckTimeout 是单次 `mihomo -t` 语义校验的上限（壳侧 65s write 档之内）。
const mihomoCheckTimeout = 30 * time.Second

// enhanceSlotPath 解析槽名到文件路径与归属 uid（global-merge 忽略 uid；
// 三槽 uid 缺省取 current，--uid 传入时校验存在）。未写盘的槽可能不存在——
// 路径解析只依赖索引，不触碰文件。
func enhanceSlotPath(home paths.Home, slot, uid string) (string, string, error) {
	switch slot {
	case EnhanceSlotGlobal:
		return home.EnhanceGlobalMerge(), "", nil
	case EnhanceSlotRules, EnhanceSlotProxies, EnhanceSlotGroups:
		resolved := uid
		if resolved == "" {
			idx, err := store.LoadIndex(home)
			if err != nil {
				return "", "", err
			}
			item, ok := idx.CurrentItem()
			if !ok {
				return "", "", errors.New("app: 无当前订阅（--uid 指定或先 tradewind add）")
			}
			resolved = item.UID
		} else {
			idx, err := store.LoadIndex(home)
			if err != nil {
				return "", "", err
			}
			item, err := resolveProfile(idx, resolved)
			if err != nil {
				return "", "", err
			}
			resolved = item.UID
		}
		return home.EnhanceSlotFile(resolved, slot), resolved, nil
	default:
		return "", "", fmt.Errorf("app: unknown enhance slot %q (valid: %s)", slot, strings.Join(enhanceSlots, "|"))
	}
}

// loadEnhanceLayers 读增强链槽文件（缺文件=槽空转；uid 为 current 订阅 uid）。
// 供 Synthesize 与语义校验候选合成共用（F4 唯一挂点经由 app.Synthesize）。
func loadEnhanceLayers(home paths.Home, uid string) (*synth.Enhance, error) {
	enh := &synth.Enhance{}
	read := func(path string, dst *[]byte) error {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("app: read enhance %s: %w", path, err)
		}
		*dst = data
		return nil
	}
	if err := read(home.EnhanceGlobalMerge(), &enh.GlobalMerge); err != nil {
		return nil, err
	}
	if uid != "" {
		if err := read(home.EnhanceSlotFile(uid, EnhanceSlotRules), &enh.Rules); err != nil {
			return nil, err
		}
		if err := read(home.EnhanceSlotFile(uid, EnhanceSlotProxies), &enh.Proxies); err != nil {
			return nil, err
		}
		if err := read(home.EnhanceSlotFile(uid, EnhanceSlotGroups), &enh.Groups); err != nil {
			return nil, err
		}
	}
	return enh, nil
}

// enhancePrecheck 写盘前预检（batch3 §3.3）：yaml 语法 + 槽形（三槽=顶层序列、
// global-merge=顶层映射）；报错不落盘。
func enhancePrecheck(slot string, content []byte) error {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return fmt.Errorf("app: enhance %s yaml 语法错误（未落盘）: %w", slot, err)
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return nil // 空文档=清除语义，放行
		}
		node = node.Content[0]
	}
	if node.Kind == 0 || (node.Kind == yaml.ScalarNode && node.Tag == "!!null") {
		return nil // 纯注释/null：空转，放行
	}
	wantMapping := slot == EnhanceSlotGlobal
	if wantMapping && node.Kind != yaml.MappingNode {
		return fmt.Errorf("app: enhance %s 顶层应为映射（key: value），got %v（未落盘）", slot, node.Kind)
	}
	if !wantMapping && node.Kind != yaml.SequenceNode {
		return fmt.Errorf("app: enhance %s 顶层应为序列（- 开头条目），got %v（未落盘）", slot, node.Kind)
	}
	return nil
}

// runMihomoCheck 运行 `<bin> -t -f <config> -d <dir>` 只读校验（var 缝隙供测试注入；
// 只读不落盘，batch3 §3.2）。
var runMihomoCheck = func(binPath, configPath, dataDir string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), mihomoCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "-t", "-f", configPath, "-d", dataDir)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("mihomo -t exited: %w", err)
	}
	return out.String(), nil
}

// verifyEnhanceCandidate 语义校验（评审 P1-2，仅挂 enhance set/clear 编排）：
// 合成候选字节（uid=校验目标订阅，code-reviewer P2-2）→ 共享校验核 checkCandidateBytes。
// 二进制缺失（裸机/未安装）跳过不阻断（best-effort，跳过说明经 progress 显式输出）。
func verifyEnhanceCandidate(ctx context.Context, home paths.Home, progress io.Writer, uid string) error {
	if _, err := os.Stat(home.Bin()); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(progress, "mihomo binary missing; skip semantic validation (best-effort)")
		return nil
	}
	candidate, err := synthesizeCandidate(ctx, home, uid)
	if err != nil {
		return err
	}
	if err := checkCandidateBytes(home, candidate); err != nil {
		return fmt.Errorf("app: enhance 语义校验未通过（mihomo -t），槽已回滚: %w", err)
	}
	return nil
}

// composeCandidate 合成候选字节（批④ §2.2 共享核，Use/update 预检与 enhance 校验
// 复用同一实现，不另起第三份）：settings + index + 该 uid 增强层 + synth.Synth。
// profileBytes 非 nil 时直接采用（Use/update 预检——目标字节尚未落盘/勿读旧盘内容），
// nil 时按 uid 从索引与磁盘读取（enhance 校验既有路径）。uid 空（global-merge 槽
// 无解析目标）退 current 订阅 + 仅全局层——全局层作用于所有订阅，对 current 合成
// 即代表校验其全局效果。
func composeCandidate(home paths.Home, uid string, profileBytes []byte) ([]byte, error) {
	settings, err := store.LoadSettings(home)
	if err != nil {
		return nil, err
	}
	idx, err := store.LoadIndex(home)
	if err != nil {
		return nil, err
	}
	profileUID := uid
	if profileUID == "" {
		profileUID = idx.Current
	}
	sub := profileBytes
	if sub == nil {
		if item, ok := idx.ByUID(profileUID); ok {
			sub, err = os.ReadFile(home.ProfileFile(item.File))
			if err != nil {
				return nil, fmt.Errorf("app: read profile %s: %w", item.File, err)
			}
		}
	}
	enh, err := loadEnhanceLayers(home, profileUID)
	if err != nil {
		return nil, err
	}
	var warnings []string
	return synth.Synth(sub, synth.Injection{Settings: settings, Secret: store.ReadSecret(home), Warnings: &warnings}, enh)
}

// checkCandidateBytes 校验候选字节（批④ §2.2 共享核）：写数据目录内临时文件
// （0600，内容含 secret 与节点，不依赖 TMPDIR 权限假设）→ `mihomo -t` → 输出中
// 候选路径脱敏为 <candidate>（不泄露内部路径）。二进制缺失（裸机/未安装）跳过
// 不阻断（返回 nil，best-effort；调用方如需显式跳过说明自行 stat 前置）。
func checkCandidateBytes(home paths.Home, candidate []byte) error {
	if _, err := os.Stat(home.Bin()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	tmp := home.Join("enhance-candidate.yaml")
	if err := store.WriteFileRaw(tmp, candidate); err != nil {
		return err
	}
	defer os.Remove(tmp)
	out, err := runMihomoCheck(home.Bin(), tmp, home.String())
	if err != nil {
		detail := strings.ReplaceAll(strings.TrimSpace(out), tmp, "<candidate>")
		if detail == "" {
			detail = err.Error()
		}
		return errors.New(detail)
	}
	return nil
}

// precheckCandidateBytes Use/update 前语义预检（批④ §2.2，收口批③ F6 登记的
// -t 绕过窗口）：候选合成失败或 mihomo -t 未通过 → 显式报错"已保持当前状态不变"，
// 调用方在任何落盘（index/runtime/snapshot/订阅文件）之前中止。
func precheckCandidateBytes(home paths.Home, uid string, profileBytes []byte) error {
	candidate, err := composeCandidate(home, uid, profileBytes)
	if err != nil {
		return fmt.Errorf("app: 目标订阅合成失败，已保持当前状态不变: %w", err)
	}
	if err := checkCandidateBytes(home, candidate); err != nil {
		return fmt.Errorf("app: 目标订阅合成后未通过 mihomo -t 校验，已保持当前状态不变: %w", err)
	}
	return nil
}

// synthesizeCandidate 合成候选字节（uid 订阅 + 该 uid 增强层 + 注入；校验专用，
// 不落 runtime）。批④ §2.2 改薄包装：组合逻辑下沉 composeCandidate 共享核。
func synthesizeCandidate(ctx context.Context, home paths.Home, uid string) ([]byte, error) {
	return composeCandidate(home, uid, nil)
}

// EnhanceShow 编排 `tradewind enhance show <slot>` 人读形态（--json 由 CLI 层走
// EnhanceShowJSONContractLine）。
func EnhanceShow(home paths.Home, slot, uid string, progress io.Writer) error {
	line, err := EnhanceShowJSONLine(home, slot, uid)
	if err != nil {
		return err
	}
	var report enhanceShowJSON
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		return err
	}
	if !report.Exists {
		fmt.Fprintf(progress, "slot %s: empty（缺文件=槽空转）\n", report.Slot)
		return nil
	}
	fmt.Fprintf(progress, "slot %s (uid %s):\n%s", report.Slot, report.UID, report.Content)
	return nil
}

// enhanceShowJSON 是 `enhance show --json` 的输出结构。
type enhanceShowJSON struct {
	Slot    string `json:"slot"`
	UID     string `json:"uid"`
	Exists  bool   `json:"exists"`
	Content string `json:"content"`
}

// EnhanceShowJSONLine 返回 show 的单行 JSON（恒退出 0 契约：失败 {"error":...}）。
func EnhanceShowJSONLine(home paths.Home, slot, uid string) (string, error) {
	path, resolved, err := enhanceSlotPath(home, slot, uid)
	if err != nil {
		return "", err
	}
	report := enhanceShowJSON{Slot: slot, UID: resolved}
	data, readErr := os.ReadFile(path)
	switch {
	case readErr == nil:
		report.Exists = true
		report.Content = string(data)
	case errors.Is(readErr, os.ErrNotExist):
		report.Exists = false // 空转态不是错误
	default:
		return "", fmt.Errorf("app: read enhance slot: %w", readErr)
	}
	line, err := json.Marshal(report)
	if err != nil {
		return "", fmt.Errorf("app: marshal enhance show json: %w", err)
	}
	return string(line), nil
}

// EnhanceSet 编排 `tradewind enhance set <slot> --file <path|->`（持 mutation.lock）：
// 读内容 → yaml 预检（语法错误报错不落盘）→ 写 600 → 语义校验（失败回滚槽旧内容）
// → Synthesize → ReloadIfRunning（batch3 §3.3）。
func EnhanceSet(ctx context.Context, home paths.Home, slot, uid string, content []byte, progress io.Writer) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	if err := enhancePrecheck(slot, content); err != nil {
		return err
	}
	path, resolvedUID, err := enhanceSlotPath(home, slot, uid)
	if err != nil {
		return err
	}
	old, hasOld := readSlotForRollback(path)
	if err := store.WriteFileRaw(path, content); err != nil {
		return err
	}
	fmt.Fprintf(progress, "enhance %s written (%d bytes); validating with mihomo -t...\n", slot, len(content))
	// 校验对象=目标 uid 的订阅+增强层（code-reviewer P2-2：--uid 指向非当前订阅时
	// 验 current 会漏验刚写入的槽，且可能被 current 的坏层误拦回滚）
	if err := verifyEnhanceCandidate(ctx, home, progress, resolvedUID); err != nil {
		restoreSlot(path, old, hasOld) // 校验失败：本次编排回滚槽文件旧内容
		return err
	}
	return reloadAfterEnhance(ctx, home, progress, slot, "updated")
}

// EnhanceClear 编排 `tradewind enhance clear <slot>`（持 mutation.lock）：写 0 字节
// 即清除 → 重合成（校验链同 set，失败回滚旧内容，batch3 §3.3）。
func EnhanceClear(ctx context.Context, home paths.Home, slot, uid string, progress io.Writer) error {
	lock, err := AcquireMutationLock(home)
	if err != nil {
		return err
	}
	defer lock.Release()
	path, resolvedUID, err := enhanceSlotPath(home, slot, uid)
	if err != nil {
		return err
	}
	old, hasOld := readSlotForRollback(path)
	if err := store.WriteFileRaw(path, nil); err != nil {
		return err
	}
	fmt.Fprintf(progress, "enhance %s cleared; validating with mihomo -t...\n", slot)
	if err := verifyEnhanceCandidate(ctx, home, progress, resolvedUID); err != nil {
		restoreSlot(path, old, hasOld)
		return err
	}
	return reloadAfterEnhance(ctx, home, progress, slot, "cleared")
}

// reloadAfterEnhance 校验通过后的收尾：重合成 + 热重载（未运行则仅落盘）。
// WHY 显式合成保留 + PreSynthesized 变体（批④ P3-7）：本函数的显式 Synthesize 是
// 内核未运行分支唯一的 runtime.yaml 落盘点（F14），收敛双实现到 PreSynthesized。
func reloadAfterEnhance(ctx context.Context, home paths.Home, progress io.Writer, slot, action string) error {
	secret, err := store.EnsureSecret(home)
	if err != nil {
		return err
	}
	if err := Synthesize(home, secret); err != nil {
		return err
	}
	if err := ReloadIfRunningPreSynthesizedW(ctx, home, progress); err != nil {
		return err
	}
	fmt.Fprintf(progress, "enhance %s %s; config reloaded\n", slot, action)
	return nil
}

// readSlotForRollback/restoreSlot 槽旧值在手（set/clear 场景回滚依据，batch3 §3.2）。
func readSlotForRollback(path string) ([]byte, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

func restoreSlot(path string, old []byte, hasOld bool) {
	if !hasOld {
		_ = os.Remove(path) // 原本不存在：删除本次写入
		return
	}
	_ = store.WriteFileRaw(path, old)
}

// --- --json 契约（恒退出 0：stdout 仅最终一行；进度一律 stderr）---

// EnhanceSetJSONLine 是 `enhance set --json` 的最终输出行（{"ok":true} / {"error":...}）。
func EnhanceSetJSONLine(ctx context.Context, home paths.Home, slot, uid string, content []byte) string {
	if err := EnhanceSet(ctx, home, slot, uid, content, os.Stderr); err != nil {
		return jsonErrorLine(err)
	}
	return `{"ok":true}`
}

// EnhanceClearJSONLine 是 `enhance clear --json` 的最终输出行（契约同 set）。
func EnhanceClearJSONLine(ctx context.Context, home paths.Home, slot, uid string) string {
	if err := EnhanceClear(ctx, home, slot, uid, os.Stderr); err != nil {
		return jsonErrorLine(err)
	}
	return `{"ok":true}`
}

// EnhanceShowJSONContractLine 是 `enhance show --json` 的最终输出行（恒退出 0）。
func EnhanceShowJSONContractLine(home paths.Home, slot, uid string) string {
	line, err := EnhanceShowJSONLine(home, slot, uid)
	if err != nil {
		return jsonErrorLine(err)
	}
	return line
}
