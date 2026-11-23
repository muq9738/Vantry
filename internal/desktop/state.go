package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

const desktopStateVersion = 1

type persistedOptions struct {
	ProviderID     string             `json:"providerId"`
	Model          string             `json:"model,omitempty"` // legacy UI selection; ignored on restore
	Workspace      *api.WorkspaceSpec `json:"workspace,omitempty"`
	ApprovalPolicy string             `json:"approvalPolicy"`
}

type persistedMention struct {
	Kind         mentionKind `json:"kind"`
	Display      string      `json:"display"`
	Expand       string      `json:"expand"`
	AttachmentID string      `json:"attachmentId,omitempty"`
}

type persistedState struct {
	Version           int                           `json:"version"`
	Selected          string                        `json:"selected"`
	Drafts            map[string]string             `json:"drafts,omitempty"`
	DraftAttachments  map[string][]api.Attachment   `json:"draftAttachments,omitempty"`
	DraftMentions     map[string][]persistedMention `json:"draftMentions,omitempty"`
	DraftPasteSeq     map[string]int                `json:"draftPasteSeq,omitempty"`
	LocalOrder        []string                      `json:"localOrder,omitempty"`
	ThemeSet          bool                          `json:"themeSet"`
	Dark              bool                          `json:"dark"`
	ThemePreset string                        `json:"themePreset,omitempty"`
	SendWithEnter     bool                          `json:"sendWithEnter"`
	ActiveTurnDefault string                        `json:"activeTurnDefault,omitempty"`
	ManualDelivery    bool                          `json:"manualDelivery"`
	Locale            string                        `json:"locale,omitempty"` // "zh" (default), "en", or "ja"
	PreferredEditor   string                        `json:"preferredEditor,omitempty"`
	Sections          map[string]bool               `json:"sections,omitempty"`
	Defaults          persistedOptions              `json:"defaults"`
	// RemoteFavorites are aliases for SSH targets; never credentials.
	RemoteFavorites []remoteFavorite `json:"remoteFavorites,omitempty"`
	RemovedHosts    []string         `json:"removedHosts,omitempty"`
	// ManualHosts are the machines added by hand, RecentHosts when each was
	// last connected to: neither holds a credential.
	ManualHosts []manualSSHHost      `json:"manualHosts,omitempty"`
	RecentHosts map[string]time.Time `json:"recentHosts,omitempty"`
	// SubagentPanels is per root session: the open flag and the child on show.
	SubagentPanels map[string]persistedSubagentPanel `json:"subagentPanels,omitempty"`
	AskAnswers     map[string]askAnswerRecord        `json:"askAnswers,omitempty"`
}

type persistedSubagentPanel struct {
	Open     bool   `json:"open"`
	ThreadID string `json:"threadId,omitempty"`
}

// restoreState deliberately has no command/outbox representation. An
// unconfirmed instruction from an earlier UI process cannot become a fresh
// draft and cannot be dispatched during startup.
func (w *workbench) restoreState() error {
	// The launcher resolves Pi settings for this startup. A model saved by an
	// older desktop is a previous selection, not the next session's default.
	w.defaultModel = w.defaults.Model
	if w.defaults.StatePath == "" {
		return nil
	}
	data, err := os.ReadFile(w.defaults.StatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Version != desktopStateVersion {
		return fmt.Errorf("unsupported desktop state version %d", state.Version)
	}
	// The executor comes from the launch options. A saved offline demo choice
	// from an older build must not turn every new thread into a demo.
	if state.Defaults.ProviderID != "" && state.Defaults.ProviderID != "controlled" {
		w.defaults.ProviderID = state.Defaults.ProviderID
	}
	w.defaults.Workspace = cloneWorkspace(state.Defaults.Workspace)
	w.defaults.ApprovalPolicy = normalizeApprovalPolicy(state.Defaults.ApprovalPolicy)
	w.widgets.themeSet = state.ThemeSet
	w.widgets.dark = state.Dark
	w.widgets.themePreset = normalizeThemePreset(state.ThemePreset)
	w.widgets.sendWithEnter = state.SendWithEnter
	w.widgets.steerMode = state.ActiveTurnDefault != "queue"
	w.widgets.manualDelivery = state.ManualDelivery
	w.widgets.locale = normalizeLocale(state.Locale)
	w.widgets.preferredEditor = strings.TrimSpace(state.PreferredEditor)
	w.remote.removed = append([]string(nil), state.RemovedHosts...)
	for _, host := range state.ManualHosts {
		if strings.TrimSpace(host.Host) != "" {
			w.ssh.manual = append(w.ssh.manual, host)
		}
	}
	if len(state.RecentHosts) > 0 {
		w.ssh.recent = make(map[string]time.Time, len(state.RecentHosts))
		for key, at := range state.RecentHosts {
			w.ssh.recent[key] = at
		}
	}
	w.remote.saved = nil
	for _, f := range state.RemoteFavorites {
		if strings.TrimSpace(f.Host) != "" {
			w.remote.saved = append(w.remote.saved, f)
		}
	}
	for key, open := range state.Sections {
		*w.projectExpanded(key) = open
	}
	for id, draft := range state.Drafts {
		if id != "" {
			w.composerFor(id).draft = draft
		}
	}
	for id, items := range state.DraftAttachments {
		if id != "" && len(items) > 0 {
			w.composerFor(id).attachments = append([]api.Attachment(nil), items...)
		}
	}
	for id, mentions := range state.DraftMentions {
		if id == "" || len(mentions) == 0 {
			continue
		}
		comp := w.composerFor(id)
		for _, pm := range mentions {
			if pm.Display == "" {
				continue
			}
			comp.addMention(composerMention{
				kind:         pm.Kind,
				display:      pm.Display,
				expand:       pm.Expand,
				attachmentID: pm.AttachmentID,
			})
		}
		comp.pruneMentions()
	}
	for id, seq := range state.DraftPasteSeq {
		if id != "" && seq > 0 {
			comp := w.composerFor(id)
			if seq > comp.pasteSeq {
				comp.pasteSeq = seq
			}
		}
	}
	seen := map[string]bool{}
	for _, id := range state.LocalOrder {
		if !isLocalSession(id) || seen[id] {
			continue
		}
		seen[id] = true
		w.localOrder = append(w.localOrder, id)
		w.composerFor(id)
		w.noteLocalSerial(id)
	}
	if isLocalSession(state.Selected) && !seen[state.Selected] {
		w.localOrder = append(w.localOrder, state.Selected)
		w.composerFor(state.Selected)
		w.noteLocalSerial(state.Selected)
	}
	w.selected = state.Selected
	for id, panel := range state.SubagentPanels {
		if id == "" || (panel.ThreadID == "" && !panel.Open) {
			continue
		}
		st := w.subagentPanelState(id)
		st.open, st.threadID = panel.Open, panel.ThreadID
	}
	if len(state.AskAnswers) > 0 {
		w.visuals.askAnswers = make(map[string]askAnswerRecord, len(state.AskAnswers))
		for key, rec := range state.AskAnswers {
			if key != "" && (rec.Skipped || strings.TrimSpace(rec.Answer) != "") {
				w.visuals.askAnswers[key] = rec
			}
		}
	}
	return nil
}

func normalizeApprovalPolicy(policy string) string {
	switch policy {
	case "ask", "auto-accept-edits":
		return policy
	}
	return "auto"
}

func (w *workbench) noteLocalSerial(id string) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, localSessionPrefix))
	if err == nil && n > w.localSerial {
		w.localSerial = n
	}
}

