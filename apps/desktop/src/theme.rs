//! Stardust（星尘）: Vantry's visual language on NSH.
//!
//! Two layers, both from one palette:
//! - [`STARDUST_THEME`], a shadcn `cssVars` theme file the app installs with
//!   [`install`], so NSH's tokens (`theme().primary`, Tailwind's
//!   `bg-primary`) and the platform's own controls follow Stardust;
//! - [`Palette`], typed [`AdaptiveColor`]s for what shadcn has no token for
//!   (the user bubble, the amber approval card, status dots).
//!
//! Light: warm paper sidebar `#F6F5F3`, white reading canvas `#FFFFFF`,
//! ember orange `#FF5A1F`. Dark: graphite sidebar `#1C1D21`, canvas
//! `#16171A`, orange `#FF6A2B` (`docs/desktop-ui.md`).

use nsh_core::prelude::*;

/// Stardust's colors, each with its light and dark value.
#[derive(Debug, Clone, Copy, PartialEq)]
pub struct Palette {
    /// The reading canvas.
    pub canvas: AdaptiveColor,
    /// The sidebar.
    pub sidebar: AdaptiveColor,
    /// Raised cards: the composer, the selected sidebar row.
    pub card: AdaptiveColor,
    /// Soft fills: hover rows, chips.
    pub soft: AdaptiveColor,
    /// Code wells.
    pub code: AdaptiveColor,
    /// Body text.
    pub ink: AdaptiveColor,
    /// Secondary text.
    pub muted: AdaptiveColor,
    /// Hints, timestamps.
    pub faint: AdaptiveColor,
    /// Borders.
    pub line: AdaptiveColor,
    pub line_soft: AdaptiveColor,
    /// The ember accent: send, focus, the active thread.
    pub accent: AdaptiveColor,
    /// Text on [`Palette::accent`].
    pub on_accent: AdaptiveColor,
    /// The user's message bubble.
    pub user: AdaptiveColor,
    pub success: AdaptiveColor,
    pub warning: AdaptiveColor,
    pub danger: AdaptiveColor,
    /// The approval card's warm amber fill and border.
    pub approval: AdaptiveColor,
    pub approval_border: AdaptiveColor,
    /// The modal's backdrop.
    pub scrim: AdaptiveColor,
}

/// `0xRRGGBBAA` in light appearance and in dark.
const fn rgba_pair(light: u32, dark: u32) -> AdaptiveColor {
    AdaptiveColor {
        light: nsh_core::color::rgba(light).to_hsla(),
        dark: nsh_core::color::rgba(dark).to_hsla(),
    }
}

/// The palette.
pub const PALETTE: Palette = Palette {
    canvas: AdaptiveColor::rgb(0xFFFFFF, 0x16171A),
    sidebar: AdaptiveColor::rgb(0xF6F5F3, 0x1C1D21),
    card: AdaptiveColor::rgb(0xFFFFFF, 0x23252A),
    soft: AdaptiveColor::rgb(0xEFEDEA, 0x26282E),
    code: AdaptiveColor::rgb(0xF4F3F1, 0x101114),
    ink: AdaptiveColor::rgb(0x1F1E1C, 0xF3F2F0),
    muted: AdaptiveColor::rgb(0x5F5C57, 0xA9A6A1),
    faint: AdaptiveColor::rgb(0x8C8882, 0x7C7A76),
    line: AdaptiveColor::rgb(0xE4E1DC, 0x2E3036),
    line_soft: AdaptiveColor::rgb(0xEEECE8, 0x24262B),
    accent: AdaptiveColor::rgb(0xFF5A1F, 0xFF6A2B),
    on_accent: AdaptiveColor::rgb(0xFFFFFF, 0xFFFFFF),
    user: AdaptiveColor::rgb(0xF3F1EE, 0x26282E),
    success: AdaptiveColor::rgb(0x1F9D4C, 0x30D158),
    warning: AdaptiveColor::rgb(0xB45309, 0xF5A524),
    danger: AdaptiveColor::rgb(0xC9252C, 0xFF5A5F),
    approval: AdaptiveColor::rgb(0xFFF7E8, 0x2B2416),
    approval_border: AdaptiveColor::rgb(0xF2C46D, 0x6B5220),
    scrim: rgba_pair(0x1F1E1C59, 0x00000099),
};

/// Layout and type scale (points).
pub mod metrics {
    /// The reading column's width.
    pub const READING_WIDTH: f32 = 760.0;
    /// The sidebar's width.
    pub const SIDEBAR_WIDTH: f32 = 264.0;
    /// The header's height (room for the macOS window buttons).
    pub const HEADER_HEIGHT: f32 = 52.0;
    /// The composer card's corner radius.
    pub const COMPOSER_RADIUS: f32 = 16.0;
    /// Selected sidebar rows, cards.
    pub const CARD_RADIUS: f32 = 10.0;
    /// The approval modal's width.
    pub const MODAL_WIDTH: f32 = 520.0;
    /// Body text.
    pub const BODY: f32 = 14.0;
    /// Secondary text.
    pub const SMALL: f32 = 12.0;
}

