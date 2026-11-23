package desktop

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/muq9738/Vantry/pkg/api"
)

const (
	narrowWidth        = 860
	sidebarWide        = 272.0
	sidebarNarrow      = 248.0
	compositionKeyHold = 150 * time.Millisecond
	// readingWidth is the conversation's measure: turns, the composer and
	// the new-thread hero share it, about 40 CJK characters or 80 Latin.
	readingWidth = float32(760)
)

type viewState struct {
	logs              map[string]*ui.ListState
	markdown          markdown.Options
	composerFocus     ui.Handle
	searchFocus       ui.Handle
	sidebarFocus      ui.Handle
	modelFocus        ui.Handle
	modelOptionsFocus ui.Handle
	composerMounted   bool
	navActivate       bool
	draggingThread    bool
	search            string
	searchOpen        bool
	sendWithEnter     bool
	dark              bool
	themeSet          bool
	themePreset   string
	// effortPending is the reasoning effort just picked, shown at once while
	// the thread's model change is still on its way to Core.
	effortPending effortPick
	// composerFooterW is the composer footer's width in the last frame, for
	// controls that compact themselves in a narrow reading column.
	composerFooterW     float32
	modelProviderFilter string
	statsPopoverOpen    bool
	usageTab            string
	resolvedTheme       *ui.Theme
	resolvedPalette     palette
	sidebarClosed       bool
	mobileNav           bool
	manualDelivery      bool
	modelSearch         string
	modelPickerOpen     bool
	projectPickerOpen   bool
	workspaceOpen       bool
	queueOpen           bool
	queueHistory        bool
	steerMode           bool
	toolExpanded        map[string]bool
	requestValues       map[string]string
	taskExpanded        map[string]bool
	diffOpen            bool
	diffLayout          int
	markdownPalette     palette
	reasoningOpen       map[string]bool
	workOpen            map[string]bool
	turnDiff            string
	diffState           ui.ListState
	shortcutsOpen       bool
	settingsOpen        bool
	settingsPage        string
	usageOpen           bool
	settledOpen         bool
	locale              string
	preferredEditor     string
	usage               usageState

	defaultToolsExpanded bool
	defaultReasoningOpen bool
	frameNow             time.Time
	frameSeq             uint64
	escHandledFrame      uint64
	escArmedAt           time.Time
	escArmedMode         string
	inboxOpen            bool
	rewind               rewindState
	fork                 forkDialogState
	sandbox              sandboxState
	piSetup              piSetupState
	envSettings          environmentSettingsState

	// projectFilter narrows the sidebar to one project's threads.
	projectFilter string
	// hoverThread is the sidebar row under the pointer this frame, for the
	// hover card; hoverRect is its window bounds.
	hoverThread string
	hoverRect   ui.Rect
	hoverSince  time.Time
	// snoozeOpen tracks each row's open snooze menu so its button stays.
	snoozeOpen map[string]*bool
	// sidebarMultiMachine is set while the sidebar lists rows from more than
	// one machine, so a row names its machine only when that disambiguates.
	sidebarMultiMachine bool
	thinkingOpen        bool
	// copiedRun is the reply whose copy button just confirmed, until
	// copiedAt plus a moment.
	copiedRun string
	copiedAt  time.Time
	// rightPanelTab selects the active tab in the right inspector panel
	// ("project" or "subtasks"). Empty shows the project.
	rightPanelTab string
	// orchestrationExpanded tracks whether the floating task orchestration card is expanded per thread.
	orchestrationExpanded map[string]bool
	// projectMoreOpen tracks whether a sidebar project group expanded past its preview cap.
	projectMoreOpen map[string]bool
	// attentionHold keeps a thread just selected from "Needs you" anchored in
	// place until the user moves to a different thread, preventing jarring jumps.
	attentionHold string
	// services opens links from callbacks that have no Context.
	services ui.Services
}

func (v *viewState) log(threadID string) *ui.ListState {
	if v.logs == nil {
		v.logs = map[string]*ui.ListState{}
	}
	if v.logs[threadID] == nil {
		v.logs[threadID] = &ui.ListState{}
	}
	return v.logs[threadID]
}
func (v *viewState) moveLog(oldID, newID string) {
	if state := v.logs[oldID]; state != nil {
		v.logs[newID] = state
		delete(v.logs, oldID)
	}
}
func (w *workbench) logState(threadID string) *ui.ListState { return w.widgets.log(threadID) }

// The "Stardust" palette (docs/desktop-ui.md): a warm paper sidebar beside a
// white canvas (graphite in dark), and one ember accent for running state,
// the primary action, links and focus.
//
// Surfaces step by elevation: sidebar (deepest) < canvas (reading) < soft
// (hover fills, quiet wells) < raised (composer, popovers, cards). Code
// wells sit below the canvas. Text has three tiers, each readable on every
// surface: ink (content), muted (secondary, >= 6:1) and faint (meta,
// >= 4.2:1 on canvas). Lines come in two weights: line for controls and
// lineSoft for structure. Semantic hues are reserved for state: running
// (sky), warning (amber), success (emerald) and danger (red).
type palette struct {
	canvas, surface, sidebar, soft ui.Color
	raised, code                   ui.Color
	card                           ui.Color // a raised row card on the sidebar: the selected thread
	ink, muted, faint, line        ui.Color
	lineSoft                       ui.Color
	accent, selected, accentInk    ui.Color
	rowHover                       ui.Color
	user, success, warning, danger ui.Color
	running                        ui.Color
	dark                           bool
	preset                         string
}

type themePresetSpec struct {
	id         string
	zh, en, ja string
}

var themePresets = []themePresetSpec{
	{id: "graphite", zh: "石墨", en: "Graphite", ja: "グラファイト"},
	{id: "ocean", zh: "深海", en: "Ocean", ja: "オーシャン"},
	{id: "ember", zh: "余烬", en: "Ember", ja: "エンバー"},
	{id: "iris", zh: "暮紫", en: "Iris", ja: "アイリス"},
}

func normalizeThemePreset(id string) string {
	switch strings.ToLower(strings.TrimSpace(id)) {
	case "ocean", "ember", "iris":
		return strings.ToLower(strings.TrimSpace(id))
	default:
		return "graphite"
	}
}

func workbenchTheme(systemDark, forceDark, dark bool) (*ui.Theme, palette) {
	return workbenchThemeFor("graphite", systemDark, forceDark, dark)
}

func paletteFor(preset string, dark bool) palette {
	_, p := workbenchThemeFor(preset, dark, true, dark)
	return p
}

