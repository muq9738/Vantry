package desktop

import (
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

// Native L1 TeX math tokenizer, AST parser, box layout engine, and GPU painter.
// It renders both inline math ($...$) directly on paragraph baselines via
// markdown.Options.InlineMath and display blocks ($$...$$) with zero WebView.

type mathTokKind int

const (
	mTokEOF mathTokKind = iota
	mTokCmd
	mTokLBrace
	mTokRBrace
	mTokSup
	mTokSub
	mTokAmp
	mTokNewline
	mTokNum
	mTokIdent
	mTokOp
	mTokSpace
)

type mathTok struct {
	kind mathTokKind
	text string
	em   float32
}

var mathSymbols = map[string]string{
	// Lowercase Greek
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ϵ", "varepsilon": "ε",
	"zeta": "ζ", "eta": "η", "theta": "θ", "vartheta": "ϑ", "iota": "ι", "kappa": "κ",
	"varkappa": "ϰ", "lambda": "λ", "mu": "μ", "nu": "ν", "xi": "ξ", "omicron": "ο",
	"pi": "π", "varpi": "ϖ", "rho": "ρ", "varrho": "ϱ", "sigma": "σ", "varsigma": "ς",
	"tau": "τ", "upsilon": "υ", "phi": "ϕ", "varphi": "φ", "chi": "χ", "psi": "ψ", "omega": "ω",
	// Uppercase Greek
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ",
	"Pi": "Π", "Sigma": "Σ", "Upsilon": "Υ", "Phi": "Φ", "Psi": "Ψ", "Omega": "Ω",
	// Relations & operators
	"pm": "±", "mp": "∓", "times": "×", "div": "÷", "cdot": "·", "cdots": "⋯",
	"ldots": "…", "dots": "…", "vdots": "⋮", "ddots": "⋱",
	"circ": "∘", "bullet": "•", "star": "⋆", "ast": "∗", "oplus": "⊕", "otimes": "⊗",
	"leq": "≤", "le": "≤", "geq": "≥", "ge": "≥", "neq": "≠", "ne": "≠",
	"approx": "≈", "equiv": "≡", "sim": "∼", "simeq": "≃", "cong": "≅", "propto": "∝",
	"ll": "≪", "gg": "≫", "prec": "≺", "succ": "≻", "preceq": "⪯", "succeq": "⪰",
	"infty": "∞", "partial": "∂", "nabla": "∇", "hbar": "ℏ", "ell": "ℓ", "Re": "ℜ", "Im": "ℑ", "aleph": "ℵ",
	"forall": "∀", "exists": "∃", "nexists": "∄", "neg": "¬", "lnot": "¬", "land": "∧", "lor": "∨",
	"in": "∈", "notin": "∉", "ni": "∋", "subset": "⊂", "subseteq": "⊆", "supset": "⊃", "supseteq": "⊇",
	"cup": "∪", "cap": "∩", "setminus": "∖", "emptyset": "∅", "varnothing": "∅",
	"to": "→", "rightarrow": "→", "leftarrow": "←", "leftrightarrow": "↔",
	"Rightarrow": "⇒", "Leftarrow": "⇐", "Leftrightarrow": "⇔",
	"implies": "⟹", "iff": "⟺", "mapsto": "↦", "uparrow": "↑", "downarrow": "↓",
	"langle": "⟨", "rangle": "⟩", "lceil": "⌈", "rceil": "⌉", "lfloor": "⌊", "rfloor": "⌋",
	"angle": "∠", "perp": "⊥", "parallel": "∥", "prime": "′", "degree": "°", "mid": "∣",
	"quad": "  ", "qquad": "    ",
}

var mathBigOps = map[string]string{
	"sum": "∑", "prod": "∏", "coprod": "∐", "int": "∫", "iint": "∬", "iiint": "∭", "oint": "∮",
	"bigcup": "⋃", "bigcap": "⋂", "bigoplus": "⨁", "bigotimes": "⨂",
}

var mathFunctions = map[string]bool{
	"sin": true, "cos": true, "tan": true, "cot": true, "sec": true, "csc": true,
	"arcsin": true, "arccos": true, "arctan": true, "sinh": true, "cosh": true, "tanh": true,
	"log": true, "ln": true, "lg": true, "exp": true, "lim": true, "limsup": true, "liminf": true,
	"max": true, "min": true, "sup": true, "inf": true, "arg": true, "det": true, "dim": true,
	"gcd": true, "lcm": true, "ker": true, "Pr": true, "deg": true, "mod": true, "bmod": true,
	"rank": true, "tr": true, "trace": true, "diag": true, "argmin": true, "argmax": true,
}

var mathBlackboard = map[rune]string{
	'R': "ℝ", 'N': "ℕ", 'Z': "ℤ", 'Q': "ℚ", 'C': "ℂ", 'E': "𝔼", 'P': "ℙ", 'F': "𝔽", 'H': "ℍ", '1': "𝟙",
}

func lexMath(src string) []mathTok {
	var out []mathTok
	i := 0
	for i < len(src) {
		ch := src[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			i++
			continue
		}
		switch ch {
		case '{':
			out = append(out, mathTok{kind: mTokLBrace, text: "{"})
			i++
		case '}':
			out = append(out, mathTok{kind: mTokRBrace, text: "}"})
			i++
		case '^':
			out = append(out, mathTok{kind: mTokSup, text: "^"})
			i++
		case '_':
			out = append(out, mathTok{kind: mTokSub, text: "_"})
			i++
		case '&':
			out = append(out, mathTok{kind: mTokAmp, text: "&"})
			i++
		case '\\':
			i++
			if i >= len(src) {
				break
			}
			next := src[i]
			switch next {
			case '\\':
				out = append(out, mathTok{kind: mTokNewline, text: `\\`})
				i++
			case ',':
				out = append(out, mathTok{kind: mTokSpace, em: 0.16})
				i++
			case ':':
				out = append(out, mathTok{kind: mTokSpace, em: 0.22})
				i++
			case ';':
				out = append(out, mathTok{kind: mTokSpace, em: 0.28})
				i++
			case '!':
				out = append(out, mathTok{kind: mTokSpace, em: -0.10})
				i++
			case ' ', '{', '}', '%', '$', '&', '_', '#', '|':
				sym := string(next)
				if next == '|' {
					sym = "‖"
				}
				out = append(out, mathTok{kind: mTokOp, text: sym})
				i++
			default:
				start := i
				for i < len(src) && ((src[i] >= 'a' && src[i] <= 'z') || (src[i] >= 'A' && src[i] <= 'Z')) {
					i++
				}
				if i == start {
					r, w := utf8.DecodeRuneInString(src[i:])
					out = append(out, mathTok{kind: mTokOp, text: string(r)})
					i += w
				} else {
					cmd := src[start:i]
					switch cmd {
					case "quad":
						out = append(out, mathTok{kind: mTokSpace, em: 1.0})
					case "qquad":
						out = append(out, mathTok{kind: mTokSpace, em: 2.0})
					case "displaystyle", "textstyle", "scriptstyle", "limits", "nolimits":
						// formatting hint handled gently
					default:
						out = append(out, mathTok{kind: mTokCmd, text: cmd})
					}
				}
			}
		default:
			r, w := utf8.DecodeRuneInString(src[i:])
			switch {
			case unicode.IsDigit(r):
				start := i
				for i < len(src) {
					d, dw := utf8.DecodeRuneInString(src[i:])
					if !unicode.IsDigit(d) && d != '.' {
						break
					}
					i += dw
				}
				out = append(out, mathTok{kind: mTokNum, text: src[start:i]})
			case unicode.IsLetter(r):
				out = append(out, mathTok{kind: mTokIdent, text: string(r)})
				i += w
			default:
				txt := string(r)
				if r == '-' {
					txt = "−"
				} else if r == '*' {
					txt = "∗"
				}
				out = append(out, mathTok{kind: mTokOp, text: txt})
				i += w
			}
		}
	}
	out = append(out, mathTok{kind: mTokEOF})
	return out
}

type mathNode interface{ isMathNode() }

type mSeq struct{ items []mathNode }
type mText struct {
	text    string
	italic  bool
	bold    bool
	upright bool
	bigOp   bool
	padded  bool
}
type mSpace struct{ em float32 }
type mFrac struct{ num, den mathNode }
type mSqrt struct{ index, body mathNode }
type mScript struct {
	base, sub, sup mathNode
	limits         bool
}
type mAccent struct {
	kind string
	body mathNode
}
type mDelim struct {
	left, right string
	body        mathNode
}
type mMatrix struct {
	env  string
	rows [][]mathNode
}

func (mSeq) isMathNode()    {}
func (mText) isMathNode()   {}
func (mSpace) isMathNode()  {}
func (mFrac) isMathNode()   {}
func (mSqrt) isMathNode()   {}
func (mScript) isMathNode() {}
func (mAccent) isMathNode() {}
func (mDelim) isMathNode()  {}
func (mMatrix) isMathNode() {}

type mathParser struct {
	toks  []mathTok
	pos   int
	depth int
}

func (p *mathParser) peek() mathTok {
	if p.pos >= len(p.toks) {
		return mathTok{kind: mTokEOF}
	}
	return p.toks[p.pos]
}

func (p *mathParser) next() mathTok {
	t := p.peek()
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func parseMathAST(tex string) mathNode {
	p := &mathParser{toks: lexMath(tex)}
	return p.parseSeq(false, "")
}

func (p *mathParser) parseSeq(stopAtRBrace bool, stopEndEnv string) mathNode {
	if p.depth > 48 {
		return mSeq{}
	}
	p.depth++
	defer func() { p.depth-- }()

	var items []mathNode
	for {
		t := p.peek()
		if t.kind == mTokEOF {
			break
		}
		if stopAtRBrace && t.kind == mTokRBrace {
			p.next()
			break
		}
		if stopEndEnv != "" && (t.kind == mTokAmp || t.kind == mTokNewline) {
			break
		}
		if t.kind == mTokCmd && (t.text == "right" || t.text == "end") {
			break
		}
		node := p.parseScripted()
		if node != nil {
			items = append(items, node)
		} else if t.kind == mTokRBrace {
			// stray closing brace in malformed TeX: consume so loop terminates
			p.next()
		}
	}
	return mSeq{items: items}
}

func (p *mathParser) parseGroupText() string {
	t := p.peek()
	if t.kind == mTokLBrace {
		p.next()
		var sb strings.Builder
		depth := 1
		for {
			cur := p.next()
			if cur.kind == mTokEOF {
				break
			}
			if cur.kind == mTokLBrace {
				depth++
			} else if cur.kind == mTokRBrace {
				depth--
				if depth == 0 {
					break
				}
			} else if cur.kind == mTokSpace {
				sb.WriteByte(' ')
			} else {
				if sb.Len() > 0 && cur.kind == mTokCmd {
					sb.WriteByte(' ')
				}
				sb.WriteString(cur.text)
			}
		}
		return sb.String()
	}
	cur := p.next()
	return cur.text
}

func (p *mathParser) parseScripted() mathNode {
	base := p.parseAtom()
	if base == nil {
		return nil
	}
	var sub, sup mathNode
	for {
		t := p.peek()
		if t.kind == mTokSub && sub == nil {
			p.next()
			sub = p.parseAtom()
			continue
		}
		if t.kind == mTokSup && sup == nil {
			p.next()
			sup = p.parseAtom()
			continue
		}
		break
	}
	if sub == nil && sup == nil {
		return base
	}
	limits := false
	if txt, ok := base.(mText); ok {
		limits = txt.bigOp || txt.text == "lim" || txt.text == "max" || txt.text == "min" || txt.text == "sup" || txt.text == "inf" || txt.text == "argmax" || txt.text == "argmin"
	}
	return mScript{base: base, sub: sub, sup: sup, limits: limits}
}

func (p *mathParser) parseAtom() mathNode {
	t := p.peek()
	switch t.kind {
	case mTokEOF, mTokRBrace, mTokAmp, mTokNewline:
		return nil
	case mTokLBrace:
		p.next()
		return p.parseSeq(true, "")
	case mTokSpace:
		p.next()
		return mSpace{em: t.em}
	case mTokNum:
		p.next()
		return mText{text: t.text, upright: true}
	case mTokIdent:
		p.next()
		return mText{text: t.text, italic: true}
	case mTokOp, mTokSup, mTokSub:
		p.next()
		padded := t.text == "=" || t.text == "+" || t.text == "−" || t.text == "<" || t.text == ">"
		return mText{text: t.text, upright: true, padded: padded}
	case mTokCmd:
		p.next()
		switch t.text {
		case "frac", "dfrac", "tfrac":
			num := p.parseAtom()
			den := p.parseAtom()
			if num == nil {
				num = mText{text: " "}
			}
			if den == nil {
				den = mText{text: " "}
			}
			return mFrac{num: num, den: den}
		case "sqrt":
			var idx mathNode
			if p.peek().kind == mTokOp && p.peek().text == "[" {
				p.next()
				var items []mathNode
				for {
					cur := p.peek()
					if cur.kind == mTokEOF || (cur.kind == mTokOp && cur.text == "]") {
						if cur.kind == mTokOp {
							p.next()
						}
						break
					}
					it := p.parseScripted()
					if it != nil {
						items = append(items, it)
					} else {
						p.next()
					}
				}
				idx = mSeq{items: items}
			}
			body := p.parseAtom()
			if body == nil {
				body = mText{text: " "}
			}
			return mSqrt{index: idx, body: body}
		case "hat", "tilde", "bar", "vec", "dot", "ddot", "overline", "underline":
			body := p.parseAtom()
			if body == nil {
				body = mText{text: " "}
			}
			return mAccent{kind: t.text, body: body}
		case "left":
			l := p.parseDelimToken()
			body := p.parseSeq(false, "right")
			r := ""
			if p.peek().kind == mTokCmd && p.peek().text == "right" {
				p.next()
				r = p.parseDelimToken()
			}
			return mDelim{left: l, right: r, body: body}
		case "right":
			return nil
		case "begin":
			env := strings.TrimSpace(p.parseGroupText())
			return p.parseEnvironment(env)
		case "end":
			_ = p.parseGroupText()
			return nil
		case "text", "textrm", "mathrm", "operatorname", "mbox":
			return mText{text: p.parseGroupText(), upright: true}
		case "textbf", "mathbf", "boldsymbol":
			return mText{text: p.parseGroupText(), bold: true, upright: true}
		case "textit", "mathit":
			return mText{text: p.parseGroupText(), italic: true}
		case "mathbb":
			raw := p.parseGroupText()
			var sb strings.Builder
			for _, r := range raw {
				if mapped, ok := mathBlackboard[r]; ok {
					sb.WriteString(mapped)
				} else {
					sb.WriteRune(r)
				}
			}
			return mText{text: sb.String(), upright: true}
		case "mathcal", "mathscr", "mathfrak":
			return mText{text: p.parseGroupText(), italic: true}
		case "color", "textcolor":
			_ = p.parseGroupText()
			return p.parseAtom()
		default:
			if glyph, ok := mathBigOps[t.text]; ok {
				return mText{text: glyph, upright: true, bigOp: true}
			}
			if sym, ok := mathSymbols[t.text]; ok {
				padded := sym == "=" || sym == "≤" || sym == "≥" || sym == "≠" || sym == "≈" || sym == "≡" || sym == "→" || sym == "←" || sym == "↔" || sym == "⇒" || sym == "⇔" || sym == "±" || sym == "×" || sym == "÷" || sym == "∈" || sym == "∉" || sym == "⊂" || sym == "⊆"
				return mText{text: sym, upright: true, padded: padded}
			}
			if mathFunctions[t.text] {
				return mText{text: t.text, upright: true, padded: true}
			}
			return mText{text: t.text, upright: true}
		}
	}
	return nil
}

func (p *mathParser) parseDelimToken() string {
	t := p.next()
	if t.kind == mTokOp && t.text == "." {
		return ""
	}
	if t.kind == mTokCmd {
		if sym, ok := mathSymbols[t.text]; ok {
			return sym
		}
		if t.text == "{" || t.text == "}" {
			return t.text
		}
		if t.text == "lvert" || t.text == "rvert" {
			return "|"
		}
		if t.text == "lVert" || t.text == "rVert" {
			return "‖"
		}
	}
	return t.text
}

func (p *mathParser) parseEnvironment(env string) mathNode {
	var rows [][]mathNode
	var curRow []mathNode
	for {
		var cell []mathNode
		for {
			t := p.peek()
			if t.kind == mTokEOF {
				if len(cell) > 0 || len(curRow) > 0 {
					curRow = append(curRow, mSeq{items: cell})
					rows = append(rows, curRow)
				}
				return mMatrix{env: env, rows: rows}
			}
			if t.kind == mTokCmd && t.text == "end" {
				p.next()
				_ = p.parseGroupText()
				if len(cell) > 0 || len(curRow) > 0 {
					curRow = append(curRow, mSeq{items: cell})
					rows = append(rows, curRow)
				}
				return mMatrix{env: env, rows: rows}
			}
			if t.kind == mTokAmp {
				p.next()
				curRow = append(curRow, mSeq{items: cell})
				cell = nil
				continue
			}
			if t.kind == mTokNewline {
				p.next()
				curRow = append(curRow, mSeq{items: cell})
				rows = append(rows, curRow)
				curRow = nil
				cell = nil
				continue
			}
			if node := p.parseScripted(); node != nil {
				cell = append(cell, node)
			} else {
				p.next()
			}
		}
	}
}

// mathBox is a positioned math glyph/formula subtree relative to its own
// baseline: height = ascent + descent, with baseline at ascent from top.
type mathBox struct {
	w, ascent, descent float32
	draw               func(painter *ui.Painter, ox, baselineY float32, color ui.Color)
}

func measureTextBox(text string, font ui.Font) mathBox {
	if text == "" {
		return mathBox{}
	}
	layout := ui.ShapeText(text, font, 0)
	w, h := layout.Size()
	ascent, descent := font.Size*0.8, font.Size*0.22
	base := ascent
	if lines := layout.Lines(); len(lines) > 0 {
		if lines[0].Ascent > 0 {
			ascent = lines[0].Ascent
		}
		if lines[0].Descent > 0 {
			descent = lines[0].Descent
		}
		base = lines[0].Baseline
	} else if h > 0 {
		ascent = h * 0.78
		descent = h * 0.22
		base = ascent
	}
	return mathBox{
		w:       w,
		ascent:  ascent,
		descent: descent,
		draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
			painter.TextLayout(layout, ox, baselineY-base, color)
		},
	}
}

func layoutMathNode(node mathNode, size float32, display bool) mathBox {
	if node == nil {
		return mathBox{}
	}
	switch n := node.(type) {
	case mSpace:
		return mathBox{w: n.em * size}
	case mText:
		sz := size
		if n.bigOp {
			if display {
				sz = size * 1.45
			} else {
				sz = size * 1.15
			}
		}
		font := ui.Font{Size: sz, Italic: n.italic}
		if n.bold {
			font.Weight = 650
		}
		txt := n.text
		padL, padR := float32(0), float32(0)
		if n.padded {
			padL = size * 0.16
			padR = size * 0.16
		}
		if n.bigOp {
			padR = size * 0.14
		}
		box := measureTextBox(txt, font)
		return mathBox{
			w:       box.w + padL + padR,
			ascent:  box.ascent,
			descent: box.descent,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				if box.draw != nil {
					box.draw(painter, ox+padL, baselineY, color)
				}
			},
		}
	case mSeq:
		if len(n.items) == 0 {
			return mathBox{}
		}
		boxes := make([]mathBox, 0, len(n.items))
		var w, asc, desc float32
		for _, item := range n.items {
			b := layoutMathNode(item, size, display)
			boxes = append(boxes, b)
			w += b.w
			if b.ascent > asc {
				asc = b.ascent
			}
			if b.descent > desc {
				desc = b.descent
			}
		}
		return mathBox{
			w: w, ascent: asc, descent: desc,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				x := ox
				for _, b := range boxes {
					if b.draw != nil {
						b.draw(painter, x, baselineY, color)
					}
					x += b.w
				}
			},
		}
	case mFrac:
		subSize := max(float32(9.5), size*0.84)
		numB := layoutMathNode(n.num, subSize, false)
		denB := layoutMathNode(n.den, subSize, false)
		w := max(numB.w, denB.w) + size*0.36
		axisH := size * 0.28 // math axis height above baseline
		gap := max(float32(2), size*0.14)
		ruleH := max(float32(1.0), size*0.06)
		asc := axisH + ruleH/2 + gap + numB.descent + numB.ascent
		desc := max(float32(0), -axisH+ruleH/2+gap+denB.ascent+denB.descent)
		return mathBox{
			w: w, ascent: asc, descent: desc,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				ruleY := baselineY - axisH
				painter.Line(ox+size*0.08, ruleY, ox+w-size*0.08, ruleY, ruleH, color)
				if numB.draw != nil {
					numX := ox + (w-numB.w)/2
					numBase := ruleY - ruleH/2 - gap - numB.descent
					numB.draw(painter, numX, numBase, color)
				}
				if denB.draw != nil {
					denX := ox + (w-denB.w)/2
					denBase := ruleY + ruleH/2 + gap + denB.ascent
					denB.draw(painter, denX, denBase, color)
				}
			},
		}
	case mSqrt:
		bodyB := layoutMathNode(n.body, size, display)
		var idxB mathBox
		if n.index != nil {
			idxB = layoutMathNode(n.index, max(float32(8.5), size*0.62), false)
		}
		radW := size * 0.65
		if idxB.w > radW*0.55 {
			radW = idxB.w + size*0.25
		}
		padT := max(float32(2.5), size*0.18)
		stroke := max(float32(1.0), size*0.065)
		asc := bodyB.ascent + padT + stroke
		desc := bodyB.descent + size*0.08
		w := radW + bodyB.w + size*0.16
		return mathBox{
			w: w, ascent: asc, descent: desc,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				topY := baselineY - asc + stroke/2
				botY := baselineY + bodyB.descent
				midY := baselineY - bodyB.ascent*0.25
				x0 := ox + radW*0.15
				x1 := ox + radW*0.42
				x2 := ox + radW*0.65
				x3 := ox + radW*0.92
				painter.StrokePolyline([][2]float32{
					{x0, midY},
					{x1, midY},
					{x2, botY},
					{x3, topY},
					{ox + w - size*0.06, topY},
				}, stroke, color)
				if idxB.draw != nil {
					idxB.draw(painter, ox+max(0, x2-idxB.w-size*0.12), midY-size*0.14, color)
				}
				if bodyB.draw != nil {
					bodyB.draw(painter, ox+radW+size*0.06, baselineY, color)
				}
			},
		}
	case mScript:
		baseB := layoutMathNode(n.base, size, display)
		subSize := max(float32(9.0), size*0.72)
		subB := layoutMathNode(n.sub, subSize, false)
		supB := layoutMathNode(n.sup, subSize, false)
		if n.limits && display {
			w := max(baseB.w, max(subB.w, supB.w)) + size*0.12
			gap := max(float32(2), size*0.12)
			asc := baseB.ascent
			if n.sup != nil {
				asc += gap + supB.descent + supB.ascent
			}
			desc := baseB.descent
			if n.sub != nil {
				desc += gap + subB.ascent + subB.descent
			}
			return mathBox{
				w: w, ascent: asc, descent: desc,
				draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
					if baseB.draw != nil {
						baseB.draw(painter, ox+(w-baseB.w)/2, baselineY, color)
					}
					if supB.draw != nil {
						supBase := baselineY - baseB.ascent - gap - supB.descent
						supB.draw(painter, ox+(w-supB.w)/2, supBase, color)
					}
					if subB.draw != nil {
						subBase := baselineY + baseB.descent + gap + subB.ascent
						subB.draw(painter, ox+(w-subB.w)/2, subBase, color)
					}
				},
			}
		}
		supShift := max(baseB.ascent*0.56, size*0.38)
		subShift := max(baseB.descent+subB.ascent*0.45, size*0.28)
		asc := baseB.ascent
		if n.sup != nil {
			asc = max(asc, supShift+supB.ascent)
		}
		desc := baseB.descent
		if n.sub != nil {
			desc = max(desc, subShift+subB.descent)
		}
		rightW := max(subB.w, supB.w)
		w := baseB.w + rightW + size*0.04
		return mathBox{
			w: w, ascent: asc, descent: desc,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				if baseB.draw != nil {
					baseB.draw(painter, ox, baselineY, color)
				}
				sx := ox + baseB.w + size*0.02
				if supB.draw != nil {
					supB.draw(painter, sx, baselineY-supShift, color)
				}
				if subB.draw != nil {
					subB.draw(painter, sx, baselineY+subShift, color)
				}
			},
		}
	case mAccent:
		bodyB := layoutMathNode(n.body, size, display)
		stroke := max(float32(1.0), size*0.06)
		pad := max(float32(3), size*0.22)
		if n.kind == "underline" {
			return mathBox{
				w: bodyB.w, ascent: bodyB.ascent, descent: bodyB.descent + pad,
				draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
					if bodyB.draw != nil {
						bodyB.draw(painter, ox, baselineY, color)
					}
					y := baselineY + bodyB.descent + pad*0.5
					painter.Line(ox, y, ox+bodyB.w, y, stroke, color)
				},
			}
		}
		if n.kind == "overline" || n.kind == "bar" {
			return mathBox{
				w: bodyB.w, ascent: bodyB.ascent + pad, descent: bodyB.descent,
				draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
					if bodyB.draw != nil {
						bodyB.draw(painter, ox, baselineY, color)
					}
					y := baselineY - bodyB.ascent - pad*0.45
					painter.Line(ox, y, ox+bodyB.w, y, stroke, color)
				},
			}
		}
		glyph := "^"
		switch n.kind {
		case "tilde":
			glyph = "˜"
		case "vec":
			glyph = "→"
		case "dot":
			glyph = "·"
		case "ddot":
			glyph = "··"
		}
		accB := measureTextBox(glyph, ui.Font{Size: size * 0.72})
		return mathBox{
			w: max(bodyB.w, accB.w), ascent: bodyB.ascent + pad, descent: bodyB.descent,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				if bodyB.draw != nil {
					bodyB.draw(painter, ox+(max(bodyB.w, accB.w)-bodyB.w)/2, baselineY, color)
				}
				if accB.draw != nil {
					accB.draw(painter, ox+(max(bodyB.w, accB.w)-accB.w)/2, baselineY-bodyB.ascent-size*0.04, color)
				}
			},
		}
	case mDelim:
		bodyB := layoutMathNode(n.body, size, display)
		totH := bodyB.ascent + bodyB.descent
		delimFont := ui.Font{Size: max(size, totH*0.78)}
		lB := measureTextBox(n.left, delimFont)
		rB := measureTextBox(n.right, delimFont)
		asc := max(bodyB.ascent, max(lB.ascent, rB.ascent))
		desc := max(bodyB.descent, max(lB.descent, rB.descent))
		midY := (bodyB.ascent - bodyB.descent) / 2
		return mathBox{
			w: lB.w + bodyB.w + rB.w, ascent: asc, descent: desc,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				if lB.draw != nil {
					lBase := baselineY - midY + (lB.ascent-lB.descent)/2
					lB.draw(painter, ox, lBase, color)
				}
				if bodyB.draw != nil {
					bodyB.draw(painter, ox+lB.w, baselineY, color)
				}
				if rB.draw != nil {
					rBase := baselineY - midY + (rB.ascent-rB.descent)/2
					rB.draw(painter, ox+lB.w+bodyB.w, rBase, color)
				}
			},
		}
	case mMatrix:
		if len(n.rows) == 0 {
			return mathBox{}
		}
		subSize := max(float32(10), size*0.92)
		if n.env == "smallmatrix" {
			subSize = max(float32(8.5), size*0.72)
		}
		cols := 0
		for _, r := range n.rows {
			if len(r) > cols {
				cols = len(r)
			}
		}
		colW := make([]float32, cols)
		cellBoxes := make([][]mathBox, len(n.rows))
		rowAsc := make([]float32, len(n.rows))
		rowDesc := make([]float32, len(n.rows))
		for r, row := range n.rows {
			cellBoxes[r] = make([]mathBox, cols)
			for c, cell := range row {
				b := layoutMathNode(cell, subSize, false)
				cellBoxes[r][c] = b
				if b.w > colW[c] {
					colW[c] = b.w
				}
				if b.ascent > rowAsc[r] {
					rowAsc[r] = b.ascent
				}
				if b.descent > rowDesc[r] {
					rowDesc[r] = b.descent
				}
			}
		}
		colGap := size * 0.7
		rowGap := size * 0.35
		totalW := float32(0)
		for c, cw := range colW {
			totalW += cw
			if c > 0 {
				totalW += colGap
			}
		}
		totalH := float32(0)
		for r := range n.rows {
			totalH += rowAsc[r] + rowDesc[r]
			if r > 0 {
				totalH += rowGap
			}
		}
		lDelim, rDelim := "", ""
		switch n.env {
		case "pmatrix":
			lDelim, rDelim = "(", ")"
		case "bmatrix":
			lDelim, rDelim = "[", "]"
		case "Bmatrix":
			lDelim, rDelim = "{", "}"
		case "vmatrix":
			lDelim, rDelim = "|", "|"
		case "Vmatrix":
			lDelim, rDelim = "‖", "‖"
		case "cases":
			lDelim, rDelim = "{", ""
		}
		delimW := float32(0)
		if lDelim != "" || rDelim != "" {
			delimW = size * 0.45
		}
		asc := totalH/2 + size*0.25
		desc := max(float32(0), totalH-asc)
		w := totalW + delimW*2
		if lDelim == "" {
			w -= delimW
		}
		if rDelim == "" {
			w -= delimW
		}
		leftPad := float32(0)
		if lDelim != "" {
			leftPad = delimW
		}
		stroke := max(float32(1.0), size*0.07)
		return mathBox{
			w: w, ascent: asc, descent: desc,
			draw: func(painter *ui.Painter, ox, baselineY float32, color ui.Color) {
				topY := baselineY - asc
				botY := baselineY + desc
				if lDelim == "[" || lDelim == "|" || lDelim == "‖" || lDelim == "(" {
					x := ox + leftPad*0.35
					painter.Line(x, topY, x, botY, stroke, color)
					if lDelim == "[" {
						painter.Line(x, topY, x+leftPad*0.45, topY, stroke, color)
						painter.Line(x, botY, x+leftPad*0.45, botY, stroke, color)
					} else if lDelim == "‖" {
						painter.Line(x+stroke*2.2, topY, x+stroke*2.2, botY, stroke, color)
					}
				} else if lDelim == "{" {
					braceB := measureTextBox("{", ui.Font{Size: max(size, totalH*0.75)})
					if braceB.draw != nil {
						braceB.draw(painter, ox, baselineY+(braceB.ascent-braceB.descent)/2-size*0.2, color)
					}
				}
				if rDelim == "]" || rDelim == "|" || rDelim == "‖" || rDelim == ")" {
					x := ox + w - delimW*0.35
					painter.Line(x, topY, x, botY, stroke, color)
					if rDelim == "]" {
						painter.Line(x-delimW*0.45, topY, x, topY, stroke, color)
						painter.Line(x-delimW*0.45, botY, x, botY, stroke, color)
					} else if rDelim == "‖" {
						painter.Line(x-stroke*2.2, topY, x-stroke*2.2, botY, stroke, color)
					}
				} else if rDelim == "}" {
					braceB := measureTextBox("}", ui.Font{Size: max(size, totalH*0.75)})
					if braceB.draw != nil {
						braceB.draw(painter, ox+w-braceB.w, baselineY+(braceB.ascent-braceB.descent)/2-size*0.2, color)
					}
				}
				cy := topY
				for r := range n.rows {
					cellBase := cy + rowAsc[r]
					cx := ox + leftPad
					for c := 0; c < cols; c++ {
						b := cellBoxes[r][c]
						if b.draw != nil {
							off := (colW[c] - b.w) / 2
							if n.env == "cases" || n.env == "aligned" || n.env == "align" {
								off = 0
							}
							b.draw(painter, cx+off, cellBase, color)
						}
						cx += colW[c] + colGap
					}
					cy += rowAsc[r] + rowDesc[r] + rowGap
				}
			},
		}
	}
	return mathBox{}
}

