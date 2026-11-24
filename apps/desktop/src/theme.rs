//! Stardust（星尘）: Vantry's visual language on NSH.
//!
//! Two layers, both from one palette:
//! - [`STARDUST_THEME`], a shadcn `cssVars` theme file the app installs with
//!   [`install`], so NSH's tokens (`theme().primary`, Tailwind's
//!   `bg-primary`) and the platform's own controls follow Stardust;
//! - [`Palette`], typed [`AdaptiveColor`]s for what shadcn has no token for
//!   (the user bubble, the amber approval card, status dots).
//!
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
    pub raised: AdaptiveColor,
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
    /// The primary accent: send, focus, the active thread.
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
    /// Subtle tinted pills (auto-policy blue, completed green).
    pub info_bg: AdaptiveColor,
    pub info_ink: AdaptiveColor,
    pub success_bg: AdaptiveColor,
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

/// The default Graphite preset from Vantry v0.1.42.
pub const PALETTE: Palette = Palette {
    canvas: AdaptiveColor::rgb(0xFCFCFC, 0x0A0A0A),
    sidebar: AdaptiveColor::rgb(0xF3F4F6, 0x111214),
    card: AdaptiveColor::rgb(0xFFFFFF, 0x202229),
    raised: AdaptiveColor::rgb(0xFFFFFF, 0x17181C),
    soft: AdaptiveColor::rgb(0xF0F2F5, 0x181A1F),
    code: AdaptiveColor::rgb(0xF4F5F7, 0x060607),
    ink: AdaptiveColor::rgb(0x111318, 0xF5F6F8),
    muted: AdaptiveColor::rgb(0x525866, 0xA3A8B4),
    faint: AdaptiveColor::rgb(0x737A8A, 0x7D8392),
    line: AdaptiveColor::rgb(0xDFE3EA, 0x272A33),
    line_soft: AdaptiveColor::rgb(0xEAEDF2, 0x1E2028),
    accent: AdaptiveColor::rgb(0x1B4ED8, 0x346BF1),
    user: AdaptiveColor::rgb(0xF0F2F5, 0x1E212B),
    success: AdaptiveColor::rgb(0x1F9D4C, 0x30D158),
    warning: AdaptiveColor::rgb(0xB25E00, 0xF5A524),
    danger: AdaptiveColor::rgb(0xC9252C, 0xFF5A5F),
    on_accent: AdaptiveColor::rgb(0xFFFFFF, 0xFFFFFF),
    approval: AdaptiveColor::rgb(0xFFF7E8, 0x2B2416),
    approval_border: AdaptiveColor::rgb(0xF2C46D, 0x6B5220),
    info_bg: AdaptiveColor::rgb(0xEBF3FF, 0x1B2B44),
    info_ink: AdaptiveColor::rgb(0x2563EB, 0x60A5FA),
    success_bg: AdaptiveColor::rgb(0xE8F7EE, 0x163123),
    scrim: rgba_pair(0x1F1E1C59, 0x00000099),
};

const OCEAN: Palette = Palette {
    canvas: AdaptiveColor::rgb(0xF5F7F8, 0x17212B),
    sidebar: AdaptiveColor::rgb(0xE8EFF4, 0x121A22),
    card: AdaptiveColor::rgb(0xFFFFFF, 0x233544),
    raised: AdaptiveColor::rgb(0xFFFFFF, 0x233544),
    soft: AdaptiveColor::rgb(0xEBF1F6, 0x1E2C3A),
    code: AdaptiveColor::rgb(0xE4ECF2, 0x101820),
    ink: AdaptiveColor::rgb(0x13222F, 0xECF3F8),
    muted: AdaptiveColor::rgb(0x465B6B, 0x9BB2C4),
    faint: AdaptiveColor::rgb(0x6A8091, 0x7690A5),
    line: AdaptiveColor::rgb(0xCFDBE4, 0x2D4153),
    line_soft: AdaptiveColor::rgb(0xDEE8F0, 0x223241),
    accent: AdaptiveColor::rgb(0x0A6F75, 0x5BD0D6),
    user: AdaptiveColor::rgb(0xE8EFF4, 0x233544),
    success: AdaptiveColor::rgb(0x15803D, 0x34D399),
    warning: AdaptiveColor::rgb(0xB45309, 0xF5A524),
    danger: AdaptiveColor::rgb(0xC9252C, 0xFB7185),
    on_accent: AdaptiveColor::rgb(0xFFFFFF, 0x0E1116),
    ..PALETTE
};