func workbenchThemeFor(preset string, systemDark, forceDark, dark bool) (*ui.Theme, palette) {
	if !forceDark {
		dark = systemDark
	}
	preset = normalizeThemePreset(preset)
	t := ui.LightTheme()
	if dark {
		t = ui.DarkTheme()
	}
	p := palette{dark: dark, preset: preset}
	var accentHover, accentPressed ui.Color
	switch preset {
	case "ocean":
		if dark {
			p.canvas, p.surface = ui.Hex("#17212B"), ui.Hex("#17212B")
			p.sidebar, p.soft = ui.Hex("#121A22"), ui.Hex("#1E2C3A")
			p.raised, p.code = ui.Hex("#233544"), ui.Hex("#101820")
			p.card = ui.Hex("#233544")
			p.ink, p.muted, p.faint = ui.Hex("#ECF3F8"), ui.Hex("#9BB2C4"), ui.Hex("#7690A5")
			p.line, p.lineSoft = ui.Hex("#2D4153"), ui.Hex("#223241")
			p.accent, p.accentInk, p.selected = ui.Hex("#5BD0D6"), ui.Hex("#7CE0E5"), ui.Hex("#263B4D")
			p.rowHover = ui.Hex("#1F3140")
			p.user, p.success, p.warning, p.danger = ui.Hex("#233544"), ui.Hex("#34D399"), ui.Hex("#F5A524"), ui.Hex("#FB7185")
			p.running = ui.Hex("#5BD0D6")
			accentHover, accentPressed = ui.Hex("#72DBE0"), ui.Hex("#42BFC6")
		} else {
			p.canvas, p.surface = ui.Hex("#F5F7F8"), ui.Hex("#F5F7F8")
			p.sidebar, p.soft = ui.Hex("#E8EFF4"), ui.Hex("#EBF1F6")
			p.raised, p.code = ui.Hex("#FFFFFF"), ui.Hex("#E4ECF2")
			p.card = ui.Hex("#FFFFFF")
			p.ink, p.muted, p.faint = ui.Hex("#13222F"), ui.Hex("#465B6B"), ui.Hex("#6A8091")
			p.line, p.lineSoft = ui.Hex("#CFDBE4"), ui.Hex("#DEE8F0")
			p.accent, p.accentInk, p.selected = ui.Hex("#0A6F75"), ui.Hex("#08595E"), ui.Hex("#DCE7F0")
			p.rowHover = ui.Hex("#E1ECF4")
			p.user, p.success, p.warning, p.danger = ui.Hex("#E8EFF4"), ui.Hex("#15803D"), ui.Hex("#B45309"), ui.Hex("#C9252C")
			p.running = ui.Hex("#0A6F75")
			accentHover, accentPressed = ui.Hex("#085E63"), ui.Hex("#064C50")
		}
	case "ember":
		if dark {
			p.canvas, p.surface = ui.Hex("#291E1A"), ui.Hex("#291E1A")
			p.sidebar, p.soft = ui.Hex("#221815"), ui.Hex("#352620")
			p.raised, p.code = ui.Hex("#432E23"), ui.Hex("#1D1411")
			p.card = ui.Hex("#432E23")
			p.ink, p.muted, p.faint = ui.Hex("#FAF0EB"), ui.Hex("#C8AEA2"), ui.Hex("#9E8275")
			p.line, p.lineSoft = ui.Hex("#543B2E"), ui.Hex("#3E2B22")
			p.accent, p.accentInk, p.selected = ui.Hex("#F78A7A"), ui.Hex("#FFA496"), ui.Hex("#4E3629")
			p.rowHover = ui.Hex("#3D2A21")
			p.user, p.success, p.warning, p.danger = ui.Hex("#432E23"), ui.Hex("#4ADE80"), ui.Hex("#FBBF24"), ui.Hex("#FB7185")
			p.running = ui.Hex("#F78A7A")
			accentHover, accentPressed = ui.Hex("#FA9D8F"), ui.Hex("#E87260")
		} else {
			p.canvas, p.surface = ui.Hex("#FAF5F2"), ui.Hex("#FAF5F2")
			p.sidebar, p.soft = ui.Hex("#F2E7E1"), ui.Hex("#EFE3DC")
			p.raised, p.code = ui.Hex("#FFFDFC"), ui.Hex("#F0E5DF")
			p.card = ui.Hex("#FFFDFC")
			p.ink, p.muted, p.faint = ui.Hex("#261914"), ui.Hex("#6B5146"), ui.Hex("#8C6F63")
			p.line, p.lineSoft = ui.Hex("#E2CFC5"), ui.Hex("#ECE0D8")
			p.accent, p.accentInk, p.selected = ui.Hex("#C44D3A"), ui.Hex("#9E3828"), ui.Hex("#EADBD3")
			p.rowHover = ui.Hex("#EFE1DA")
			p.user, p.success, p.warning, p.danger = ui.Hex("#F2E7E1"), ui.Hex("#1F9D4C"), ui.Hex("#B25E00"), ui.Hex("#C9252C")
			p.running = ui.Hex("#C44D3A")
			accentHover, accentPressed = ui.Hex("#B0412F"), ui.Hex("#963425")
		}
	case "iris":
		if dark {
			p.canvas, p.surface = ui.Hex("#1B1726"), ui.Hex("#1B1726")
			p.sidebar, p.soft = ui.Hex("#15121F"), ui.Hex("#241E34")
			p.raised, p.code = ui.Hex("#2B243D"), ui.Hex("#120F1B")
			p.card = ui.Hex("#2B243D")
			p.ink, p.muted, p.faint = ui.Hex("#F2EEF9"), ui.Hex("#B5AAD0"), ui.Hex("#8D80AD")
			p.line, p.lineSoft = ui.Hex("#3B3154"), ui.Hex("#2D2542")
			p.accent, p.accentInk, p.selected = ui.Hex("#B48CF2"), ui.Hex("#CBB0F8"), ui.Hex("#362D4D")
			p.rowHover = ui.Hex("#2A233B")
			p.user, p.success, p.warning, p.danger = ui.Hex("#2B243D"), ui.Hex("#34D399"), ui.Hex("#F5A524"), ui.Hex("#FB7185")
			p.running = ui.Hex("#B48CF2")
			accentHover, accentPressed = ui.Hex("#C29FF6"), ui.Hex("#9E70EA")
		} else {
			p.canvas, p.surface = ui.Hex("#F7F5FA"), ui.Hex("#F7F5FA")
			p.sidebar, p.soft = ui.Hex("#EDE8F5"), ui.Hex("#EAE4F3")
			p.raised, p.code = ui.Hex("#FFFFFF"), ui.Hex("#EAE5F2")
			p.card = ui.Hex("#FFFFFF")
			p.ink, p.muted, p.faint = ui.Hex("#1F182B"), ui.Hex("#5A4F6E"), ui.Hex("#7B6F91")
			p.line, p.lineSoft = ui.Hex("#DCD4EB"), ui.Hex("#E7E1F2")
			p.accent, p.accentInk, p.selected = ui.Hex("#6F42C1"), ui.Hex("#5932A3"), ui.Hex("#E3DBF0")
			p.rowHover = ui.Hex("#EAE3F5")
			p.user, p.success, p.warning, p.danger = ui.Hex("#EDE8F5"), ui.Hex("#1F9D4C"), ui.Hex("#B25E00"), ui.Hex("#C9252C")
			p.running = ui.Hex("#6F42C1")
			accentHover, accentPressed = ui.Hex("#5F35AB"), ui.Hex("#4E2A91")
		}
	default: // graphite
		if dark {
			p.canvas, p.surface = ui.Hex("#0A0A0A"), ui.Hex("#0A0A0A")
			p.sidebar, p.soft = ui.Hex("#111214"), ui.Hex("#181A1F")
			p.raised, p.code = ui.Hex("#17181C"), ui.Hex("#060607")
			p.card = ui.Hex("#202229")
			p.ink, p.muted, p.faint = ui.Hex("#F5F6F8"), ui.Hex("#A3A8B4"), ui.Hex("#7D8392")
			p.line, p.lineSoft = ui.Hex("#272A33"), ui.Hex("#1E2028")
			p.accent, p.accentInk, p.selected = ui.Hex("#346BF1"), ui.Hex("#6B97FF"), ui.Hex("#222633")
			p.rowHover = ui.Hex("#1B1E27")
			p.user, p.success, p.warning, p.danger = ui.Hex("#1E212B"), ui.Hex("#30D158"), ui.Hex("#F5A524"), ui.Hex("#FF5A5F")
			p.running = ui.Hex("#346BF1")
			accentHover, accentPressed = ui.Hex("#4D7EF4"), ui.Hex("#2558D9")
		} else {
			p.canvas, p.surface = ui.Hex("#FCFCFC"), ui.Hex("#FCFCFC")
			p.sidebar, p.soft = ui.Hex("#F3F4F6"), ui.Hex("#F0F2F5")
			p.raised, p.code = ui.Hex("#FFFFFF"), ui.Hex("#F4F5F7")
			p.card = ui.Hex("#FFFFFF")
			p.ink, p.muted, p.faint = ui.Hex("#111318"), ui.Hex("#525866"), ui.Hex("#737A8A")
			p.line, p.lineSoft = ui.Hex("#DFE3EA"), ui.Hex("#EAEDF2")
			p.accent, p.accentInk, p.selected = ui.Hex("#1B4ED8"), ui.Hex("#1E40AF"), ui.Hex("#E7EBF2")
			p.rowHover = ui.Hex("#ECEFF5")
			p.user, p.success, p.warning, p.danger = ui.Hex("#F0F2F5"), ui.Hex("#1F9D4C"), ui.Hex("#B25E00"), ui.Hex("#C9252C")
			p.running = ui.Hex("#1B4ED8")
			accentHover, accentPressed = ui.Hex("#1642BA"), ui.Hex("#123699")
		}
	}
	t.Background, t.Surface, t.SurfaceHover, t.SurfacePressed = p.canvas, p.soft, p.selected, p.selected
	t.Border, t.Text, t.TextMuted = p.line, p.ink, p.muted
	t.Accent, t.AccentHover, t.AccentPressed = p.accent, accentHover, accentPressed
	t.AccentText = ui.Hex("#FFFFFF")
	if dark && (preset == "ocean" || preset == "ember" || preset == "iris") {
		t.AccentText = ui.Hex("#0E1116")
	}
	t.InactiveAccent, t.Selection, t.Focus = p.accent, accentSelectionColor(p.accent, dark), p.accent
	t.Danger, t.Warning, t.Success = p.danger, p.warning, p.success
	t.Radius, t.Spacing, t.FontSize = 8, 4, 13
	return t, p
}

