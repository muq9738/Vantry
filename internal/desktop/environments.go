package desktop

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/pkg/api"
)

// EnvironmentSession is a connected Core owned by a different machine.
// Its service, model context and lifecycle never fall back to the local Core.
type EnvironmentSession struct {
	Service api.Service
	Info    api.EnvironmentInfo
	Close   func() error
	Mode    func() string
}

type environmentRecord struct {
	key, connectionID, label string
	view                     *workbench
	close                    func() error
	cancel                   context.CancelFunc
	// connecting marks a dial in progress and failed its last outcome, so
	// one unreachable machine never stalls the others or the local list.
	connecting bool
	failed     string
}
type environmentHub struct {
	root, current *workbench
	records       []environmentRecord
	// filter narrows the aggregated sidebar to one machine's identity;
	// "" lists every machine's threads.
	filter  string
	failure string
	manage  bool
	pairing string
	status  remoteaccess.Status
	ctx     context.Context
	bind    func(*workbench)
}

func newEnvironmentHub(root *workbench, ctx context.Context) *environmentHub {
	h := &environmentHub{root: root, current: root, ctx: ctx, records: []environmentRecord{{key: "local", label: root.hostLabel(), view: root}}}
	root.environments = h
	root.environmentKey = "local"
	return h
}
func (h *environmentHub) syncStatus(status remoteaccess.Status) {
	h.status = status
	h.root.access.status = status
	if h.root.access.name == "" {
		h.root.access.name = nonEmptyString(status.Name, machineName())
	}
	if h.root.access.relay == "" {
		h.root.access.relay = status.Relay
	}
	if h.root.access.publicURL == "" {
		h.root.access.publicURL = status.PublicURL
	}
	present := map[string]remoteaccess.PeerInfo{}
	for _, peer := range status.Peers {
		present[peer.ID] = peer
	}
	kept := make([]environmentRecord, 0, len(h.records)+len(status.Peers))
	seen := map[string]bool{}
	for _, record := range h.records {
		if record.connectionID == "" {
			if name := strings.TrimSpace(status.Name); name != "" {
				record.label = name
			}
			kept = append(kept, record)
			continue
		}
		peer, ok := present[record.connectionID]
		if !ok {
			if record.view != nil {
				record.view.releaseRemoteDesktop()
				record.view.closeTerminals()
				record.view.saveState()
			}
			if h.current == record.view {
				h.current = h.root
			}
			if record.cancel != nil {
				record.cancel()
			}
			if record.close != nil {
				_ = record.close()
			}
			if h.filter == record.connectionID {
				h.filter = ""
			}
			continue
		}
		record.label = peer.Name
		if peer.DeviceID != "" && record.view == nil {
			record.key = peer.DeviceID
		}
		if record.view != nil {
			record.view.defaults.RemoteName = peer.Name
		}
		kept = append(kept, record)
		seen[peer.ID] = true
	}
	for _, peer := range status.Peers {
		if !seen[peer.ID] {
			kept = append(kept, environmentRecord{key: peer.DeviceID, connectionID: peer.ID, label: peer.Name})
		}
	}
	h.records = kept
}

func (h *environmentHub) recordByConnection(connectionID string) *environmentRecord {
	if connectionID == "" {
		return nil
	}
	for i := range h.records {
		if h.records[i].connectionID == connectionID {
			return &h.records[i]
		}
	}
	return nil
}

func (h *environmentHub) disconnectRecord(connectionID string) {
	record := h.recordByConnection(connectionID)
	if record == nil {
		return
	}
	if record.view != nil {
		record.view.releaseRemoteDesktop()
		record.view.closeTerminals()
		record.view.saveState()
	}
	if h.current == record.view {
		h.current = h.root
	}
	if record.cancel != nil {
		record.cancel()
	}
	if record.close != nil {
		_ = record.close()
	}
	record.view = nil
	record.cancel = nil
	record.close = nil
	record.connecting = false
	record.failed = ""
}

func (h *environmentHub) reconnectRecord(connectionID string) {
	h.disconnectRecord(connectionID)
	if record := h.recordByConnection(connectionID); record != nil {
		h.connectRecord(record, nil, false)
	}
}

func (h *environmentHub) load() {
	service, ok := h.root.service.(api.WorkbenchService)
	if !ok {
		return
	}
	h.root.start(func() {
		response, err := service.Workbench(h.ctx, api.WorkbenchRequest{Action: "access.status"})
		h.root.post(func() {
			if err != nil {
				h.failure = describe("读取机器连接失败", err)
				return
			}
			var status remoteaccess.Status
			if json.Unmarshal([]byte(response.Output), &status) != nil {
				return
			}
			h.syncStatus(status)
			// Saved machines connect in the background so the sidebar can
			// aggregate their sessions; the current conversation stays put.
			h.ensureViews()
		})
	})
}

