//! Lucide v0.564.0 (ISC) vector icons rendered natively via NSH's `Canvas`
//! `Painter` at device pixels, inheriting `currentColor` from `.text_color(…)`
//! so every glyph adapts automatically to Stardust light/dark themes.
//!
//! Zero external SVG/resvg dependencies: each Lucide icon uses a 24×24 viewBox
//! with 2px round-capped strokes drawn directly with CoreGraphics / Skia.

use std::f32::consts::TAU;

use nsh_core::{
    SvgPaint,
    canvas::{LineCap, LineJoin},
    prelude::*,
};

/// Named Lucide icons ported from Vantry `v0.1.42` (`internal/desktop/lucide_icons.go`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
pub enum Icon {
    Archive,
    ArrowDown,
    ArrowLeft,
    ArrowLeftRight,
    ArrowRight,
    ArrowUp,
    Blocks,
    Bot,
    Brain,
    ChartBar,
    Check,
    ChevronDown,
    ChevronRight,
    ChevronUp,
    CircleAlert,
    CircleCheck,
    CircleHelp,
    Clock,
    Code,
    Copy,
    Download,
    Ellipsis,
    ExternalLink,
    Eye,
    File,
    FileDiff,
    FileText,
    Folder,
    FolderOpen,
    FolderPlus,
    GitBranch,
    GitCommit,
    GitFork,
    GitPullRequest,
    Inbox,
    Layers,
    ListChecks,
    ListFilter,
    ListTodo,
    Maximize2,
    Minimize2,
    Minus,
    Monitor,
    Moon,
    PanelLeft,
    PanelRight,
    Paperclip,
    Pencil,
    Pin,
    PinOff,
    Plug,
    Plus,
    Quote,
    RefreshCw,
    RotateCcw,
    Search,
    Send,
    Server,
    Settings,
    Shield,
    ShieldCheck,
    ShieldOff,
    SlidersHorizontal,
    Sparkles,
    Square,
    SquarePen,
    SquareTerminal,
    Sun,
    Terminal,
    Trash2,
    TriangleAlert,
    Undo2,
    Upload,
    Wrench,
    X,
    Zap,
}