// accentSelectionColor is the text selection highlight: a tinted accent wash.
func accentSelectionColor(accent ui.Color, dark bool) ui.Color {
	if dark {
		return accent.Alpha(0.36)
	}
	return accent.Alpha(0.22)
}

func accentSelection(dark bool) ui.Color {
	if dark {
		return ui.RGBA(255, 106, 43, 0.38)
	}
	return ui.RGBA(255, 90, 31, 0.22)
}

func (w *workbench) view(c *ui.Context) {
	w.visuals.askHotkey = askHotkey{}
	if !w.permissionPageActive() {
		w.cancelRemoteHostingIntent()
	}
	if !w.remoteDesktopFocused(c) && (len(w.rd.heldKeys) > 0 || len(w.rd.heldButtons) > 0) {
		w.releaseRemoteKeys()
	}
	dark := c.Theme().Dark
	if w.widgets.themeSet {
		dark = w.widgets.dark
	}
	preset := normalizeThemePreset(w.widgets.themePreset)
	if w.widgets.resolvedTheme == nil || w.widgets.resolvedPalette.dark != dark || w.widgets.resolvedPalette.preset != preset {
		w.widgets.resolvedTheme, w.widgets.resolvedPalette = workbenchThemeFor(preset, dark, true, dark)
	}
	p := w.widgets.resolvedPalette
	w.widgets.dark = p.dark
	c.SetTheme(w.widgets.resolvedTheme)
	w.widgets.services = c.Services()
	w.indexFrame()
	defer func() { w.frame.valid = false }()
	w.widgets.frameNow = c.Now()
	w.widgets.frameSeq++
	if w.widgets.escArmedMode != "" && !w.widgets.escArmedAt.IsZero() {
		window := 600 * time.Millisecond
		if w.widgets.escArmedMode == "stop" {
			window = 1200 * time.Millisecond
		}
		if w.widgets.frameNow.Sub(w.widgets.escArmedAt) > window {
			if w.widgets.escArmedMode == "stop" && w.actionNotice.Text == w.escStopHint() {
				w.actionNotice = notice{}
			}
			w.widgets.escArmedMode = ""
			w.widgets.escArmedAt = time.Time{}
		}
	}
	overlayOpenBefore := w.hasOpenOverlay(c)
	if overlayOpenBefore && w.widgets.escArmedMode != "" {
		if w.widgets.escArmedMode == "stop" && w.actionNotice.Text == w.escStopHint() {
			w.actionNotice = notice{}
		}
		w.widgets.escArmedMode = ""
		w.widgets.escArmedAt = time.Time{}
	}
	w.markdownOptions(p)
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Cmd, ui.KeyF) {
		w.timelineNav.open = true
		w.timelineNav.focus.Focus()
	}
	w.timelineNav.focus.OnShortcut(c, 0, ui.KeyEscape, func() { w.timelineNav.open = false; w.widgets.composerFocus.Focus() })
	if !w.remoteDesktopFocused(c) && c.Shortcut(0, ui.KeyF6) {
		if w.widgets.sidebarFocus.Focused(c) {
			w.widgets.composerFocus.Focus()
		} else {
			w.widgets.sidebarClosed = false
			w.widgets.sidebarFocus.Focus()
		}
	}
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Cmd, ui.KeyN) {
		w.newSidebarSession()
	}
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Cmd, ui.KeySlash) {
		w.widgets.shortcutsOpen = !w.widgets.shortcutsOpen
	}
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Cmd, ui.KeyComma) {
		w.widgets.settingsOpen = !w.widgets.settingsOpen
	}
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Cmd, ui.KeyK) {
		w.widgets.sidebarClosed, w.widgets.mobileNav = false, true
		w.widgets.searchFocus.Focus()
	}
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Ctrl, ui.KeyO) {
		w.toggleAllTools()
	}
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Ctrl, ui.KeyT) {
		w.toggleAllReasoning()
	}
	if !w.remoteDesktopFocused(c) && c.Shortcut(ui.Ctrl|ui.Shift, ui.KeyB) {
		if ok, _ := w.toolAvailable("git"); ok {
			width, _ := c.Size()
			narrow := width < narrowWidth
			if w.headerShowsBranchPill(narrow) {
				w.toggleBranchPopover("header")
			} else if w.features.panel == "git" {
				w.toggleBranchPopover("dock")
			} else {
				w.toggleBranchPopover("footer")
			}
		}
	}
	w.widgets.sidebarFocus.OnShortcut(c, 0, ui.KeyEnter, func() {
		w.widgets.mobileNav = false
		w.widgets.composerFocus.Focus()
	})
	w.widgets.searchFocus.OnShortcut(c, 0, ui.KeyDown, func() { w.widgets.sidebarFocus.Focus() })
	width, _ := c.Size()
	narrow := width < narrowWidth
	ui.Row(c).Fill().AlignItems(ui.Stretch).Background(p.canvas).Children(func() {
		if narrow && w.widgets.mobileNav {
			w.sidebarView(c, p, 0)
			return
		}
		if !narrow && !w.widgets.sidebarClosed {
			sidebarWidth := float32(sidebarWide)
			if width < 1180 {
				sidebarWidth = sidebarNarrow
			}
			w.sidebarView(c, p, sidebarWidth)
		}
		w.mainView(c, p, narrow)
	})
	w.gitDialog(c, p)
	w.diffDialog(c, p)
	w.turnDiffDialog(c, p)
	w.visualDialog(c, p)
	w.imageLightbox(c, p)
	w.shortcutsDialog(c, p)
	w.settingsDialog(c, p)
	w.piSetupDialog(c, p)
	w.usageDialog(c, p)
	w.rewindDialog(c, p)
	w.forkDialog(c, p)
	w.mcpAddDialog(c, p)
	w.remoteDirectoryDialog(c, p)
	w.environmentConnectionsDialog(c, p)
	w.bindAskHotkeys(c)
	if !w.remoteDesktopFocused(c) && !overlayOpenBefore && c.Shortcut(0, ui.KeyEscape) {
		w.handleBareEscape()
	}
}

func (w *workbench) navigationSessions() []string {
	ids := make([]string, 0, len(w.localOrder)+len(w.threads)+1)
	seen := map[string]bool{}
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for i := len(w.localOrder) - 1; i >= 0; i-- {
		add(w.localOrder[i])
	}
	pending := []string{}
	for id, c := range w.composers {
		if !isLocalSession(id) && c.pending != nil {
			pending = append(pending, id)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		a, b := w.composers[pending[i]].pending, w.composers[pending[j]].pending
		if a.startedAt.Equal(b.startedAt) {
			return pending[i] < pending[j]
		}
		return a.startedAt.After(b.startedAt)
	})
	for _, id := range pending {
		add(id)
	}
	add(w.awaitingThread)
	committed := make([]string, 0, len(w.threads))
	for i := len(w.threads) - 1; i >= 0; i-- {
		committed = append(committed, w.threads[i].ID)
	}
	sort.SliceStable(committed, func(i, j int) bool {
		return w.sessionActivity(committed[i]).After(w.sessionActivity(committed[j]))
	})
	for _, id := range committed {
		add(id)
	}
	return ids
}