const EMBER: Palette = Palette {
    canvas: AdaptiveColor::rgb(0xFAF5F2, 0x291E1A),
    sidebar: AdaptiveColor::rgb(0xF2E7E1, 0x221815),
    card: AdaptiveColor::rgb(0xFFFDFC, 0x432E23),
    raised: AdaptiveColor::rgb(0xFFFDFC, 0x432E23),
    soft: AdaptiveColor::rgb(0xEFE3DC, 0x352620),
    code: AdaptiveColor::rgb(0xF0E5DF, 0x1D1411),
    ink: AdaptiveColor::rgb(0x261914, 0xFAF0EB),
    muted: AdaptiveColor::rgb(0x6B5146, 0xC8AEA2),
    faint: AdaptiveColor::rgb(0x8C6F63, 0x9E8275),
    line: AdaptiveColor::rgb(0xE2CFC5, 0x543B2E),
    line_soft: AdaptiveColor::rgb(0xECE0D8, 0x3E2B22),
    accent: AdaptiveColor::rgb(0xC44D3A, 0xF78A7A),
    user: AdaptiveColor::rgb(0xF2E7E1, 0x432E23),
    success: AdaptiveColor::rgb(0x1F9D4C, 0x4ADE80),
    warning: AdaptiveColor::rgb(0xB25E00, 0xFBBF24),
    danger: AdaptiveColor::rgb(0xC9252C, 0xFB7185),
    on_accent: AdaptiveColor::rgb(0xFFFFFF, 0x0E1116),
    ..PALETTE
};

const IRIS: Palette = Palette {
    canvas: AdaptiveColor::rgb(0xF7F5FA, 0x1B1726),
    sidebar: AdaptiveColor::rgb(0xEDE8F5, 0x15121F),
    card: AdaptiveColor::rgb(0xFFFFFF, 0x2B243D),
    raised: AdaptiveColor::rgb(0xFFFFFF, 0x2B243D),
    soft: AdaptiveColor::rgb(0xEAE4F3, 0x241E34),
    code: AdaptiveColor::rgb(0xEAE5F2, 0x120F1B),
    ink: AdaptiveColor::rgb(0x1F182B, 0xF2EEF9),
    muted: AdaptiveColor::rgb(0x5A4F6E, 0xB5AAD0),
    faint: AdaptiveColor::rgb(0x7B6F91, 0x8D80AD),
    line: AdaptiveColor::rgb(0xDCD4EB, 0x3B3154),
    line_soft: AdaptiveColor::rgb(0xE7E1F2, 0x2D2542),
    accent: AdaptiveColor::rgb(0x6F42C1, 0xB48CF2),
    user: AdaptiveColor::rgb(0xEDE8F5, 0x2B243D),
    success: AdaptiveColor::rgb(0x1F9D4C, 0x34D399),
    warning: AdaptiveColor::rgb(0xB25E00, 0xF5A524),
    danger: AdaptiveColor::rgb(0xC9252C, 0xFB7185),
    on_accent: AdaptiveColor::rgb(0xFFFFFF, 0x0E1116),
    ..PALETTE
};

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum ThemePreset {
    Ocean,
    Ember,
    Iris,
    #[default]
    #[serde(other)]
    Graphite,
}

impl ThemePreset {
    pub const ALL: [Self; 4] = [Self::Graphite, Self::Ocean, Self::Ember, Self::Iris];

    pub fn label(self) -> &'static str {
        match self {
            Self::Graphite => "石墨",
            Self::Ocean => "深海",
            Self::Ember => "余烬",
            Self::Iris => "暮紫",
        }
    }

    pub fn colors(self) -> &'static Palette {
        match self {
            Self::Graphite => &PALETTE,
            Self::Ocean => &OCEAN,
            Self::Ember => &EMBER,
            Self::Iris => &IRIS,
        }
    }
}

/// Colors for the mounted workbench, including views opened later in this window.
pub(crate) fn palette() -> &'static Palette {
    use_context::<ThemePreset>().unwrap_or_default().colors()
}

/// Layout and type scale (points).
pub mod metrics {
    /// The reading column's width.
    pub const READING_WIDTH: f32 = 760.0;
    pub const NARROW_WIDTH: f32 = 860.0;
    pub const DOCK_SIDE_BREAKPOINT: f32 = 900.0;