// saveState is a no-op unless the parent explicitly supplied StatePath. The
// temporary file is written and synced beside the destination, then renamed so
// a restart observes either complete generation, never partial JSON.
func (w *workbench) saveState() {
	if w == nil || w.defaults.StatePath == "" {
		return
	}
	activeDefault := "steer"
	if !w.widgets.steerMode {
		activeDefault = "queue"
	}
	state := persistedState{
		ActiveTurnDefault: activeDefault,
		Version:           desktopStateVersion,
		Selected:          w.selected,
		Drafts:            map[string]string{},
		LocalOrder:        append([]string(nil), w.localOrder...),
		ThemeSet:          w.widgets.themeSet,
		Dark:              w.widgets.dark,
		ThemePreset: normalizeThemePreset(w.widgets.themePreset),
		SendWithEnter:     w.widgets.sendWithEnter,
		ManualDelivery:    w.widgets.manualDelivery,
		Locale:            normalizeLocale(w.widgets.locale),
		PreferredEditor:   strings.TrimSpace(w.widgets.preferredEditor),
		RemoteFavorites:   append([]remoteFavorite(nil), w.remote.saved...),
		RemovedHosts:      append([]string(nil), w.remote.removed...),
		ManualHosts:       append([]manualSSHHost(nil), w.ssh.manual...),
		RecentHosts:       w.ssh.recent,
		SubagentPanels:    w.persistedSubagentPanels(),
		AskAnswers:        cloneAskAnswers(w.visuals.askAnswers),
		Defaults: persistedOptions{
			ProviderID:     w.defaults.ProviderID,
			Workspace:      cloneWorkspace(w.defaults.Workspace),
			ApprovalPolicy: normalizeApprovalPolicy(w.defaults.ApprovalPolicy),
		},
	}
	state.Sections = make(map[string]bool, len(w.features.projectOpen))
	for key, open := range w.features.projectOpen {
		if open != nil {
			state.Sections[key] = *open
		}
	}
	for id, composer := range w.composers {
		if composer == nil {
			continue
		}
		if edit := w.queue.composer; edit != nil && edit.input.ThreadID == id {
			// An in-progress queue edit must not overwrite the unsent draft
			// that will be restored when editing finishes.
			original := *composer
			original.draft, original.attachments, original.mentions = edit.draft, edit.attachments, edit.mentions
			composer = &original
		}
		if composer.draft != "" {
			state.Drafts[id] = composer.draft
		}
		if len(composer.attachments) > 0 {
			if state.DraftAttachments == nil {
				state.DraftAttachments = map[string][]api.Attachment{}
			}
			state.DraftAttachments[id] = append([]api.Attachment(nil), composer.attachments...)
		}
		if len(composer.mentions) > 0 {
			if state.DraftMentions == nil {
				state.DraftMentions = map[string][]persistedMention{}
			}
			saved := make([]persistedMention, 0, len(composer.mentions))
			for _, m := range composer.mentions {
				saved = append(saved, persistedMention{
					Kind:         m.kind,
					Display:      m.display,
					Expand:       m.expand,
					AttachmentID: m.attachmentID,
				})
			}
			state.DraftMentions[id] = saved
		}
		if composer.pasteSeq > 0 && (composer.draft != "" || len(composer.attachments) > 0) {
			if state.DraftPasteSeq == nil {
				state.DraftPasteSeq = map[string]int{}
			}
			state.DraftPasteSeq[id] = composer.pasteSeq
		}
	}
	if err := writeStateAtomic(w.defaults.StatePath, state); err != nil {
		w.stateNotice = notice{Text: "界面状态保存失败；当前窗口内状态仍保留。", Level: noticeWarn}
		return
	}
	w.stateNotice = notice{}
}

func writeStateAtomic(path string, state persistedState) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".desktop-state-*.tmp")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(state); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}
