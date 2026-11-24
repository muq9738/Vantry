//! macOS CoreGraphics system input injection and display metrics for Remote Desktop.
#![cfg(target_os = "macos")]

use std::ffi::c_void;
use vantry_connectivity::desktop_media::InputEvent;

#[repr(C)]
#[derive(Clone, Copy)]
struct CGPoint {
    x: f64,
    y: f64,
}

#[repr(C)]
#[derive(Clone, Copy)]
struct CGSize {
    width: f64,
    height: f64,
}

#[repr(C)]
#[derive(Clone, Copy)]
struct CGRect {
    origin: CGPoint,
    size: CGSize,
}

const K_CG_HID_EVENT_TAP: u32 = 0;
const K_CG_EVENT_LEFT_MOUSE_DOWN: u32 = 1;
const K_CG_EVENT_LEFT_MOUSE_UP: u32 = 2;
const K_CG_EVENT_RIGHT_MOUSE_DOWN: u32 = 3;
const K_CG_EVENT_RIGHT_MOUSE_UP: u32 = 4;
const K_CG_EVENT_MOUSE_MOVED: u32 = 5;
const K_CG_EVENT_OTHER_MOUSE_DOWN: u32 = 25;
const K_CG_EVENT_OTHER_MOUSE_UP: u32 = 26;
const K_CG_SCROLL_EVENT_UNIT_PIXEL: u32 = 0;

#[link(name = "CoreGraphics", kind = "framework")]
unsafe extern "C" {
    fn CGMainDisplayID() -> u32;
    fn CGDisplayBounds(display: u32) -> CGRect;
    fn CGEventCreateMouseEvent(
        source: *const c_void,
        mouse_type: u32,
        position: CGPoint,
        button: u32,
    ) -> *mut c_void;
    fn CGEventCreateScrollWheelEvent(
        source: *const c_void,
        units: u32,
        wheel_count: u32,
        wheel1: i32,
        wheel2: i32,
    ) -> *mut c_void;
    fn CGEventCreateKeyboardEvent(
        source: *const c_void,
        virtual_key: u16,
        key_down: bool,
    ) -> *mut c_void;
    fn CGEventKeyboardSetUnicodeString(
        event: *mut c_void,
        string_length: usize,
        unicode_string: *const u16,
    );
    fn CGEventPost(tap: u32, event: *mut c_void);
}

#[link(name = "CoreFoundation", kind = "framework")]
unsafe extern "C" {
    fn CFRelease(cf: *mut c_void);
}

pub fn main_display_size() -> (u32, u32) {
    unsafe {
        let bounds = CGDisplayBounds(CGMainDisplayID());
        (
            bounds.size.width.max(1.0) as u32,
            bounds.size.height.max(1.0) as u32,
        )
    }
}

fn map_point(ev: &InputEvent) -> CGPoint {
    unsafe {
        let bounds = CGDisplayBounds(CGMainDisplayID());
        let x = if (0.0..=1.0).contains(&ev.x) && (0.0..=1.0).contains(&ev.y) {
            bounds.origin.x + ev.x * bounds.size.width
        } else {
            bounds.origin.x + ev.x
        };
        let y = if (0.0..=1.0).contains(&ev.x) && (0.0..=1.0).contains(&ev.y) {
            bounds.origin.y + ev.y * bounds.size.height
        } else {
            bounds.origin.y + ev.y
        };
        CGPoint { x, y }
    }
}

fn post_and_release(event: *mut c_void) {
    if !event.is_null() {
        unsafe {
            CGEventPost(K_CG_HID_EVENT_TAP, event);
            CFRelease(event);
        }
    }
}

/// Inject a mouse, scroll, or keyboard input event on macOS via CoreGraphics.
pub fn inject_input(ev: &InputEvent) {
    let pt = map_point(ev);
    let (down_ty, up_ty, btn) = match ev.button.to_ascii_lowercase().as_str() {
        "right" | "2" => (K_CG_EVENT_RIGHT_MOUSE_DOWN, K_CG_EVENT_RIGHT_MOUSE_UP, 1u32),
        "middle" | "1" => (K_CG_EVENT_OTHER_MOUSE_DOWN, K_CG_EVENT_OTHER_MOUSE_UP, 2u32),
        _ => (K_CG_EVENT_LEFT_MOUSE_DOWN, K_CG_EVENT_LEFT_MOUSE_UP, 0u32),
    };
    match ev.kind.to_ascii_lowercase().as_str() {
        "move" | "mousemove" => unsafe {
            post_and_release(CGEventCreateMouseEvent(
                std::ptr::null(),
                K_CG_EVENT_MOUSE_MOVED,
                pt,
                0,
            ));
        },
        "down" | "mousedown" => unsafe {
            post_and_release(CGEventCreateMouseEvent(std::ptr::null(), down_ty, pt, btn));
        },
        "up" | "mouseup" => unsafe {
            post_and_release(CGEventCreateMouseEvent(std::ptr::null(), up_ty, pt, btn));
        },
        "click" => unsafe {
            post_and_release(CGEventCreateMouseEvent(std::ptr::null(), down_ty, pt, btn));
            post_and_release(CGEventCreateMouseEvent(std::ptr::null(), up_ty, pt, btn));
        },
        "wheel" | "scroll" => unsafe {
            let dy = (-ev.y).round() as i32;
            let dx = (-ev.x).round() as i32;
            post_and_release(CGEventCreateScrollWheelEvent(
                std::ptr::null(),
                K_CG_SCROLL_EVENT_UNIT_PIXEL,
                2,
                dy,
                dx,
            ));
        },
        "key" | "keydown" | "keyup" | "text" => {
            let utf16: Vec<u16> = ev.key.encode_utf16().collect();
            if utf16.is_empty() {
                return;
            }
            let down = ev.kind.to_ascii_lowercase() != "keyup";
            unsafe {
                let e = CGEventCreateKeyboardEvent(std::ptr::null(), 0, down);
                if !e.is_null() {
                    CGEventKeyboardSetUnicodeString(e, utf16.len(), utf16.as_ptr());
                    post_and_release(e);
                }
                if matches!(ev.kind.to_ascii_lowercase().as_str(), "key" | "text") {
                    let up = CGEventCreateKeyboardEvent(std::ptr::null(), 0, false);
                    if !up.is_null() {
                        CGEventKeyboardSetUnicodeString(up, utf16.len(), utf16.as_ptr());
                        post_and_release(up);
                    }
                }
            }
        }
        other => {
            tracing::debug!(kind = %other, "unhandled remote desktop input event");
        }
    }
}
