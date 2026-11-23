package desktop

import "github.com/egoist/mygo/ui"

// KeyEvent's stable plugin contract uses macOS virtual key codes and
// CoreGraphics flags, not MyGo enum ordinals or modifier bit positions.
var remoteMacPhysicalKeys = map[ui.KeyCode]int{
	ui.CodeKeyA: 0, ui.CodeKeyS: 1, ui.CodeKeyD: 2, ui.CodeKeyF: 3, ui.CodeKeyH: 4, ui.CodeKeyG: 5,
	ui.CodeKeyZ: 6, ui.CodeKeyX: 7, ui.CodeKeyC: 8, ui.CodeKeyV: 9, ui.CodeKeyB: 11,
	ui.CodeKeyQ: 12, ui.CodeKeyW: 13, ui.CodeKeyE: 14, ui.CodeKeyR: 15, ui.CodeKeyY: 16, ui.CodeKeyT: 17,
	ui.CodeDigit1: 18, ui.CodeDigit2: 19, ui.CodeDigit3: 20, ui.CodeDigit4: 21, ui.CodeDigit6: 22, ui.CodeDigit5: 23,
	ui.CodeEqual: 24, ui.CodeDigit9: 25, ui.CodeDigit7: 26, ui.CodeMinus: 27, ui.CodeDigit8: 28, ui.CodeDigit0: 29,
	ui.CodeBracketRight: 30, ui.CodeKeyO: 31, ui.CodeKeyU: 32, ui.CodeBracketLeft: 33, ui.CodeKeyI: 34, ui.CodeKeyP: 35,
	ui.CodeEnter: 36, ui.CodeKeyL: 37, ui.CodeKeyJ: 38, ui.CodeQuote: 39, ui.CodeKeyK: 40, ui.CodeSemicolon: 41,
	ui.CodeBackslash: 42, ui.CodeComma: 43, ui.CodeSlash: 44, ui.CodeKeyN: 45, ui.CodeKeyM: 46, ui.CodePeriod: 47,
	ui.CodeTab: 48, ui.CodeSpace: 49, ui.CodeBackquote: 50, ui.CodeBackspace: 51, ui.CodeEscape: 53,
	ui.CodeSuperRight: 54, ui.CodeSuperLeft: 55, ui.CodeShiftLeft: 56, ui.CodeCapsLock: 57, ui.CodeAltLeft: 58, ui.CodeControlLeft: 59,
	ui.CodeShiftRight: 60, ui.CodeAltRight: 61, ui.CodeControlRight: 62,
	ui.CodeF1: 122, ui.CodeF2: 120, ui.CodeF3: 99, ui.CodeF4: 118, ui.CodeF5: 96, ui.CodeF6: 97,
	ui.CodeF7: 98, ui.CodeF8: 100, ui.CodeF9: 101, ui.CodeF10: 109, ui.CodeF11: 103, ui.CodeF12: 111,
	ui.CodeHome: 115, ui.CodePageUp: 116, ui.CodeDelete: 117, ui.CodeEnd: 119, ui.CodePageDown: 121,
	ui.CodeArrowLeft: 123, ui.CodeArrowRight: 124, ui.CodeArrowDown: 125, ui.CodeArrowUp: 126,
}
var remoteMacLogicalKeys = map[ui.Key]int{
	ui.KeyEnter: 36, ui.KeyEscape: 53, ui.KeyBackspace: 51, ui.KeyTab: 48, ui.KeySpace: 49, ui.KeyDelete: 117,
	ui.KeyHome: 115, ui.KeyEnd: 119, ui.KeyPageUp: 116, ui.KeyPageDown: 121,
	ui.KeyLeft: 123, ui.KeyRight: 124, ui.KeyDown: 125, ui.KeyUp: 126,
	ui.KeyF1: 122, ui.KeyF2: 120, ui.KeyF3: 99, ui.KeyF4: 118, ui.KeyF5: 96, ui.KeyF6: 97,
	ui.KeyF7: 98, ui.KeyF8: 100, ui.KeyF9: 101, ui.KeyF10: 109, ui.KeyF11: 103, ui.KeyF12: 111,
	ui.KeyMinus: 27, ui.KeyEqual: 24, ui.KeyComma: 43, ui.KeyPeriod: 47, ui.KeySlash: 44, ui.KeySemicolon: 41,
	ui.KeyQuote: 39, ui.KeyBracketLeft: 33, ui.KeyBracketRight: 30, ui.KeyBackslash: 42, ui.KeyBackquote: 50,
}

func remoteMacKey(ev ui.InputEvent) (int, bool) {
	if code, ok := remoteMacPhysicalKeys[ev.Code]; ok {
		return code, true
	}
	if ev.Key >= ui.KeyA && ev.Key <= ui.KeyZ {
		letters := [...]int{0, 11, 8, 2, 14, 3, 5, 4, 34, 38, 40, 37, 46, 45, 31, 35, 12, 15, 1, 17, 32, 9, 13, 7, 16, 6}
		return letters[int(ev.Key-ui.KeyA)], true
	}
	if ev.Key >= ui.Key0 && ev.Key <= ui.Key9 {
		digits := [...]int{29, 18, 19, 20, 21, 23, 22, 26, 28, 25}
		return digits[int(ev.Key-ui.Key0)], true
	}
	code, ok := remoteMacLogicalKeys[ev.Key]
	return code, ok
}
func remoteTextKey(ev ui.InputEvent) bool {
	if ev.Code >= ui.CodeKeyA && ev.Code <= ui.CodeKeyZ || ev.Code >= ui.CodeDigit1 && ev.Code <= ui.CodeDigit0 || ev.Code >= ui.CodeMinus && ev.Code <= ui.CodeSlash || ev.Code == ui.CodeSpace {
		return true
	}
	if ev.Key >= ui.KeyA && ev.Key <= ui.KeyZ || ev.Key >= ui.Key0 && ev.Key <= ui.Key9 {
		return true
	}
	switch ev.Key {
	case ui.KeySpace, ui.KeyMinus, ui.KeyEqual, ui.KeyComma, ui.KeyPeriod, ui.KeySlash, ui.KeySemicolon, ui.KeyQuote, ui.KeyBracketLeft, ui.KeyBracketRight, ui.KeyBackslash, ui.KeyBackquote:
		return true
	}
	return false
}
func remoteCGFlags(mods ui.Modifiers) uint32 {
	var flags uint32
	if mods&ui.Shift != 0 {
		flags |= 1 << 17
	}
	if mods&ui.Ctrl != 0 {
		flags |= 1 << 18
	}
	if mods&ui.Alt != 0 {
		flags |= 1 << 19
	}
	if mods&ui.Super != 0 {
		flags |= 1 << 20
	}
	return flags
}
func (w *workbench) remoteDesktopFocused(c *ui.Context) bool {
	return w.stage != "" && w.rd.connected && w.rd.focus.Focused(c)
}
func (w *workbench) releaseRemoteKeys() {
	if driver := w.defaults.RemoteDesktop; driver != nil {
		for code := range w.rd.heldKeys {
			driver.SendKey("up", code, "", 0)
		}
		for button := range w.rd.heldButtons {
			driver.SendMouse("up", w.rd.lastX, w.rd.lastY, button)
		}
	}
	w.rd.heldKeys = nil
	w.rd.heldButtons = nil
}