type mathCacheKey struct {
	tex     string
	size    float32
	display bool
}

var (
	mathCacheMu sync.Mutex
	mathCache   = map[mathCacheKey]mathBox{}
	mathOrder   []mathCacheKey
)

func compileMathBox(tex string, size float32, display bool) mathBox {
	key := mathCacheKey{tex: tex, size: size, display: display}
	mathCacheMu.Lock()
	if b, ok := mathCache[key]; ok {
		mathCacheMu.Unlock()
		return b
	}
	mathCacheMu.Unlock()

	ast := parseMathAST(tex)
	box := layoutMathNode(ast, size, display)
	if box.w <= 0 {
		box = measureTextBox(strings.TrimSpace(tex), ui.Font{Size: size})
	}

	mathCacheMu.Lock()
	if len(mathOrder) >= 256 {
		delete(mathCache, mathOrder[0])
		mathOrder = mathOrder[1:]
	}
	mathCache[key] = box
	mathOrder = append(mathOrder, key)
	mathCacheMu.Unlock()
	return box
}

func (w *workbench) inlineMathObject(tex string, size float32) (markdown.InlineObject, bool) {
	if strings.TrimSpace(tex) == "" {
		return markdown.InlineObject{}, false
	}
	if size <= 0 {
		size = 15
	}
	box := compileMathBox(tex, size, false)
	if box.w <= 0 || math.IsNaN(float64(box.w)) {
		return markdown.InlineObject{}, false
	}
	return markdown.InlineObject{
		Width:  box.w,
		Height: box.ascent,
		Depth:  box.descent,
		Draw: func(painter *ui.Painter, r ui.Rect) {
			if box.draw == nil {
				return
			}
			color := painter.Theme().Text
			if color.A == 0 {
				color = w.widgets.markdownPalette.ink
			}
			box.draw(painter, r.X, r.Y+box.ascent, color)
		},
	}, true
}

