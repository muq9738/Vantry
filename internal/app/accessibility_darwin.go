//go:build darwin

package app

import (
	"errors"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// macOS lists an app under 隐私与安全性 → 辅助功能 only after the app itself
// asks TCC for Accessibility. A PostEvent preflight never creates that entry,
// so an explicit Allow action registers the owning Core with the system.
// TCC determines the responsible application. AX trust alone must not replace
// the Core Graphics permission check used by the actual input sink.
var accessibilityAPI struct {
	once        sync.Once
	err         error
	trusted     func() bool
	withOptions func(uintptr) bool
	dictCreate  func(uintptr, *uintptr, *uintptr, int, uintptr, uintptr) uintptr
	release     func(uintptr)
	promptKey   uintptr
	trueValue   uintptr
	keyCalls    uintptr
	valueCalls  uintptr
}

func loadAccessibility() error {
	a := &accessibilityAPI
	a.once.Do(func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.err = errors.New("无法加载系统辅助功能接口")
			}
		}()
		services, err := purego.Dlopen("/System/Library/Frameworks/ApplicationServices.framework/ApplicationServices", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			a.err = err
			return
		}
		foundation, err := purego.Dlopen("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation", purego.RTLD_LAZY|purego.RTLD_GLOBAL)
		if err != nil {
			a.err = err
			return
		}
		purego.RegisterLibFunc(&a.trusted, services, "AXIsProcessTrusted")
		purego.RegisterLibFunc(&a.withOptions, services, "AXIsProcessTrustedWithOptions")
		purego.RegisterLibFunc(&a.dictCreate, foundation, "CFDictionaryCreate")
		purego.RegisterLibFunc(&a.release, foundation, "CFRelease")
		promptKey, err := purego.Dlsym(services, "kAXTrustedCheckOptionPrompt")
		if err != nil {
			a.err = err
			return
		}
		trueValue, err := purego.Dlsym(foundation, "kCFBooleanTrue")
		if err != nil {
			a.err = err
			return
		}
		if a.keyCalls, err = purego.Dlsym(foundation, "kCFTypeDictionaryKeyCallBacks"); err != nil {
			a.err = err
			return
		}
		if a.valueCalls, err = purego.Dlsym(foundation, "kCFTypeDictionaryValueCallBacks"); err != nil {
			a.err = err
			return
		}
		// Both symbols are global CF object pointers; dlsym returns their address.
		a.promptKey = **(**uintptr)(unsafe.Pointer(&promptKey))
		a.trueValue = **(**uintptr)(unsafe.Pointer(&trueValue))
	})
	return a.err
}

// accessibilityTrusted is read-only; it never shows a dialog.
func accessibilityTrusted() bool {
	if loadAccessibility() != nil {
		return false
	}
	return accessibilityAPI.trusted()
}

// registerAccessibility asks macOS to add Vantry to the Accessibility list and
// shows the system prompt. Call it only for an explicit user action.
func registerAccessibility() (bool, error) {
	if err := loadAccessibility(); err != nil {
		return false, err
	}
	a := &accessibilityAPI
	if a.trusted() {
		return true, nil
	}
	// An untrusted result describes this process, not a stale system record.
	// Never reset consent here: a repeated request could revoke the user's grant.
	key, value := a.promptKey, a.trueValue
	options := a.dictCreate(0, &key, &value, 1, a.keyCalls, a.valueCalls)
	if options == 0 {
		return false, errors.New("无法创建辅助功能授权请求")
	}
	defer a.release(options)
	return a.withOptions(options), nil
}