func (w *workbench) mainView(c *ui.Context, p palette, narrow bool) {
	ui.Column(c).Grow(1).MinWidth(0).Background(p.surface).Children(func() {
		w.headerView(c, p, narrow)
		w.connectionView(c, p)
		if w.stage != "" {
			w.stageView(c, p, narrow)
			return
		}
		hasConversation := w.hasConversation(w.selected)
		mode := w.dockMode(c, narrow)
		main := w.mainStageWidth(c, narrow)
		// The subagent panel shares the dock's column. A narrow stage floats it
		// instead of replacing the conversation; the dock comes back when it closes.
		subagentOpen := w.subagentPanelOpen()
		subagentFloat := subagentOpen && w.subagentPanelFloats(c, narrow)
		if subagentOpen && !subagentFloat && mode == dockMaximized {
			mode = dockSide
		}
		if subagentFloat {
			mode = dockNone
		}
		ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			if mode == dockMaximized {
				w.dockView(c, p, mode, main)
				return
			}
			ui.Column(c.Key("chat-column")).Grow(1).MinWidth(0).Children(func() {
				switch {
				case mode == dockSheet:
					// Narrow stages: the tool takes the timeline's full height as
					// a sheet; the composer stays reachable below it.
					w.dockView(c, p, mode, 0)
					w.composerView(c, p)
				case hasConversation:
					w.logView(c, p, narrow)
					w.composerView(c, p)
				default:
					w.homeView(c, p)
				}
			})
			if subagentOpen && !subagentFloat {
				w.subagentResizeHandle(c, p, main)
				w.subagentSideView(c, p, w.subagentPanelWidth(main))
				return
			}
			if mode == dockSide {
				w.dockResizeHandle(c, p, main)
				w.dockView(c, p, mode, w.dockWidth(main))
				return
			}
			if w.widgets.workspaceOpen && w.canShowInlineWorkspace(c, narrow) {
				w.workspaceView(c, p)
			}
		})
		if subagentFloat {
			w.subagentFloatingView(c, p)
		}
	})
}

func (w *workbench) canShowInlineWorkspace(c *ui.Context, narrow bool) bool {
	width, _ := c.Size()
	return width >= 1000 && w.stage == "" && w.dockMode(c, narrow) == dockNone
}

func (w *workbench) headerView(c *ui.Context, p palette, narrow bool) {
	bar := c.TitleBar()
	left := float32(20)
	if narrow || w.widgets.sidebarClosed {
		left = max(left, bar.Left+10)
	}
	ui.Row(c).Height(max(float32(46), bar.Height)).Padding(0, max(float32(12), bar.Right+10), 0, left).Gap(6).AlignItems(ui.Center).
		BorderWidth(0, 0, 1, 0).BorderColor(p.lineSoft).DragWindow().Children(func() {
		if narrow || w.widgets.sidebarClosed {
			if iconButton(c, p, "sidebar-open", w.tr("会话导航", "Threads"), panelIcon).Clicked() {
				w.widgets.sidebarClosed = false
				w.widgets.mobileNav = narrow
			}
		}
		title := w.tr("新会话", "New thread")
		if w.hasConversation(w.selected) {
			title = w.displayTitleFor(w.selected)
		}
		if thread, ok := w.selectedThread(); ok {
			w.originChip(c, p, thread)
		}
		w.breadcrumb(c, p, title, narrow)
		w.subagentRelationshipsControl(c, p)
		if !w.timelineNav.open && w.hasConversation(w.selected) {
			w.timelineToolbar(c, p, w.conversationFor(w.selected))
		}
		w.headerTools(c, p)
	})
}

// headerTool is one entry of the title bar's compact tool switcher. Each opens
// its tool in the workbench dock beside the conversation; Escape or a second
// click closes it.
type headerTool struct {
	id, zh, en string
	icon       *ui.SVG
}

var headerTools = []headerTool{
	{"files", "文件", "Files", fileIcon},
	{"git", "Git", "Git", branchIcon},
	{"terminal", "终端", "Terminal", terminalIcon},
	{"plugins", "插件", "Plugins", toolIcon},
	{"mcp", "MCP", "MCP", serverIcon},
}

func (w *workbench) toolAvailable(id string) (bool, string) {
	if w.proxyEnvironmentID() != "" && (id == "files" || id == "git" || id == "mcp") {
		return false, w.tr("远端任务代理：请切换到目标机器操作文件、Git 或 MCP", "Remote task proxy: open the target environment for files, Git or MCP")
	}
	switch id {
	case "git":
		// A chosen project folder is enough to read its Git state: no prompt
		// has to be sent first.
		if !w.hasThread(w.selected) && w.selectedWorkspacePath() == "" {
			return false, w.tr("选择项目目录或首次发送后可打开 Git", "Choose a project folder or send a first message to open Git")
		}
	case "session":
		if !w.hasThread(w.selected) {
			return false, w.tr("首次发送并创建会话后可打开项目工具", "Available after the first message creates the thread")
		}
	case "files":
		if _, _, ok := w.fileScope(w.selected); !ok {
			return false, w.tr("选择项目目录后可浏览文件", "Choose a project folder to browse files")
		}
	}
	return true, ""
}

func (w *workbench) togglePanel(id string) {
	if w.features.panel == id && w.stage == "" {
		w.closeDock()
		return
	}
	w.features.panel = ""
	w.openDock(id)
}

// remoteDesktopEntry returns the enabled Remote Desktop plugin page, if any.
func (w *workbench) remoteDesktopEntry() (api.PluginInfo, api.PluginNavItem, bool) {
	for _, plugin := range w.features.plugins {
		if plugin.Error != "" || plugin.Disabled {
			continue
		}
		for _, page := range plugin.Navigation {
			if page.NativePanel == "remote-desktop" {
				return plugin, page, true
			}
		}
	}
	return api.PluginInfo{}, api.PluginNavItem{}, false
}

// headerTools is the right side of the title bar: the tool switcher, the
// thread menu, the shortcut sheet and the workspace inspector. Wide windows
// show each tool as an icon; narrow ones fold them into one menu.
func (w *workbench) headerTools(c *ui.Context, p palette) {
	width, height := c.Size()
	rdPlugin, rdPage, hasRD := w.remoteDesktopEntry()
	rdStage := ""
	if hasRD {
		rdStage = pluginStage(rdPlugin.ID, rdPage.Page)
	}
	row := ui.Row(c.Key("header-tools")).Gap(2).AlignItems(ui.Center).Shrink(0).Role(ui.RoleToolbar).Label(w.tr("顶栏工具", "Title bar tools"))
	if w.features.panel != "" && row.Shortcut(0, ui.KeyEscape) {
		w.closeDock()
	}
	dockTabsOpen := w.features.panel != "" && w.features.panel != "session" && w.stage == ""
	renderRD := func() {
		if !hasRD {
			return
		}
		rdActive := w.stage == rdStage
		rdLabel := nonEmptyString(rdPage.Title, w.tr("远程桌面", "Remote Desktop"))
		btn := ui.Button(c.Key("header-rd"), "").Label(rdLabel).Tooltip(rdLabel).Size(26, 26).Shrink(0).Padding(5).
			Border(0, p.line).Background(ui.Transparent).TextColor(p.muted).Radius(7).Role(ui.RoleTab).Selected(rdActive)
		if rdActive {
			btn.Background(p.selected).TextColor(p.ink)
		} else if btn.Hovered() {
			btn.Background(p.soft).TextColor(p.ink)
		}
		btn.Children(func() {
			ui.Icon(c, remoteIcon).FontSize(16)
			if w.rd.connected {
				ui.Box(c).Absolute().Right(3).Top(3).Size(5, 5).Radius(3).Background(p.success)
			}
		})
		if btn.Clicked() {
			if rdActive {
				w.stage = ""
			} else {
				w.features.panel = ""
				w.openPluginPage(rdPlugin, rdPage)
			}
		}
	}
	row.Children(func() {
		if width >= 1220 {
			w.headerUsageCapsule(c, p)
		}
		if width >= 760 {
			if dockTabsOpen {
				// The open dock header already displays the tool tab strip right below;
				// avoid stacking two duplicate tool bars on top of each other.
				renderRD()
			} else {
				ui.Row(c.Key("tool-switcher")).Gap(1).Padding(2).Radius(10).AlignItems(ui.Center).Background(p.soft).Border(1, p.lineSoft).Children(func() {
					for _, tool := range headerTools {
						label := w.tr(tool.zh, tool.en)
						selected := w.features.panel == tool.id && w.stage == ""
						button := iconButton(c, p, "panel:"+tool.id, label, tool.icon).Size(26, 26).Padding(5).Radius(7).Role(ui.RoleTab).Selected(selected)
						if ok, why := w.toolAvailable(tool.id); !ok {
							button.Disabled(true).Description(why)
						}
						if selected {
							button.Background(p.raised).TextColor(p.ink)
						}
						if button.Clicked() {
							w.togglePanel(tool.id)
						}
					}
					renderRD()
				})
			}
		} else {
			tools := iconButton(c, p, "tools-menu", w.tr("工具", "Tools"), gridIcon).Size(28, 28).Padding(6)
			if w.features.panel != "" && w.features.panel != "session" || hasRD && w.stage == rdStage {
				tools.Background(p.selected).TextColor(p.ink)
			}
			tools.PopoverMenuButton(func(m *ui.Menu) {
				for _, tool := range headerTools {
					ok, _ := w.toolAvailable(tool.id)
					if m.Item(w.tr(tool.zh, tool.en)).Radio(w.features.panel == tool.id && w.stage == "").Disabled(!ok).Chosen() {
						w.togglePanel(tool.id)
					}
				}
				if hasRD {
					rdLabel := nonEmptyString(rdPage.Title, w.tr("远程桌面", "Remote Desktop"))
					if m.Item(rdLabel).Radio(w.stage == rdStage).Chosen() {
						if w.stage == rdStage {
							w.stage = ""
						} else {
							w.features.panel = ""
							w.openPluginPage(rdPlugin, rdPage)
						}
					}
				}
				m.Separator()
				if m.Item(w.tr("快捷键速查", "Keyboard shortcuts")).Chosen() {
					w.widgets.shortcutsOpen = true
				}
			})
		}
		ui.Box(c).Width(6).Shrink(0)
		w.threadMenuButton(c, p)
		workspace := iconButton(c, p, "workspace", w.tr("工作区", "Workspace"), sidebarRightIcon).Size(28, 28).Padding(6)
		workspace.Description(w.tr("查看项目、分支和文件变更", "Project, branch and file changes"))
		if w.widgets.workspaceOpen {
			workspace.Background(p.selected).TextColor(p.ink)
		}
		if workspace.Clicked() {
			w.widgets.workspaceOpen = !w.widgets.workspaceOpen
			if w.widgets.workspaceOpen {
				w.refreshWorkspace()
				w.refreshGit(w.selected, false)
			}
		}
		if !w.canShowInlineWorkspace(c, width < narrowWidth) {
			ui.PopoverBase(c, workspace, &w.widgets.workspaceOpen, func(panel ui.Element) {
				panel.AttachTo(workspace, ui.AnchorBottomEnd, ui.AnchorTopEnd).Margin(6, 0, 0).
					Width(min(float32(316), width-24)).MaxHeight(min(float32(540), height-70)).Padding(10).Radius(14).Background(p.raised).Border(1, p.line)
				if p.dark {
					panel.Shadow(0, 12, 32, 0, ui.Hex("#00000066"))
				} else {
					panel.Shadow(0, 8, 24, 0, ui.Hex("#0F172A18"))
				}
				w.workspaceContents(c, p)
			})
		}
	})
}