func (w *workbench) mathBlock(c *ui.Context, tex string) {
	p := w.widgets.markdownPalette
	if strings.TrimSpace(tex) == "" {
		return
	}
	frame := ui.Row(c).FillWidth().Padding(12, 16).Radius(10).Background(p.soft).Border(1, p.lineSoft).AlignItems(ui.Center)
	frame.Role(ui.RoleGroup).Label(fmt.Sprintf("数学公式：%s", strings.TrimSpace(tex)))
	frame.Children(func() {
		if img, ok := w.snapshotImage("math", tex, p.dark); ok {
			ui.Column(c).Grow(1).AlignItems(ui.Center).Children(func() {
				ui.Image(c, img).Fit(ui.Contain).MaxWidth(700).MaxHeight(260)
			})
		} else {
			box := compileMathBox(tex, 17, true)
			ui.ScrollHorizontal(c).Grow(1).MinWidth(0).Children(func() {
				canvas := ui.Box(c).Size(max(box.w+8, 24), max(box.ascent+box.descent+8, 28)).Shrink(0)
				canvas.Draw(func(painter *ui.Painter, r ui.Rect) {
					if box.draw != nil {
						box.draw(painter, r.X+4, r.Y+4+box.ascent, p.ink)
					}
				})
			})
		}
		shown := float32(0)
		if frame.Hovered() {
			shown = 1
		}
		tools := ui.Row(c).Gap(2).Shrink(0)
		tools.Opacity(tools.AnimateTo("math-tools", shown, controlEase()))
		tools.Children(func() {
			if iconButton(c, p, "copy-math", "复制 TeX 公式", copyIcon).Size(24, 24).Padding(5).Clicked() {
				c.WriteClipboard(strings.TrimSpace(tex))
			}
		})
	})
}