impl Icon {
    /// Lucide SVG inner elements on the canonical 24×24 viewBox.
    pub fn markup(self) -> &'static str {
        match self {
            Icon::Archive => r#"<rect width="20" height="5" x="2" y="3" rx="1"/><path d="M4 8v11a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8"/><path d="M10 12h4"/>"#,
            Icon::ArrowDown => r#"<path d="M12 5v14"/><path d="m19 12-7 7-7-7"/>"#,
            Icon::ArrowLeft => r#"<path d="m12 19-7-7 7-7"/><path d="M19 12H5"/>"#,
            Icon::ArrowLeftRight => r#"<path d="M8 3 4 7l4 4"/><path d="M4 7h16"/><path d="m16 21 4-4-4-4"/><path d="M20 17H4"/>"#,
            Icon::ArrowRight => r#"<path d="M5 12h14"/><path d="m12 5 7 7-7 7"/>"#,
            Icon::ArrowUp => r#"<path d="m5 12 7-7 7 7"/><path d="M12 19V5"/>"#,
            Icon::Blocks => r#"<path d="M10 22V7a1 1 0 0 0-1-1H4a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-5a1 1 0 0 0-1-1H2"/><rect x="14" y="2" width="8" height="8" rx="1"/>"#,
            Icon::Bot => r#"<path d="M12 8V4H8"/><rect width="16" height="12" x="4" y="8" rx="2"/><path d="M2 14h2"/><path d="M20 14h2"/><path d="M15 13v2"/><path d="M9 13v2"/>"#,
            Icon::Brain => r#"<path d="M12 18V5"/><path d="M15 13a4.17 4.17 0 0 1-3-4 4.17 4.17 0 0 1-3 4"/><path d="M17.598 6.5A3 3 0 1 0 12 5a3 3 0 1 0-5.598 1.5"/><path d="M17.997 5.125a4 4 0 0 1 2.526 5.77"/><path d="M18 18a4 4 0 0 0 2-7.464"/><path d="M19.967 17.483A4 4 0 1 1 12 18a4 4 0 1 1-7.967-.517"/><path d="M6 18a4 4 0 0 1-2-7.464"/><path d="M6.003 5.125a4 4 0 0 0-2.526 5.77"/>"#,
            Icon::ChartBar => r#"<path d="M5 21v-6"/><path d="M12 21V3"/><path d="M19 21V9"/>"#,
            Icon::Check => r#"<path d="M20 6 9 17l-5-5"/>"#,
            Icon::ChevronDown => r#"<path d="m6 9 6 6 6-6"/>"#,
            Icon::ChevronRight => r#"<path d="m9 18 6-6-6-6"/>"#,
            Icon::ChevronUp => r#"<path d="m18 15-6-6-6 6"/>"#,
            Icon::CircleAlert => r#"<circle cx="12" cy="12" r="10"/><line x1="12" x2="12" y1="8" y2="12"/><line x1="12" x2="12.01" y1="16" y2="16"/>"#,
            Icon::CircleCheck => r#"<circle cx="12" cy="12" r="10"/><path d="m9 12 2 2 4-4"/>"#,
            Icon::CircleHelp => r#"<circle cx="12" cy="12" r="10"/><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"/><path d="M12 17h.01"/>"#,
            Icon::Clock => r#"<path d="M12 6v6l4 2"/><circle cx="12" cy="12" r="10"/>"#,
            Icon::Code => r#"<path d="m16 18 6-6-6-6"/><path d="m8 6-6 6 6 6"/>"#,
            Icon::Copy => r#"<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>"#,
            Icon::Download => r#"<path d="M12 15V3"/><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/><path d="m7 10 5 5 5-5"/>"#,
            Icon::Ellipsis => r#"<circle cx="12" cy="12" r="1"/><circle cx="19" cy="12" r="1"/><circle cx="5" cy="12" r="1"/>"#,
            Icon::ExternalLink => r#"<path d="M15 3h6v6"/><path d="M10 14 21 3"/><path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/>"#,
            Icon::Eye => r#"<path d="M2.062 12.348a1 1 0 0 1 0-.696 10.75 10.75 0 0 1 19.876 0 1 1 0 0 1 0 .696 10.75 10.75 0 0 1-19.876 0"/><circle cx="12" cy="12" r="3"/>"#,
            Icon::File => r#"<path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M14 2v5a1 1 0 0 0 1 1h5"/>"#,
            Icon::FileDiff => r#"<path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M9 10h6"/><path d="M12 13V7"/><path d="M9 17h6"/>"#,
            Icon::FileText => r#"<path d="M6 22a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h8a2.4 2.4 0 0 1 1.704.706l3.588 3.588A2.4 2.4 0 0 1 20 8v12a2 2 0 0 1-2 2z"/><path d="M14 2v5a1 1 0 0 0 1 1h5"/><path d="M10 9H8"/><path d="M16 13H8"/><path d="M16 17H8"/>"#,
            Icon::Folder => r#"<path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>"#,
            Icon::FolderOpen => r#"<path d="m6 14 1.5-2.9A2 2 0 0 1 9.24 10H20a2 2 0 0 1 1.94 2.5l-1.54 6a2 2 0 0 1-1.95 1.5H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h3.9a2 2 0 0 1 1.69.9l.81 1.2a2 2 0 0 0 1.67.9H18a2 2 0 0 1 2 2v2"/>"#,
            Icon::FolderPlus => r#"<path d="M12 10v6"/><path d="M9 13h6"/><path d="M20 20a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.69-.9L9.6 3.9A2 2 0 0 0 7.93 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2Z"/>"#,
            Icon::GitBranch => r#"<path d="M15 6a9 9 0 0 0-9 9V3"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/>"#,
            Icon::GitCommit => r#"<circle cx="12" cy="12" r="3"/><line x1="3" x2="9" y1="12" y2="12"/><line x1="15" x2="21" y1="12" y2="12"/>"#,
            Icon::GitFork => r#"<circle cx="12" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><circle cx="18" cy="6" r="3"/><path d="M18 9v2c0 .6-.4 1-1 1H7c-.6 0-1-.4-1-1V9"/><path d="M12 12v3"/>"#,
            Icon::GitPullRequest => r#"<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M13 6h3a2 2 0 0 1 2 2v7"/><line x1="6" x2="6" y1="9" y2="21"/>"#,
            Icon::Inbox => r#"<polyline points="22 12 16 12 14 15 10 15 8 12 2 12"/><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/>"#,
            Icon::Layers => r#"<path d="M12.83 2.18a2 2 0 0 0-1.66 0L2.6 6.08a1 1 0 0 0 0 1.83l8.58 3.91a2 2 0 0 0 1.66 0l8.58-3.9a1 1 0 0 0 0-1.83z"/><path d="M2 12a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 12"/><path d="M2 17a1 1 0 0 0 .58.91l8.6 3.91a2 2 0 0 0 1.65 0l8.58-3.9A1 1 0 0 0 22 17"/>"#,
            Icon::ListChecks => r#"<path d="M13 5h8"/><path d="M13 12h8"/><path d="M13 19h8"/><path d="m3 17 2 2 4-4"/><path d="m3 7 2 2 4-4"/>"#,
            Icon::ListFilter => r#"<path d="M2 5h20"/><path d="M6 12h12"/><path d="M9 19h6"/>"#,
            Icon::ListTodo => r#"<path d="M13 5h8"/><path d="M13 12h8"/><path d="M13 19h8"/><path d="m3 17 2 2 4-4"/><rect x="3" y="4" width="6" height="6" rx="1"/>"#,
            Icon::Maximize2 => r#"<path d="M15 3h6v6"/><path d="m21 3-7 7"/><path d="m3 21 7-7"/><path d="M9 21H3v-6"/>"#,
            Icon::Minimize2 => r#"<path d="m14 10 7-7"/><path d="M20 10h-6V4"/><path d="m3 21 7-7"/><path d="M4 14h6v6"/>"#,
            Icon::Minus => r#"<path d="M5 12h14"/>"#,
            Icon::Monitor => r#"<rect width="20" height="14" x="2" y="3" rx="2"/><line x1="8" x2="16" y1="21" y2="21"/><line x1="12" x2="12" y1="17" y2="21"/>"#,
            Icon::Moon => r#"<path d="M20.985 12.486a9 9 0 1 1-9.473-9.472c.405-.022.617.46.402.803a6 6 0 0 0 8.268 8.268c.344-.215.825-.004.803.401"/>"#,
            Icon::PanelLeft => r#"<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/>"#,
            Icon::PanelRight => r#"<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M15 3v18"/>"#,
            Icon::Paperclip => r#"<path d="m16 6-8.414 8.586a2 2 0 0 0 2.829 2.829l8.414-8.586a4 4 0 1 0-5.657-5.657l-8.379 8.551a6 6 0 1 0 8.485 8.485l8.379-8.551"/>"#,
            Icon::Pencil => r#"<path d="M21.174 6.812a1 1 0 0 0-3.986-3.987L3.842 16.174a2 2 0 0 0-.5.83l-1.321 4.352a.5.5 0 0 0 .623.622l4.353-1.32a2 2 0 0 0 .83-.497z"/><path d="m15 5 4 4"/>"#,
            Icon::Pin => r#"<path d="M12 17v5"/><path d="M9 10.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-.76a2 2 0 0 0-1.11-1.79l-1.78-.9A2 2 0 0 1 15 10.76V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H8a2 2 0 0 0 0 4 1 1 0 0 1 1 1z"/>"#,
            Icon::PinOff => r#"<path d="M12 17v5"/><path d="M15 9.34V7a1 1 0 0 1 1-1 2 2 0 0 0 0-4H7.89"/><path d="m2 2 20 20"/><path d="M9 9v1.76a2 2 0 0 1-1.11 1.79l-1.78.9A2 2 0 0 0 5 15.24V16a1 1 0 0 0 1 1h11"/>"#,
            Icon::Plug => r#"<path d="M12 22v-5"/><path d="M15 8V2"/><path d="M17 8a1 1 0 0 1 1 1v4a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V9a1 1 0 0 1 1-1z"/><path d="M9 8V2"/>"#,
            Icon::Plus => r#"<path d="M5 12h14"/><path d="M12 5v14"/>"#,
            Icon::Quote => r#"<path d="M16 3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2 1 1 0 0 1 1 1v1a2 2 0 0 1-2 2 1 1 0 0 0-1 1v2a1 1 0 0 0 1 1 6 6 0 0 0 6-6V5a2 2 0 0 0-2-2z"/><path d="M5 3a2 2 0 0 0-2 2v6a2 2 0 0 0 2 2 1 1 0 0 1 1 1v1a2 2 0 0 1-2 2 1 1 0 0 0-1 1v2a1 1 0 0 0 1 1 6 6 0 0 0 6-6V5a2 2 0 0 0-2-2z"/>"#,
            Icon::RefreshCw => r#"<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>"#,
            Icon::RotateCcw => r#"<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"/><path d="M3 3v5h5"/>"#,
            Icon::Search => r#"<path d="m21 21-4.34-4.34"/><circle cx="11" cy="11" r="8"/>"#,
            Icon::Send => r#"<path d="M14.536 21.686a.5.5 0 0 0 .937-.024l6.5-19a.496.496 0 0 0-.635-.635l-19 6.5a.5.5 0 0 0-.024.937l7.93 3.18a2 2 0 0 1 1.112 1.11z"/><path d="m21.854 2.147-10.94 10.939"/>"#,
            Icon::Server => r#"<rect width="20" height="8" x="2" y="2" rx="2" ry="2"/><rect width="20" height="8" x="2" y="14" rx="2" ry="2"/><line x1="6" x2="6.01" y1="6" y2="6"/><line x1="6" x2="6.01" y1="18" y2="18"/>"#,
            Icon::Settings => r#"<path d="M9.671 4.136a2.34 2.34 0 0 1 4.659 0 2.34 2.34 0 0 0 3.319 1.915 2.34 2.34 0 0 1 2.33 4.033 2.34 2.34 0 0 0 0 3.831 2.34 2.34 0 0 1-2.33 4.033 2.34 2.34 0 0 0-3.319 1.915 2.34 2.34 0 0 1-4.659 0 2.34 2.34 0 0 0-3.32-1.915 2.34 2.34 0 0 1-2.33-4.033 2.34 2.34 0 0 0 0-3.831A2.34 2.34 0 0 1 6.35 6.051a2.34 2.34 0 0 0 3.319-1.915"/><circle cx="12" cy="12" r="3"/>"#,
            Icon::Shield => r#"<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/>"#,
            Icon::ShieldCheck => r#"<path d="M20 13c0 5-3.5 7.5-7.66 8.95a1 1 0 0 1-.67-.01C7.5 20.5 4 18 4 13V6a1 1 0 0 1 1-1c2 0 4.5-1.2 6.24-2.72a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1z"/><path d="m9 12 2 2 4-4"/>"#,
            Icon::ShieldOff => r#"<path d="m2 2 20 20"/><path d="M5 5a1 1 0 0 0-1 1v7c0 5 3.5 7.5 7.67 8.94a1 1 0 0 0 .67.01c2.35-.82 4.48-1.97 5.9-3.71"/><path d="M9.309 3.652A12.252 12.252 0 0 0 11.24 2.28a1.17 1.17 0 0 1 1.52 0C14.51 3.81 17 5 19 5a1 1 0 0 1 1 1v7a9.784 9.784 0 0 1-.08 1.264"/>"#,
            Icon::SlidersHorizontal => r#"<line x1="21" x2="14" y1="4" y2="4"/><line x1="10" x2="3" y1="4" y2="4"/><line x1="21" x2="12" y1="12" y2="12"/><line x1="8" x2="3" y1="12" y2="12"/><line x1="21" x2="16" y1="20" y2="20"/><line x1="12" x2="3" y1="20" y2="20"/><line x1="14" x2="14" y1="2" y2="6"/><line x1="8" x2="8" y1="10" y2="14"/><line x1="16" x2="16" y1="18" y2="22"/>"#,
            Icon::Sparkles => r#"<path d="M11.017 2.814a1 1 0 0 1 1.966 0l1.051 5.558a2 2 0 0 0 1.594 1.594l5.558 1.051a1 1 0 0 1 0 1.966l-5.558 1.051a2 2 0 0 0-1.594 1.594l-1.051 5.558a1 1 0 0 1-1.966 0l-1.051-5.558a2 2 0 0 0-1.594-1.594l-5.558-1.051a1 1 0 0 1 0-1.966l5.558-1.051a2 2 0 0 0 1.594-1.594z"/><path d="M20 2v4"/><path d="M22 4h-4"/><circle cx="4" cy="20" r="2"/>"#,
            Icon::Square => r#"<rect width="18" height="18" x="3" y="3" rx="2"/>"#,
            Icon::SquarePen => r#"<path d="M12 3H5a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7"/><path d="M18.375 2.625a1 1 0 0 1 3 3l-9.013 9.014a2 2 0 0 1-.853.505l-2.873.84a.5.5 0 0 1-.62-.62l.84-2.873a2 2 0 0 1 .506-.852z"/>"#,
            Icon::SquareTerminal => r#"<path d="m7 11 2-2-2-2"/><path d="M11 13h4"/><rect width="18" height="18" x="3" y="3" rx="2" ry="2"/>"#,
            Icon::Sun => r#"<circle cx="12" cy="12" r="4"/><path d="M12 2v2"/><path d="M12 20v2"/><path d="m4.93 4.93 1.41 1.41"/><path d="m17.66 17.66 1.41 1.41"/><path d="M2 12h2"/><path d="M20 12h2"/><path d="m6.34 17.66-1.41 1.41"/><path d="m19.07 4.93-1.41 1.41"/>"#,
            Icon::Terminal => r#"<path d="M12 19h8"/><path d="m4 17 6-6-6-6"/>"#,
            Icon::Trash2 => r#"<path d="M10 11v6"/><path d="M14 11v6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M3 6h18"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>"#,
            Icon::TriangleAlert => r#"<path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3"/><path d="M12 9v4"/><path d="M12 17h.01"/>"#,
            Icon::Undo2 => r#"<path d="M9 14 4 9l5-5"/><path d="M4 9h10.5a5.5 5.5 0 0 1 5.5 5.5a5.5 5.5 0 0 1-5.5 5.5H11"/>"#,
            Icon::Upload => r#"<path d="M12 3v12"/><path d="m17 8-5-5-5 5"/><path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/>"#,
            Icon::Wrench => r#"<path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.106-3.105c.32-.322.863-.22.983.218a6 6 0 0 1-8.259 7.057l-7.91 7.91a1 1 0 0 1-2.999-3l7.91-7.91a6 6 0 0 1 7.057-8.259c.438.12.54.662.219.984z"/>"#,
            Icon::X => r#"<path d="M18 6 6 18"/><path d="m6 6 12 12"/>"#,
            Icon::Zap => r#"<path d="M4 14a1 1 0 0 1-.78-1.63l9.9-10.2a.5.5 0 0 1 .86.46l-1.92 6.02A1 1 0 0 0 13 10h7a1 1 0 0 1 .78 1.63l-9.9 10.2a.5.5 0 0 1-.86-.46l1.92-6.02A1 1 0 0 0 11 14z"/>"#,
        }
    }
}

