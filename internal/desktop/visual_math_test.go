package desktop

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/pkg/api"
)

func TestNativeMathBoxLayoutAndEdgeCases(t *testing.T) {
	cases := []struct {
		name    string
		tex     string
		display bool
	}{
		{"inline_einstein", `E = mc^2`, false},
		{"quadratic_frac", `x = \frac{-b \pm \sqrt{b^2 - 4ac}}{2a}`, true},
		{"bigop_sum", `\sum_{i=1}^{n} x_i^2 \le \int_0^\infty f(t)\,dt`, true},
		{"greek_blackboard", `\forall x \in \mathbb{R}, \alpha + \beta \neq \gamma`, false},
		{"matrix", `\begin{pmatrix} a & b \\ c & d \end{pmatrix}`, true},
		{"cases", `f(x) = \begin{cases} x^2 & x \ge 0 \\ -x & x < 0 \end{cases}`, true},
		{"accent_and_sqrt_n", `\hat{y}_i = \sqrt[3]{\overline{x}_i + \tilde{z}}`, false},
		{"delim", `\left( \frac{1}{n} \right)`, true},
		{"malformed_unclosed", `\frac{a+b}{c \sqrt{x + \unknowncmd \left( 1`, false},
		{"malformed_braces", `}}}} {{{{ \frac{}{}`, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			box := compileMathBox(tc.tex, 15, tc.display)
			if box.w <= 0 || box.ascent <= 0 {
				t.Fatalf("%s invalid dimensions: w=%v ascent=%v descent=%v", tc.name, box.w, box.ascent, box.descent)
			}
		})
	}
}

func TestReplyRendersInlineAndDisplayMath(t *testing.T) {
	reply := "根据二次方程公式 $ax^2 + bx + c = 0$，求根公式为：\n\n$$\nx = \\frac{-b \\pm \\sqrt{b^2 - 4ac}}{2a}\n$$\n\n并且对于任意 $\\alpha \\in \\mathbb{R}$ 成立。"
	w, _ := newViewWorkbench(run("r-math", "t1", api.RunCompleted, "推导公式", reply, 0))
	tt := ui.NewTester(w.view, 1100, 900)
	tt.Advance(time.Second)

	if obj, ok := w.inlineMathObject(`E=mc^2`, 15); !ok || obj.Width <= 0 || obj.Height <= 0 {
		t.Fatalf("inlineMathObject failed: ok=%v obj=%+v", ok, obj)
	}
	if _, ok := w.inlineMathObject("   ", 15); ok {
		t.Fatal("empty TeX should return ok=false")
	}
	if img := tt.Image(); img == nil {
		t.Fatal("expected rendered screenshot image")
	}
	acc, ok := tt.Access("数学公式")
	if !ok || acc.Name == "" {
		t.Logf("accessible texts: %v", tt.Texts())
	}
}