/// Stardust as a shadcn `cssVars` theme file ([`nsh_base::Theme::from_json`]).
pub const STARDUST_THEME: &str = r##"{
  "name": "Stardust",
  "cssVars": {
    "theme": { "radius": "0.625rem" },
    "light": {
      "background": "#FFFFFF",
      "foreground": "#1F1E1C",
      "card": "#FFFFFF",
      "card-foreground": "#1F1E1C",
      "popover": "#FFFFFF",
      "popover-foreground": "#1F1E1C",
      "primary": "#FF5A1F",
      "primary-foreground": "#FFFFFF",
      "secondary": "#EFEDEA",
      "secondary-foreground": "#1F1E1C",
      "muted": "#EFEDEA",
      "muted-foreground": "#5F5C57",
      "accent": "#EFEDEA",
      "accent-foreground": "#1F1E1C",
      "destructive": "#C9252C",
      "border": "#E4E1DC",
      "input": "#E4E1DC",
      "ring": "#FF5A1F",
      "sidebar": "#F6F5F3",
      "sidebar-foreground": "#1F1E1C",
      "sidebar-primary": "#FF5A1F",
      "sidebar-primary-foreground": "#FFFFFF",
      "sidebar-accent": "#FFFFFF",
      "sidebar-accent-foreground": "#1F1E1C",
      "sidebar-border": "#E4E1DC",
      "sidebar-ring": "#FF5A1F"
    },
    "dark": {
      "background": "#16171A",
      "foreground": "#F3F2F0",
      "card": "#23252A",
      "card-foreground": "#F3F2F0",
      "popover": "#23252A",
      "popover-foreground": "#F3F2F0",
      "primary": "#FF6A2B",
      "primary-foreground": "#FFFFFF",
      "secondary": "#26282E",
      "secondary-foreground": "#F3F2F0",
      "muted": "#26282E",
      "muted-foreground": "#A9A6A1",
      "accent": "#26282E",
      "accent-foreground": "#F3F2F0",
      "destructive": "#FF5A5F",
      "border": "#2E3036",
      "input": "#2E3036",
      "ring": "#FF6A2B",
      "sidebar": "#1C1D21",
      "sidebar-foreground": "#F3F2F0",
      "sidebar-primary": "#FF6A2B",
      "sidebar-primary-foreground": "#FFFFFF",
      "sidebar-accent": "#23252A",
      "sidebar-accent-foreground": "#F3F2F0",
      "sidebar-border": "#2E3036",
      "sidebar-ring": "#FF6A2B"
    }
  }
}"##;

/// The Stardust theme.
pub fn stardust() -> nsh_base::Theme {
    nsh_base::Theme::from_json(STARDUST_THEME).expect("the Stardust theme is JSON")
}

/// Draws the app with Stardust's tokens (call once, in the root component).
pub fn install() {
    set_tokens(stardust());
}

#[cfg(test)]
mod tests {
    use super::*;

    fn close(a: Hsla, b: Hsla) -> bool {
        let a: Rgba = a.into();
        let b: Rgba = b.into();
        (a.r - b.r).abs() < 0.01
            && (a.g - b.g).abs() < 0.01
            && (a.b - b.b).abs() < 0.01
            && (a.a - b.a).abs() < 0.01
    }

    #[test]
    fn the_theme_file_reads_without_dropping_anything() {
        let (_, ignored) = nsh_base::Theme::from_json_lenient(STARDUST_THEME).unwrap();
        assert!(ignored.is_empty(), "ignored: {ignored:?}");
    }

    #[test]
    fn tokens_and_palette_agree() {
        let theme = stardust();
        for (token, color) in [
            (theme.primary, PALETTE.accent),
            (theme.background, PALETTE.canvas),
            (theme.sidebar, PALETTE.sidebar),
            (theme.foreground, PALETTE.ink),
            (theme.border, PALETTE.line),
            (theme.destructive, PALETTE.danger),
        ] {
            assert!(close(token.light, color.light), "{token:?} vs {color:?}");
            assert!(close(token.dark, color.dark), "{token:?} vs {color:?}");
        }
    }

    #[test]
    fn stardust_keys_match_the_desktop_spec() {
        let hex = |c: Hsla| {
            let c: Rgba = c.into();
            let byte = |v: f32| (v * 255.0).round() as u32;
            (byte(c.r) << 16) | (byte(c.g) << 8) | byte(c.b)
        };
        assert_eq!(hex(PALETTE.sidebar.light), 0xF6F5F3);
        assert_eq!(hex(PALETTE.canvas.light), 0xFFFFFF);
        assert_eq!(hex(PALETTE.sidebar.dark), 0x1C1D21);
        assert_eq!(hex(PALETTE.canvas.dark), 0x16171A);
        assert_eq!(hex(PALETTE.accent.light), 0xFF5A1F);
        assert_eq!(hex(PALETTE.accent.dark), 0xFF6A2B);
    }
}