/// Draws `glyph` at `size` × `size` pt with `color` (reactive `AdaptiveColor`),
/// using Lucide's 2px round-capped stroke on its 24×24 coordinate space.
pub fn icon(
    glyph: Icon,
    size: f32,
    color: impl IntoReactive<AdaptiveColor>,
) -> CanvasView {
    icon_stroke(glyph, size, 1.9, color)
}

/// Same as [`icon`] with an explicit stroke width in 24×24 viewBox units.
pub fn icon_stroke(
    glyph: Icon,
    size: f32,
    stroke_width: f32,
    color: impl IntoReactive<AdaptiveColor>,
) -> CanvasView {
    let painter = paint_lucide(glyph.markup(), size, stroke_width);
    canvas(painter)
        .w(px(size))
        .h(px(size))
        .flex_shrink_0()
        .text_color(color)
}

/// Builds a [`Painter`] for Lucide SVG primitives (`path`, `circle`, `rect`,
/// `line`, `polyline`), scaled cleanly from 24×24 to `size`×`size` pt.
pub fn paint_lucide(markup: &str, size: f32, stroke_width: f32) -> Painter {
    let scale = size / 24.0;
    let mut p = Painter::new()
        .save()
        .scale(scale, scale)
        .stroke_style_from_view(SvgPaint::CurrentColor)
        .fill_style_from_view(SvgPaint::CurrentColor)
        .line_width(stroke_width)
        .line_cap(LineCap::Round)
        .line_join(LineJoin::Round);

    let mut rest = markup;
    while let Some(start) = rest.find('<') {
        rest = &rest[start + 1..];
        let Some(end) = rest.find('>') else {
            break;
        };
        let tag_body = rest[..end].trim_end_matches('/').trim();
        rest = &rest[end + 1..];
        let mut parts = tag_body.splitn(2, char::is_whitespace);
        let tag = parts.next().unwrap_or("");
        let attrs = parts.next().unwrap_or("");
        match tag {
            "path" => {
                if let Some(d) = attr(attrs, "d") {
                    p = p.stroke_svg(d);
                }
            }
            "circle" => {
                let cx = attr_f32(attrs, "cx").unwrap_or(0.0);
                let cy = attr_f32(attrs, "cy").unwrap_or(0.0);
                let r = attr_f32(attrs, "r").unwrap_or(0.0);
                let filled = attr(attrs, "fill") == Some("currentColor");
                p = p.begin_path().arc(cx, cy, r, 0.0_f32, TAU);
                p = if filled { p.fill() } else { p.stroke() };
            }
            "rect" => {
                let x = attr_f32(attrs, "x").unwrap_or(0.0);
                let y = attr_f32(attrs, "y").unwrap_or(0.0);
                let w = attr_f32(attrs, "width").unwrap_or(0.0);
                let h = attr_f32(attrs, "height").unwrap_or(0.0);
                let rx = attr_f32(attrs, "rx").or_else(|| attr_f32(attrs, "ry")).unwrap_or(0.0);
                let rect = Bounds::new(point(px(x), px(y)), nsh_core::prelude::size(px(w), px(h)));
                p = p.begin_path();
                p = if rx > 0.0 {
                    p.round_rect(rect, px(rx))
                } else {
                    p.rect(rect)
                };
                p = p.stroke();
            }
            "line" => {
                let x1 = attr_f32(attrs, "x1").unwrap_or(0.0);
                let y1 = attr_f32(attrs, "y1").unwrap_or(0.0);
                let x2 = attr_f32(attrs, "x2").unwrap_or(0.0);
                let y2 = attr_f32(attrs, "y2").unwrap_or(0.0);
                p = p.begin_path().move_to(x1, y1).line_to(x2, y2).stroke();
            }
            "polyline" => {
                if let Some(pts) = attr(attrs, "points") {
                    let nums: Vec<f32> = pts
                        .split(|c: char| c.is_whitespace() || c == ',')
                        .filter(|s| !s.is_empty())
                        .filter_map(|s| s.parse::<f32>().ok())
                        .collect();
                    if nums.len() >= 4 {
                        p = p.begin_path().move_to(nums[0], nums[1]);
                        for pair in nums[2..].chunks_exact(2) {
                            p = p.line_to(pair[0], pair[1]);
                        }
                        p = p.stroke();
                    }
                }
            }
            _ => {}
        }
    }
    p.restore()
}