// freshTokens separates uncached input+output+cache-write tokens from cached
// prompt reads so UI counters do not make 99% cache-hit turns look like full
// re-sends.
func freshTokens(input, output, cacheWrite, cacheRead, total int64) int64 {
	if cacheRead > 0 {
		if fresh := input + output + cacheWrite; fresh > 0 {
			return fresh
		}
	}
	return total
}

// headerUsageCapsule renders a subtle usage/context capsule in the top bar
// ONLY when real telemetry or usage data is available; gracefully hidden when unknown.
func (w *workbench) headerUsageCapsule(c *ui.Context, p palette) {
	_, _, tel, ctxWindow := w.threadTelemetry(w.selected)
	summary := w.widgets.usage.summary
	hasCtx := tel != nil && tel.LastContextTokens > 0 && ctxWindow > 0
	totalTokens := int64(0)
	cacheReadTokens := int64(0)
	displayTokens := int64(0)
	if tel != nil && tel.TotalTokens > 0 {
		totalTokens = tel.TotalTokens
		cacheReadTokens = tel.CacheReadTokens
		displayTokens = freshTokens(tel.InputTokens, tel.OutputTokens, tel.CacheWriteTokens, tel.CacheReadTokens, tel.TotalTokens)
	} else if summary != nil && summary.TotalTokens > 0 {
		totalTokens = summary.TotalTokens
		cacheReadTokens = summary.CacheReadTokens
		displayTokens = freshTokens(summary.InputTokens, summary.OutputTokens, summary.CacheWriteTokens, summary.CacheReadTokens, summary.TotalTokens)
	}
	if !hasCtx && totalTokens <= 0 {
		return
	}
	tip := w.tr3("打开用量统计", "Open usage details", "使用量の詳細を開く")
	if totalTokens > 0 && cacheReadTokens > 0 {
		tip = fmt.Sprintf(w.tr3("新增 %s · 缓存读 %s · 总计 %s（点击查看明细）", "%s new · %s cached · %s total (click for details)", "新規 %s・キャッシュ %s・合計 %s"),
			compactCount64(displayTokens), compactCount64(cacheReadTokens), compactCount64(totalTokens))
	}
	capsule := ui.Button(c.Key("header-usage-capsule"), "").Label(w.tr3("用量概览", "Usage overview", "使用量の概要")).
		Tooltip(tip).
		Height(28).Padding(0, 10).Radius(14).Shrink(0).
		Background(p.soft).Border(1, p.lineSoft).TextColor(p.muted)
	if capsule.Hovered() {
		capsule.Background(p.selected).BorderColor(p.line).TextColor(p.ink)
	}
	capsule.Children(func() {
		ui.Row(c).Gap(7).AlignItems(ui.Center).Children(func() {
			if hasCtx {
				pct := int(min(int64(999), (tel.LastContextTokens*100)/ctxWindow))
				frac := float32(tel.LastContextTokens) / float32(ctxWindow)
				tone := p.accent
				if pct >= 85 {
					tone = p.danger
				} else if pct >= 70 {
					tone = p.warning
				}
				progressRing(c, 12, frac, p.line, tone)
				ui.Text(c, fmt.Sprintf(w.tr3("上下文 %d%%", "Context %d%%", "コンテキスト %d%%"), pct)).
					FontSize(11).FontWeight(500).FontFeatures("tnum").TextColor(p.ink).NoWrap()
			}
			if hasCtx && totalTokens > 0 {
				ui.Box(c).Size(1, 11).Background(p.line).Shrink(0)
			}
			if totalTokens > 0 {
				label := fmt.Sprintf(w.tr3("%s 令牌", "%s tok", "%s tok"), compactCount64(displayTokens))
				if cacheReadTokens > 0 {
					label = fmt.Sprintf(w.tr3("新增 %s", "%s new", "新規 %s"), compactCount64(displayTokens))
				}
				ui.Text(c, label).
					FontSize(11).FontFeatures("tnum").TextColor(p.muted).NoWrap()
			}
		})
	})
	if capsule.Clicked() {
		w.openUsage()
	}
	ui.Box(c).Width(4).Shrink(0)
}

// threadMenuButton holds the selected thread's actions, as its sidebar row's
// context menu, plus the session drawer for rename and delivery settings.
func (w *workbench) threadMenuButton(c *ui.Context, p palette) {
	thread, ok := w.selectedThread()
	more := iconButton(c, p, "panel:session", w.tr("会话", "Thread"), moreIcon).Size(28, 28).Padding(6)
	more.Description(w.tr("会话选项：重命名、置顶、归档、分叉或删除", "Thread options: rename, pin, settle, fork or delete"))
	if !ok {
		more.Disabled(true)
		return
	}
	if w.features.panel == "session" {
		more.Background(p.selected).TextColor(p.ink)
	}
	if more.Clicked() {
		w.togglePanel("session")
	}
	_ = thread
}

// originChip links a child, forked, or peer thread back to the thread that made it.
func (w *workbench) originChip(c *ui.Context, p palette, thread api.Thread) {
	origin, label, icon := thread.ParentThreadID, w.tr("返回父任务", "Parent"), backIcon
	if origin == "" {
		origin, label, icon = thread.ForkedFromThreadID, w.tr("分叉自", "Forked from"), forkIcon
	}
	if origin == "" {
		origin, label, icon = thread.CreatedByThreadID, w.tr("来源会话", "Origin thread"), backIcon
	}
	if origin == "" || origin == thread.ID {
		return
	}
	originTitle := w.displayTitleFor(origin)
	chip := ui.Button(c.Key("parent"), "").Label(label).Height(24).Padding(0, 8).Radius(12).Shrink(0).
		Border(1, p.lineSoft).Background(p.soft).TextColor(p.muted)
	chip.Tooltip(label + ": " + originTitle).Description(originTitle)
	if chip.Hovered() {
		chip.Background(p.selected).TextColor(p.ink).BorderColor(p.line)
	}
	chip.Children(func() {
		ui.Row(c).Gap(4).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, icon).FontSize(11)
			ui.Text(c, originTitle).FontSize(11.5).MaxWidth(140).SingleLine().Ellipsis("…")
		})
	})
	if chip.Clicked() {
		w.selectThread(origin)
	}
}