    pub fn sidebar_width(window: f32) -> f32 {
        if window < NARROW_WIDTH {
            window
        } else if window < 1180.0 {
            248.0
        } else {
            272.0
        }
    }
    /// The header's height (room for the macOS window buttons).
    pub const HEADER_HEIGHT: f32 = 46.0;
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
      "background": "#FCFCFC",
      "foreground": "#111318",
      "card": "#FFFFFF",
      "card-foreground": "#111318",
      "popover": "#FFFFFF",
      "popover-foreground": "#111318",
      "primary": "#1B4ED8",
      "primary-foreground": "#FFFFFF",
      "secondary": "#F0F2F5",
      "secondary-foreground": "#111318",
      "muted": "#F0F2F5",
      "muted-foreground": "#525866",
      "accent": "#F0F2F5",
      "accent-foreground": "#111318",
      "destructive": "#C9252C",
      "border": "#DFE3EA",
      "input": "#DFE3EA",
      "ring": "#1B4ED8",
      "sidebar": "#F3F4F6",
      "sidebar-foreground": "#111318",
      "sidebar-primary": "#1B4ED8",
      "sidebar-primary-foreground": "#FFFFFF",
      "sidebar-accent": "#FFFFFF",
      "sidebar-accent-foreground": "#111318",
      "sidebar-border": "#DFE3EA",
      "sidebar-ring": "#1B4ED8"
    },
    "dark": {
      "background": "#0A0A0A",
      "foreground": "#F5F6F8",
      "card": "#17181C",
      "card-foreground": "#F5F6F8",
      "popover": "#17181C",
      "popover-foreground": "#F5F6F8",
      "primary": "#346BF1",
      "primary-foreground": "#FFFFFF",
      "secondary": "#181A1F",
      "secondary-foreground": "#F5F6F8",
      "muted": "#181A1F",
      "muted-foreground": "#A3A8B4",
      "accent": "#181A1F",
      "accent-foreground": "#F5F6F8",
      "destructive": "#FF5A5F",
      "border": "#272A33",
      "input": "#272A33",
      "ring": "#346BF1",
      "sidebar": "#111214",
      "sidebar-foreground": "#F5F6F8",
      "sidebar-primary": "#346BF1",
      "sidebar-primary-foreground": "#FFFFFF",
      "sidebar-accent": "#202229",
      "sidebar-accent-foreground": "#F5F6F8",
      "sidebar-border": "#272A33",
      "sidebar-ring": "#346BF1"
    }
  }
}"##;

/// The Stardust theme.
pub fn stardust() -> nsh_base::Theme {
    nsh_base::Theme::from_json(STARDUST_THEME).expect("the Stardust theme is JSON")
}

/// Installs the default theme for embedders.
pub fn install() {
    set_tokens(stardust());
}

pub(crate) fn install_preset(preset: ThemePreset) {
    provide_context(preset);
    let p = preset.colors();
    let mut tokens = stardust().into_tokens();
    for (name, color) in [
        ("background", p.canvas),
        ("foreground", p.ink),
        ("card", p.raised),
        ("card-foreground", p.ink),
        ("popover", p.raised),
        ("popover-foreground", p.ink),
        ("primary", p.accent),
        ("primary-foreground", p.on_accent),
        ("secondary", p.soft),
        ("secondary-foreground", p.ink),
        ("muted", p.soft),
        ("muted-foreground", p.muted),
        ("accent", p.soft),
        ("accent-foreground", p.ink),
        ("destructive", p.danger),
        ("border", p.line),
        ("input", p.line),
        ("ring", p.accent),
        ("sidebar", p.sidebar),
        ("sidebar-foreground", p.ink),
        ("sidebar-primary", p.accent),
        ("sidebar-primary-foreground", p.on_accent),
        ("sidebar-accent", p.card),
        ("sidebar-accent-foreground", p.ink),
        ("sidebar-border", p.line),
        ("sidebar-ring", p.accent),
    ] {
        tokens.set_color(name, color);
    }
    set_tokens(tokens);
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
    fn graphite_matches_the_released_desktop() {
        let hex = |c: Hsla| {
            let c: Rgba = c.into();
            let byte = |v: f32| (v * 255.0).round() as u32;
            (byte(c.r) << 16) | (byte(c.g) << 8) | byte(c.b)
        };
        assert_eq!(hex(PALETTE.sidebar.light), 0xF3F4F6);
        assert_eq!(hex(PALETTE.canvas.light), 0xFCFCFC);
        assert_eq!(hex(PALETTE.sidebar.dark), 0x111214);
        assert_eq!(hex(PALETTE.canvas.dark), 0x0A0A0A);
        assert_eq!(hex(PALETTE.accent.light), 0x1B4ED8);
        assert_eq!(hex(PALETTE.accent.dark), 0x346BF1);
    }
}