fn attr<'a>(attrs: &'a str, name: &str) -> Option<&'a str> {
    let mut rest = attrs;
    while !rest.is_empty() {
        rest = rest.trim_start();
        let eq = rest.find('=')?;
        let key = rest[..eq].trim();
        rest = rest[eq + 1..].trim_start();
        let quote = rest.as_bytes().first().copied()?;
        if quote != b'"' && quote != b'\'' {
            return None;
        }
        rest = &rest[1..];
        let end = rest.find(quote as char)?;
        let value = &rest[..end];
        rest = &rest[end + 1..];
        if key == name {
            return Some(value);
        }
    }
    None
}

fn attr_f32(attrs: &str, name: &str) -> Option<f32> {
    attr(attrs, name)?.parse::<f32>().ok()
}

/// Deterministic 10-color Stardust project monogram badge (`VY` pink tint, etc.),
/// matching `v0.1.42`'s sidebar and breadcrumb project badges.
pub fn project_badge(name: &str) -> View {
    let (bg, fg) = project_palette(name);
    let mono = project_monogram(name);
    view()
        .w(px(18.0))
        .h(px(18.0))
        .rounded(px(5.0))
        .flex_shrink_0()
        .items_center()
        .justify_center()
        .bg(bg)
        .child(
            text(mono)
                .text_size(px(9.0))
                .font_weight(FontWeight::BOLD)
                .text_color(fg),
        )
}