// homeView is the workbench's new-thread hero: one quiet question, a link to start
// without a project, and the composer; wide windows add the floating
// project card at the top right.
func (w *workbench) homeView(c *ui.Context, p palette) {
	width, _ := c.Size()
	spec := w.selectedWorkspace()
	inlineWorkspace := w.widgets.workspaceOpen && w.canShowInlineWorkspace(c, width < narrowWidth)
	ui.Column(c).Grow(1).Children(func() {
		if spec != nil && !inlineWorkspace && width >= 760 && w.features.panel == "" {
			ui.Row(c).FillWidth().Justify(ui.End).Shrink(0).Children(func() { w.projectCard(c, p) })
		}
		ui.Column(c).Grow(1).Padding(20, 24, 54).Justify(ui.Center).AlignItems(ui.Center).Children(func() {
			ui.Column(c).FillWidth().MaxWidth(readingWidth).Gap(22).Children(func() {
				ui.Column(c).Gap(8).AlignItems(ui.Center).Children(func() {
					question := w.tr3("今天想构建什么？", "What should we build today?", "今日は何を作りますか？")
					if spec != nil {
						name := w.workspaceLabel()
						question = w.tr3("想在 "+name+" 构建什么？", "What should we build in "+name+"?", name+" で何を作りますか？")
					}
					ui.Text(c, question).FontSize(24).FontWeight(600).LetterSpacing(-0.2).TextColor(p.ink).TextAlign(ui.Center).Wrap().Role(ui.RoleHeading).Level(2)
					if spec != nil {
						without := ui.Button(c.Key("start-without-project"), "").Label(w.tr("或不关联项目直接开始", "or start without a project")).
							Height(22).Padding(0, 6).Radius(6).Border(0, p.line).Background(ui.Transparent).TextColor(p.muted)
						if without.Hovered() {
							without.TextColor(p.ink)
						}
						without.Children(func() {
							ui.Text(c, w.tr("或不关联项目直接开始", "or start without a project")).FontSize(13).NoWrap()
						})
						if without.Clicked() {
							w.configureWorkspaceSpec(nil)
							w.widgets.composerFocus.Focus()
						}
					} else {
						ui.Text(c, w.tr("一个问题、一段代码，或一个待完成的想法。", "A question, a piece of code, or an idea to finish.")).FontSize(13).TextColor(p.muted)
					}
				})
				w.homeOnboardingCard(c, p)
				ui.Column(c).FillWidth().Children(func() { w.composerBox(c, p, true) })
			})
		})
	})
}

// projectCard is the workbench's floating top-right panel on a new thread,
// aligned with T3 Code's ThreadPanel: machine & folder, smart Open in
// <Editor / Finder> with picker, Open in Terminal, branch, one-click
// Commit & push (localized <=20-char auto summary), and working-tree changes.
func (w *workbench) projectCard(c *ui.Context, p palette) {
	card := ui.Column(c.Key("project-card")).Width(268).Margin(12, 16, 0, 0).Padding(8).Gap(2).Radius(14).
		Background(p.sidebar).Border(1, p.line).Shrink(0).Label(w.tr("项目卡片", "Project card"))
	if p.dark {
		card.Shadow(0, 8, 24, 0, ui.Hex("#00000040"))
	} else {
		card.Shadow(0, 6, 20, 0, ui.Hex("#0F172A12"))
	}
	card.Children(func() {
		w.threadPanelRows(c, p, false)
	})
}

// diffLineCounts counts the added and removed lines of a unified diff.
func diffLineCounts(diff string) (added, removed int) {
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

func (w *workbench) connectionView(c *ui.Context, p palette) {
	if n, ok := w.visibleConnectionNotice(); ok {
		ui.Row(c).Padding(8, 24).Gap(8).Background(p.soft).Children(func() { w.noticeLine(c, p, n) })
	}
}
func (w *workbench) visibleConnectionNotice() (notice, bool) {
	if w.healthNotice.Level == noticeError && w.healthNotice.valid() {
		return notice{Text: w.tr("本地服务版本与界面不兼容。", "The local service version is incompatible with this window."), Level: noticeError}, true
	}
	if w.connNotice.Level != noticeInfo && w.connNotice.valid() {
		return notice{Text: w.tr("连接不可用，正在重试；草稿和回复会保留。", "Connection unavailable, retrying. Drafts and replies are kept."), Level: w.connNotice.Level}, true
	}
	if w.streamNotice.Level != noticeInfo && w.streamNotice.valid() {
		return notice{Text: w.tr("连接已中断，正在恢复；草稿和已收到的回复会保留。", "Connection interrupted, recovering. Drafts and received replies are kept."), Level: w.streamNotice.Level}, true
	}
	if w.connNotice.valid() && strings.Contains(w.connNotice.Text, "正在连接") {
		return notice{Text: w.tr("正在连接…", "Connecting…")}, true
	}
	return notice{}, false
}

type conversationEntry struct {
	id      string
	run     api.Run
	pending *submission
}

func (entry conversationEntry) createdAt() time.Time {
	return entry.run.CreatedAt
}

func (entry conversationEntry) inputText() string {
	if entry.pending != nil {
		return entry.pending.send.Message
	}
	return entry.run.Input
}

func (w *workbench) hasConversation(id string) bool {
	if comp := w.composers[id]; comp != nil && comp.pending != nil {
		return true
	}
	if w.frame.valid {
		_, found := w.frame.latest[id]
		return found
	}
	for _, run := range w.runs {
		if run.ThreadID == id {
			return true
		}
	}
	return false
}

func (w *workbench) conversationFor(threadID string) []conversationEntry {
	f := &w.frame
	if f.valid && f.conversationBuilt && f.conversationThread == threadID {
		return f.conversation
	}
	var entries []conversationEntry
	if f.valid {
		entries = f.conversation[:0]
	}
	for _, r := range w.runs {
		if r.ThreadID == threadID {
			entries = append(entries, conversationEntry{id: r.ID, run: r})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].createdAt().Before(entries[j].createdAt()) })
	if c := w.composers[threadID]; c != nil && c.pending != nil {
		entries = append(entries, conversationEntry{id: c.pending.key(), pending: c.pending})
	}
	if f.valid {
		f.conversationThread, f.conversation, f.conversationBuilt = threadID, entries, true
	}
	return entries
}

func (w *workbench) logView(c *ui.Context, p palette, narrow bool) {
	entries := w.conversationFor(w.selected)
	if w.timelineNav.open {
		w.timelineToolbar(c, p, entries)
	}
	state := w.logState(w.selected)
	state.Key = func(i int) any { return entries[i].id }
	state.Label = func(i int) string { return entries[i].inputText() }
	state.FollowEnd = true
	horizontal := float32(28)
	if narrow {
		horizontal = 16
	}
	// The list fills the stage; "Jump to latest" floats over its foot, as
	// the workbench's, instead of pushing the conversation down when it appears.
	ui.Column(c.Key("log-stage")).Grow(1).MinHeight(0).Children(func() {
		ui.List(c.Key("conversation:"+w.selected), state, len(entries), func(i int) { w.conversationRow(c, p, entries[i], narrow) }).
			Grow(1).Gap(36).Padding(28, horizontal, 36).Selectable().SelectionColor(accentSelection(p.dark)).CopyData(copyConversationSelection).Role(ui.RoleLog).Label(w.tr("会话内容", "Conversation"))
		w.timelineMinimap(c, p, entries, state, horizontal)
		if !state.AtEnd() {
			ui.Row(c.Key("jump-latest-dock")).Absolute().Left(0).Right(0).Bottom(10).Justify(ui.Center).PassThrough().Children(func() {
				jump := ui.Button(c.Key("jump-latest"), "").Label(w.tr("跳到最新", "Jump to latest")).Height(28).Padding(0, 12, 0, 10).Radius(14).
					Background(p.raised).Border(1, p.line).TextColor(p.muted).Shadow(0, 4, 14, 0, ui.Hex("#00000033"))
				if jump.Hovered() {
					jump.TextColor(p.ink).BorderColor(p.muted.Alpha(0.5))
				}
				jump.Children(func() {
					ui.Row(c).Gap(6).AlignItems(ui.Center).Children(func() {
						ui.Icon(c, downIcon).FontSize(13)
						ui.Text(c, w.tr("跳到最新", "Jump to latest")).FontSize(12).NoWrap()
					})
				})
				if jump.Clicked() {
					state.ScrollToEnd()
				}
			})
		}
	})
}