// ensureViews starts background connections for saved machines that have
// none yet. The caller's stage never changes and each machine dials on its
// own, so an unreachable peer leaves local and other remote threads listed.
func (h *environmentHub) ensureViews() {
	if h.root.defaults.ConnectEnvironment == nil {
		return
	}
	for i := range h.records {
		record := &h.records[i]
		if record.view != nil || record.connecting || record.connectionID == "" {
			continue
		}
		h.connectRecord(record, nil, false)
	}
}
func (h *environmentHub) choose(source *workbench, index int, draft bool) {
	if index < 0 || index >= len(h.records) {
		return
	}
	record := &h.records[index]
	sourceSession := source.selected
	if draft && (!isLocalSession(sourceSession) || source.composerFor(sourceSession).pending != nil || source.composerFor(sourceSession).uploading) {
		source.setAction("已建立或正在发送的会话不能更换运行机器，请先新建会话。")
		return
	}
	if draft && len(source.composerFor(source.selected).attachments) > 0 {
		source.setAction("请先移除附件，切换机器后再在目标机器添加。")
		return
	}
	activate := func(target *workbench) {
		if draft && (source.selected != sourceSession || !isLocalSession(sourceSession) || source.composerFor(sourceSession).pending != nil) {
			source.setAction("草稿状态已变化，请重新选择运行机器。")
			return
		}
		if draft && target != source {
			text := source.composerFor(source.selected).draft
			target.newSession()
			target.setDraft(target.selected, text)
			source.setDraft(source.selected, "")
			source.saveState()
			target.saveState()
		}
		if h.current != nil && h.current != target {
			h.current.cancelRemoteHostingIntent()
			h.current.releaseRemoteKeys()
		}
		h.current = target
		target.widgets.composerFocus.Focus()
	}
	if record.view != nil {
		activate(record.view)
		return
	}
	if record.connecting {
		if !draft {
			return
		}
		source.setAction("正在连接该机器，请稍后。")
		return
	}
	if h.root.defaults.ConnectEnvironment == nil {
		source.setAction("当前窗口不支持连接其他机器。")
		return
	}
	h.connectRecord(record, func(target *workbench) {
		if target == nil {
			source.setAction(h.failure)
			return
		}
		activate(target)
	}, true)
}

// connectRecord dials one saved machine. activate, when given, runs with the
// new view on success or nil on failure; it is what makes an explicit machine
// choice bring the stage over, while a background listing dial only fills the
// sidebar. The connected view is a separate workbench so every thread ID,
// draft, request, subscription cursor and provider selection stays in its
// owning Core.
func (h *environmentHub) connectRecord(record *environmentRecord, activate func(*workbench), userFacing bool) {
	connector := h.root.defaults.ConnectEnvironment
	if connector == nil || record.connecting || record.view != nil || record.connectionID == "" {
		return
	}
	record.connecting = true
	record.failed = ""
	if userFacing {
		h.failure = ""
	}
	connectionCtx, cancelConnection := context.WithCancel(h.ctx)
	h.root.start(func() {
		session, err := connector(connectionCtx, record.connectionID)
		h.root.post(func() {
			// Peers refreshed while dialing may have reallocated records;
			// resolve this machine again by its stable connection identity.
			current := record
			for i := range h.records {
				if h.records[i].connectionID == record.connectionID {
					current = &h.records[i]
					break
				}
			}
			if current != record {
				record = current
			}
			record.connecting = false
			if err != nil {
				cancelConnection()
				record.failed = describe("连接 "+record.label+" 失败", err)
				if userFacing {
					h.failure = record.failed
				}
				if activate != nil {
					activate(nil)
				}
				return
			}
			if h.ctx.Err() != nil {
				cancelConnection()
				if session.Close != nil {
					_ = session.Close()
				}
				return
			}
			target := newWorkbench(session.Service)
			target.defaults = Options{ProviderID: session.Info.ProviderID, Model: session.Info.DefaultModel, Workspace: cloneWorkspace(session.Info.Workspace), ApprovalPolicy: "auto", RemotePeer: true, RemoteName: record.label, ConnectionMode: session.Mode, Version: h.root.defaults.Version, RemoteDesktopFactory: h.root.defaults.RemoteDesktopFactory}
			if h.root.defaults.StatePath != "" {
				target.defaults.StatePath = filepath.Join(filepath.Dir(h.root.defaults.StatePath), "environment-state", environmentStateComponent(session.Info.ID), "desktop-state.json")
			}
			target.environments = h
			target.environmentKey = session.Info.ID
			target.environmentInfo = session.Info
			target.widgets.locale = h.root.widgets.locale
			target.ctx = connectionCtx
			target.initializeRemoteDesktop()
			_ = target.restoreState()
			record.view = target
			record.key = session.Info.ID
			record.failed = ""
			record.close = session.Close
			record.cancel = cancelConnection
			if h.bind != nil {
				h.bind(target)
			}
			if activate != nil {
				activate(target)
			}
		})
	})
}

