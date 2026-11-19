import CoreGraphics
import Testing
@testable import TradewindKit

/// SwitchPill：demo .switch 自绘规格常量守门（v0.4.3 壳热修，ui-demo.html L92-101）
/// + 几何自洽（knob+inset×2 ≤ 轨高，防后续调值破形）。
@Suite struct SwitchPillTests {
    @Test func constantsMatchDemoSwitchSpec() {
        // 标准（demo .switch：40×24，knob 20，checked 位移 16）
        #expect(SwitchPill.standardWidth == 40)
        #expect(SwitchPill.standardHeight == 24)
        #expect(SwitchPill.standardKnob == 20)
        #expect(SwitchPill.standardCheckedKnobOffset == 16)
        // 小号（demo .switch.sm：34×21，knob 17，checked 位移 13）
        #expect(SwitchPill.smallWidth == 34)
        #expect(SwitchPill.smallHeight == 21)
        #expect(SwitchPill.smallKnob == 17)
        #expect(SwitchPill.smallCheckedKnobOffset == 13)
        // 旋钮距轨内边（demo top/left 2px）
        #expect(SwitchPill.knobInset == 2)
    }

    @Test func knobFitsTrackInBothSizes() {
        // 几何自洽：knob + 左右 inset ≤ 轨高（两个尺寸各自成立）——
        // 未来调常量时静默破形（旋钮溢出轨道）在此报警，而非只靠真机兜底
        #expect(SwitchPill.standardKnob + SwitchPill.knobInset * 2 <= SwitchPill.standardHeight)
        #expect(SwitchPill.smallKnob + SwitchPill.knobInset * 2 <= SwitchPill.smallHeight)
        // 横向选中位守门（code-reviewer P3）：inset + 位移 + knob ≤ 轨宽——改坏 offset
        // 时"旋钮选中位溢出右缘"在此报警（标准 2+16+20=38≤40、小号 2+13+17=32≤34）
        #expect(SwitchPill.knobInset + SwitchPill.standardCheckedKnobOffset + SwitchPill.standardKnob
                    <= SwitchPill.standardWidth)
        #expect(SwitchPill.knobInset + SwitchPill.smallCheckedKnobOffset + SwitchPill.smallKnob
                    <= SwitchPill.smallWidth)
    }
}