func (w *workbench) awaitingApproval(runID string) bool {
	for _, request := range w.requests {
		if request.RunID == runID && (request.Status == "pending" || request.Status == "responding") {
			return true
		}
	}
	return false
}

func (w *workbench) pendingStatusView(c *ui.Context, p palette, pending *submission) {
	switch {
	case pending.inFlight:
		ui.Text(c, w.tr("正在发送…", "Sending…")).FontSize(12).TextColor(p.muted)
	case pending.accepted:
		ui.Text(c, w.tr("等待回复…", "Waiting for reply…")).FontSize(12).TextColor(p.muted)
	default:
		ui.Row(c).Gap(10).AlignItems(ui.Center).Children(func() {
			ui.Text(c, w.tr("发送结果待确认，原指令已保留", "Send result unconfirmed; your message is kept")).FontSize(12).TextColor(p.danger).Wrap()
			if ui.Button(c, w.tr("重试原请求", "Retry original request")).Clicked() {
				w.retryPending(w.selected)
			}
		})
	}
}

func (w *workbench) runStatusView(c *ui.Context, p palette, run api.Run) {
	text, color := "", p.muted
	switch {
	case w.stoppingRuns[run.ID] && !run.Status.Terminal():
		text = w.tr("正在停止…", "Stopping…")
	case w.awaitingApproval(run.ID):
		text, color = w.tr("需要你的确认", "Needs your confirmation"), p.warning
	case run.Status == api.RunRunning:
		text = w.tr("正在处理", "Working")
	case run.Status == api.RunQueued:
		text = w.tr("等待开始…", "Waiting to start…")
	case run.Status == api.RunWaiting:
		text = w.tr("等待子任务结果", "Waiting on subtasks")
	case run.Status == api.RunCancelled:
		text, color = w.tr("已停止 · 已保留收到的回复", "Stopped · received reply kept"), p.warning
	case run.Status == api.RunInterrupted:
		text, color = w.tr("回复已中断 · 已保留收到的内容", "Interrupted · received content kept"), p.danger
	case run.Status == api.RunFailed:
		text, color = w.tr("回复失败 · 已保留收到的内容", "Failed · received content kept"), p.danger
	case run.Status == api.RunCompleted:
		seconds := int(run.UpdatedAt.Sub(run.CreatedAt).Seconds())
		if seconds < 1 {
			seconds = 1
		}
		text = w.tr("已完成 · "+formatElapsed(time.Duration(seconds)*time.Second), fmt.Sprintf("Done · %ds", seconds))
		color = p.faint
	default:
		text = statusLabel(run.Status)
	}
	ui.Text(c, text).FontSize(11.5).TextColor(color).NoWrap().Shrink(0)
}

func (w *workbench) composerView(c *ui.Context, p palette) {
	// MyGo invalidates the native paint layer on edits, focus, theme and
	// layout changes; reply deltas can reuse the input area as one image.
	ui.Row(c.Key("composer-region")).Padding(8, 28, 18).Justify(ui.Center).Background(p.surface).Cache().Children(func() {
		ui.Column(c).Grow(1).MaxWidth(readingWidth).Children(func() { w.composerBox(c, p, false) })
	})
}

func (w *workbench) observeComposerInput(threadID string, ev ui.InputEvent) {
	if threadID == "" {
		return
	}
	comp := w.composerFor(threadID)
	switch ev.Kind {
	case ui.InputCompose:
		if ev.Text != "" {
			comp.composing = true
			comp.compositionEnding = false
		} else if comp.composing {
			comp.composing = false
			comp.compositionEnding = true
			comp.lastCompositionSet = time.Now()
		}
	case ui.InputText:
		if comp.composing || comp.compositionEnding {
			comp.composing = false
			comp.compositionEnding = false
			comp.lastCompositionSet = time.Now()
		}
	}
}

func (w *workbench) composerKey(threadID string, k ui.TextKey) bool {
	comp := w.composerFor(threadID)
	compositionRecent := !comp.lastCompositionSet.IsZero() && time.Since(comp.lastCompositionSet) < compositionKeyHold
	if k.Key == ui.KeyEnter && (comp.composing || compositionRecent) {
		return true
	}
	if comp.composing {
		return false // Arrow/Tab keys still belong to the input method.
	}
	if k.Key == ui.KeyZ && k.Mods&ui.Shift == 0 && (k.Mods&ui.Cmd != 0 || k.Mods&ui.Super != 0) && w.undoComposerPaste(threadID, k) {
		return true
	}
	if w.pickerTextKey(threadID, k) {
		return true
	}
	if w.skillTextKey(threadID, k) {
		return true
	}
	if k.Key != ui.KeyEnter {
		return false
	}
	// Empty preedit can mean cancellation, with no following InputText. Keep
	// the same short boundary guard, then allow an intentional later send.
	comp.compositionEnding = false
	newlineChord, sendChord, alternate := composerEnterBehavior(w.widgets.sendWithEnter, k.Mods, runtime.GOOS)
	if newlineChord {
		comp.newlinePending = true
		return true
	}
	if !sendChord {
		return false
	}
	w.setDraft(threadID, k.Text())
	if w.editingQueue(threadID) {
		w.saveQueueComposerEdit(threadID)
		return true
	}
	if run, active := w.activeRun(threadID); active {
		canSteer := active && run.Status == api.RunRunning && run.AttemptID != "" && w.controlCapable(threadID)
		w.queueDraft(threadID, followUpSteer(w.widgets.steerMode, canSteer, alternate))
	} else {
		w.send(threadID)
	}
	return true
}

// composerEnterBehavior resolves Enter+modifier chords across macOS and
// Windows/Linux. In the default Enter-to-send mode, macOS uses ⌘+Enter (and
// Shift+Enter) to insert a newline, while Windows/Linux maps Ctrl+Enter (and
// Shift+Enter) to newline so both platforms share the same muscle memory.
func composerEnterBehavior(sendWithEnter bool, mods ui.Modifiers, goos string) (newline, send, alternate bool) {
	if goos == "darwin" {
		if mods == ui.Super || mods == ui.Super|ui.Shift {
			return true, false, false
		}
		if sendWithEnter {
			send = mods == 0 || mods == ui.Ctrl || mods == ui.Ctrl|ui.Shift || mods == ui.Alt
			alternate = mods == ui.Ctrl|ui.Shift || mods == ui.Alt
			return false, send, alternate
		}
		send = mods == ui.Ctrl || mods == ui.Ctrl|ui.Shift
		alternate = mods == ui.Ctrl|ui.Shift
		return false, send, alternate
	}
	if sendWithEnter {
		if mods == ui.Ctrl || mods == ui.Ctrl|ui.Shift || mods == ui.Super || mods == ui.Super|ui.Shift {
			return true, false, false
		}
		send = mods == 0 || mods == ui.Alt || mods == ui.Alt|ui.Shift
		alternate = mods == ui.Alt || mods == ui.Alt|ui.Shift
		return false, send, alternate
	}
	if mods == ui.Super || mods == ui.Super|ui.Shift {
		return true, false, false
	}
	send = mods == ui.Ctrl || mods == ui.Ctrl|ui.Shift
	alternate = mods == ui.Ctrl|ui.Shift
	return false, send, alternate
}

func platformPrimaryNewlineKey(goos string) string {
	if goos == "darwin" {
		return "⌘ Enter"
	}
	return "Ctrl Enter"
}