func (h *environmentHub) close() {
	for _, record := range h.records {
		if record.view != nil {
			record.view.releaseRemoteDesktop()
			record.view.closeTerminals()
			record.view.saveState()
		}
		if record.cancel != nil {
			record.cancel()
		}
		if record.close != nil {
			_ = record.close()
		}
	}
}

// A proxy thread belongs to this Core's task ledger but executes in another
// environment. Keep that execution identity distinct from the window's Core.
func (w *workbench) proxyEnvironmentID() string {
	if thread, ok := w.selectedThread(); ok && thread.Workspace != nil {
		return thread.Workspace.EnvironmentID
	}
	return ""
}

func (w *workbench) proxyEnvironmentLabel() string {
	id := w.proxyEnvironmentID()
	if hub := w.environments; hub != nil {
		for _, record := range hub.records {
			if (record.key == id || record.connectionID == id) && record.label != "" {
				return record.label
			}
		}
	}
	return id
}

func (w *workbench) environmentMenu(c *ui.Context, p palette, composer bool) {
	h := w.environments
	if h == nil {
		return
	}
	locked := composer && w.hasThread(w.selected) && !isLocalSession(w.selected)
	label := w.tr("运行机器", "Run on")
	if !composer {
		label = w.tr("执行环境", "Execution environment")
	}
	button := quietButton(c, p, "environment-menu", "").Label(label).Height(26).Padding(0, 8).MinWidth(0)
	button.Children(func() {
		ui.Row(c).Gap(5).AlignItems(ui.Center).Children(func() {
			ui.Icon(c, serverIcon).FontSize(12).TextColor(p.muted)
			ui.Text(c, w.hostLabel()).FontSize(11).TextColor(p.muted).SingleLine().Ellipsis("…")
			ui.Icon(c, chevronIcon).FontSize(9).TextColor(p.faint)
		})
	})
	button.PopoverMenuButton(func(m *ui.Menu) {
		for i, record := range h.records {
			selected := record.view == w
			if m.Item(record.label).Radio(selected).Disabled(locked && !selected || record.connecting).Chosen() {
				h.choose(w, i, composer)
			}
		}
		m.Separator()
		if m.Item(w.tr("连接另一台机器…", "Connect another machine…")).Chosen() {
			h.manage = true
			h.load()
		}
		if m.Item(w.tr("环境设置…", "Environment settings…")).Chosen() {
			w.openEnvironmentSettings("")
		}
		if m.Item(w.tr("刷新机器连接", "Refresh machine connections")).Chosen() {
			h.load()
		}
	})
	if h.anyConnecting() {
		ui.Text(c, w.tr("正在连接…", "Connecting…")).FontSize(10).TextColor(p.faint)
	}
	if h.failure != "" {
		button.Tooltip(h.failure)
	}
	if locked {
		button.Description(w.tr("会话已固定在这台机器；新建会话后可选择其他机器。", "This thread stays on its machine. Choose another machine in a new draft."))
	}
}

func (w *workbench) environmentSidebar(c *ui.Context, p palette) {
	if w.environments == nil {
		return
	}
	ui.Row(c.Key("environment-sidebar")).Padding(0, 10, 6).Gap(4).AlignItems(ui.Center).Children(func() {
		w.machineFilterMenu(c, p)
		ui.Spacer(c)
		if iconButton(c, p, "manage-machines", w.tr("连接机器", "Connect machines"), plusIcon).Size(24, 24).Padding(5).Clicked() {
			w.environments.manage = true
			w.environments.load()
		}
	})
	w.machineStatusRows(c, p)
	if failure := w.environments.failure; failure != "" {
		ui.Text(c, failure).FontSize(10).TextColor(p.warning).Wrap().Padding(0, 12, 8)
	}
}

func environmentStateComponent(value string) string {
	value = strings.NewReplacer("/", "_", "\\", "_", ":", "_").Replace(value)
	if value == "" || value == "." || value == ".." {
		return "environment"
	}
	return value
}