fn project_monogram(name: &str) -> String {
    let trimmed = name.trim();
    if trimmed.is_empty() {
        return "VT".to_owned();
    }
    let words: Vec<&str> = trimmed
        .split(|c: char| !c.is_alphanumeric())
        .filter(|w| !w.is_empty())
        .collect();
    if words.len() >= 2 {
        let a = words[0].chars().next().unwrap_or('V');
        let b = words[1].chars().next().unwrap_or('T');
        return format!("{a}{b}").to_uppercase();
    }
    let chars: Vec<char> = trimmed.chars().collect();
    if chars.len() >= 2 {
        // Pick first + last for clean Vantry -> VY style when ASCII word,
        // or first two characters otherwise.
        if chars.iter().all(|c| c.is_ascii_alphabetic()) && chars.len() >= 4 {
            return format!("{}{}", chars[0], chars[chars.len() - 1]).to_uppercase();
        }
        return format!("{}{}", chars[0], chars[1]).to_uppercase();
    }
    trimmed.to_uppercase()
}

fn project_palette(name: &str) -> (AdaptiveColor, AdaptiveColor) {
    // 10 harmonious Stardust pastel-tint / rich-ink pairs (light & dark).
    const HUES: [u32; 10] = [
        0xD99A06, 0xF26B1D, 0xE5484D, 0xE54D8A, 0x8E5CF0,
        0x5B63E8, 0x2F7FF0, 0x0EA5C6, 0x12A594, 0x30A46C,
    ];
    let mut hash: u32 = 2166136261;
    for b in name.bytes() {
        hash ^= b as u32;
        hash = hash.wrapping_mul(16777619);
    }
    let hue = HUES[(hash as usize) % HUES.len()];
    let ink = AdaptiveColor::rgb(hue, hue);
    (ink.opacity(0.2), ink)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn every_icon_parses_into_drawing_ops() {
        let all = [
            Icon::Archive,
            Icon::ArrowDown,
            Icon::ArrowLeft,
            Icon::ArrowRight,
            Icon::ArrowUp,
            Icon::Blocks,
            Icon::Bot,
            Icon::Brain,
            Icon::ChartBar,
            Icon::Check,
            Icon::ChevronDown,
            Icon::ChevronRight,
            Icon::ChevronUp,
            Icon::CircleAlert,
            Icon::CircleCheck,
            Icon::CircleHelp,
            Icon::Clock,
            Icon::Code,
            Icon::Copy,
            Icon::Ellipsis,
            Icon::ExternalLink,
            Icon::Eye,
            Icon::File,
            Icon::FileDiff,
            Icon::FileText,
            Icon::Folder,
            Icon::FolderOpen,
            Icon::FolderPlus,
            Icon::GitBranch,
            Icon::GitCommit,
            Icon::GitFork,
            Icon::GitPullRequest,
            Icon::Inbox,
            Icon::Layers,
            Icon::ListChecks,
            Icon::ListFilter,
            Icon::ListTodo,
            Icon::Maximize2,
            Icon::Minimize2,
            Icon::Monitor,
            Icon::Moon,
            Icon::PanelLeft,
            Icon::PanelRight,
            Icon::Paperclip,
            Icon::Pencil,
            Icon::Pin,
            Icon::PinOff,
            Icon::Plug,
            Icon::Plus,
            Icon::Quote,
            Icon::RefreshCw,
            Icon::RotateCcw,
            Icon::Search,
            Icon::Send,
            Icon::Server,
            Icon::Settings,
            Icon::Shield,
            Icon::ShieldCheck,
            Icon::ShieldOff,
            Icon::Sparkles,
            Icon::Square,
            Icon::SquarePen,
            Icon::SquareTerminal,
            Icon::Sun,
            Icon::Terminal,
            Icon::Trash2,
            Icon::TriangleAlert,
            Icon::Undo2,
            Icon::Wrench,
            Icon::X,
            Icon::Zap,
        ];
        for glyph in all {
            let drawing = paint_lucide(glyph.markup(), 16.0, 1.9).finish();
            assert!(
                drawing.ops.len() > 6,
                "icon {glyph:?} produced too few canvas ops: {}",
                drawing.ops.len()
            );
        }
    }

    #[test]
    fn project_monogram_formats_cleanly() {
        assert_eq!(project_monogram("Vantry"), "VY");
        assert_eq!(project_monogram("my-app"), "MA");
        assert_eq!(project_monogram(""), "VT");
    }
}