func (w *workbench) applyPendingComposerNewline(c *ui.Context, field ui.Element, comp *composer, threadID string, draft *string) {
	if comp.newlinePending {
		comp.newlinePending = false
		anchor, caret := field.TextAnchorCaret()
		runes := []rune(*draft)
		a := min(max(0, min(anchor, caret)), len(runes))
		z := min(max(0, max(anchor, caret)), len(runes))
		*draft = string(runes[:a]) + "\n" + string(runes[z:])
		comp.caretAfter = a + 1
		comp.caretRestore = true
		w.setDraft(threadID, *draft)
		// The control was built before this edit. Request the frame that
		// adopts the new buffer and restores its caret.
		c.After(time.Millisecond)
	} else if comp.caretRestore {
		field.SetTextSelection(comp.caretAfter, comp.caretAfter)
		comp.liveAnchor, comp.liveCaret = comp.caretAfter, comp.caretAfter
		comp.caretRestore = false
	} else {
		comp.liveAnchor, comp.liveCaret = field.TextAnchorCaret()
	}
}

func (w *workbench) composerLine(comp *composer, active bool, run api.Run) notice {
	if comp.feedback.valid() {
		return comp.feedback
	}
	if w.actionNotice.valid() {
		return w.actionNotice
	}
	if w.stateNotice.valid() {
		return w.stateNotice
	}
	if active && w.stoppingRuns[run.ID] {
		return notice{Text: w.tr("正在确认停止结果；已有输出和草稿会保留。", "Confirming the stop; output and drafts are kept."), Level: noticeInfo}
	}
	return notice{}
}

func (w *workbench) countersText(narrow bool) string {
	text := fmt.Sprintf("游标 %d · 已折叠事件 %d", w.sequence, w.appliedEvents)
	if w.staleEvents > 0 {
		text += fmt.Sprintf(" · 重复 %d", w.staleEvents)
	}
	if w.unknownEvents > 0 {
		text += fmt.Sprintf(" · 待重同步 %d", w.unknownEvents)
	}
	if !narrow {
		text += fmt.Sprintf(" · 协议 v%d", api.ProtocolVersion)
	}
	return text
}
func (w *workbench) noticeLine(c *ui.Context, p palette, n notice) {
	if !n.valid() {
		return
	}
	text := ui.Text(c, n.Text).FontSize(11).TextColor(w.noticeColor(p, n.Level)).Wrap().Selectable()
	if n.Level == noticeError {
		text.Invalid(true)
	}
}
func (w *workbench) noticeColor(p palette, level noticeLevel) ui.Color {
	if level == noticeError {
		return p.danger
	}
	if level == noticeWarn {
		return p.warning
	}
	return p.muted
}
func statusLabel(status api.RunStatus) string {
	switch status {
	case api.RunQueued:
		return "排队中"
	case api.RunRunning:
		return "进行中"
	case api.RunWaiting:
		return "等待子任务"
	case api.RunCompleted:
		return "已完成"
	case api.RunCancelled:
		return "已停止"
	case api.RunInterrupted:
		return "已中断"
	case api.RunFailed:
		return "失败"
	case "":
		return "状态未知"
	default:
		return "状态 " + string(status)
	}
}

func (w *workbench) escStopHint() string {
	return w.tr("再按一次 Esc 停止当前任务（保留已有输出与草稿）", "Press Esc again to stop the current turn (keeps output and draft)")
}

func (w *workbench) hasOpenOverlay(c *ui.Context) bool {
	if w.stage != "" ||
		w.widgets.settingsOpen ||
		w.widgets.piSetup.open ||
		w.widgets.usageOpen ||
		w.widgets.shortcutsOpen ||
		w.widgets.diffOpen ||
		w.widgets.turnDiff != "" ||
		w.widgets.rewind.open ||
		w.widgets.fork.open ||
		w.widgets.inboxOpen ||
		w.gitView.expanded ||
		w.gitView.commitOpen ||
		w.gitView.branchOpen ||
		w.gitView.pathOpen ||
		w.mcp.addOpen ||
		w.visuals.expanded ||
		len(w.visuals.lightbox) > 0 ||
		w.widgets.modelPickerOpen ||
		w.widgets.projectPickerOpen ||
		w.widgets.thinkingOpen ||
		w.widgets.statsPopoverOpen ||
		w.widgets.workspaceOpen ||
		w.timelineNav.open ||
		w.features.panel != "" ||
		w.remoteDirectories.open ||
		(w.environments != nil && w.environments.manage) ||
		w.editingQueue(w.selected) ||
		w.sessions.renamingID != "" ||
		w.subagentPanelOpen() {
		return true
	}
	for _, openPtr := range w.widgets.snoozeOpen {
		if openPtr != nil && *openPtr {
			return true
		}
	}
	if comp := w.composers[w.selected]; comp != nil {
		if comp.composing || (!comp.lastCompositionSet.IsZero() && time.Since(comp.lastCompositionSet) < compositionKeyHold) {
			return true
		}
		if w.pickerFor(w.selected, comp.draft).open || w.skillStateFor(w.selected, comp.draft).open {
			return true
		}
	}
	if c != nil && w.widgets.searchFocus.Focused(c) {
		return true
	}
	return false
}

func (w *workbench) handleBareEscape() bool {
	if w.widgets.frameSeq != 0 && w.widgets.escHandledFrame == w.widgets.frameSeq {
		return true
	}
	if w.widgets.frameSeq != 0 {
		w.widgets.escHandledFrame = w.widgets.frameSeq
	}
	now := w.widgets.frameNow
	if now.IsZero() {
		now = time.Now()
	}
	if _, active := w.activeRun(w.selected); active {
		if w.widgets.escArmedMode == "stop" && !w.widgets.escArmedAt.IsZero() && now.Sub(w.widgets.escArmedAt) <= 1200*time.Millisecond {
			w.widgets.escArmedMode = ""
			w.widgets.escArmedAt = time.Time{}
			w.stopActiveRun()
			return true
		}
		w.widgets.escArmedMode = "stop"
		w.widgets.escArmedAt = now
		w.setAction(w.escStopHint())
		return true
	}
	if w.hasThread(w.selected) && !isLocalSession(w.selected) {
		for _, r := range w.runsFor(w.selected) {
			if r.Status.Terminal() {
				if w.widgets.escArmedMode == "rewind" && !w.widgets.escArmedAt.IsZero() && now.Sub(w.widgets.escArmedAt) <= 600*time.Millisecond {
					w.widgets.escArmedMode = ""
					w.widgets.escArmedAt = time.Time{}
					w.openRewindDialog("")
					return true
				}
				w.widgets.escArmedMode = "rewind"
				w.widgets.escArmedAt = now
				return true
			}
		}
	}
	return false
}

func (w *workbench) toggleAllTools() {
	next := !w.widgets.defaultToolsExpanded
	w.widgets.defaultToolsExpanded = next
	if w.widgets.workOpen == nil {
		w.widgets.workOpen = map[string]bool{}
	}
	if w.widgets.toolExpanded == nil {
		w.widgets.toolExpanded = map[string]bool{}
	}
	for _, r := range w.runsFor(w.selected) {
		tools := w.runTools(r.ID)
		for _, tool := range tools {
			w.widgets.toolExpanded[tool.ID] = next
		}
		for idx, part := range splitTurn(r.Output, tools) {
			if part.tools != nil {
				w.widgets.workOpen[r.ID+":"+strconv.Itoa(idx)] = next
			}
		}
	}
	if next {
		w.setAction(w.tr("已展开全部工具明细 (Ctrl+O 收起)", "Expanded all tool details (Ctrl+O to collapse)"))
	} else {
		w.setAction(w.tr("已收起全部工具明细 (Ctrl+O 展开)", "Collapsed all tool details (Ctrl+O to expand)"))
	}
}

func (w *workbench) toggleAllReasoning() {
	next := !w.widgets.defaultReasoningOpen
	w.widgets.defaultReasoningOpen = next
	if w.widgets.reasoningOpen == nil {
		w.widgets.reasoningOpen = map[string]bool{}
	}
	for _, r := range w.runsFor(w.selected) {
		if r.Reasoning != "" {
			w.widgets.reasoningOpen[r.ID] = next
		}
	}
	if next {
		w.setAction(w.tr("已展开全部思考过程 (Ctrl+T 收起)", "Expanded all reasoning blocks (Ctrl+T to collapse)"))
	} else {
		w.setAction(w.tr("已收起全部思考过程 (Ctrl+T 展开)", "Collapsed all reasoning blocks (Ctrl+T to expand)"))
	}
}
