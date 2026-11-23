package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/adapters/attachments"
	"github.com/muq9738/Vantry/internal/adapters/files"
	gitwork "github.com/muq9738/Vantry/internal/adapters/git"
	"github.com/muq9738/Vantry/internal/adapters/pr"
	"github.com/muq9738/Vantry/internal/adapters/preview"
	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/environments"
	"github.com/muq9738/Vantry/internal/hostplatform"
	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/internal/plugins"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/internal/sshconfig"
	"github.com/muq9738/Vantry/internal/telemetry"
	"github.com/muq9738/Vantry/internal/updater"
	"github.com/muq9738/Vantry/pkg/api"
)

type workbenchService struct {
	api.Service
	config        Config
	ledger        *telemetry.Ledger
	rdMu          sync.Mutex
	rd            *RemoteDesktopBridge
	upd           *updater.Manager
	accessMu      sync.Mutex
	access        *remoteaccess.Manager
	root          context.Context
	environmentMu sync.Mutex
	federator     *environments.Federator
	skillsMu      sync.Mutex
	skills        map[string]skillCatalogEntry
	permissions   *remotePermissionBackend
}

func (s *workbenchService) Workbench(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	timeout := 30 * time.Second
	switch request.Action {
	case "rd.permission.request":
		timeout = 75 * time.Second
	case "mcp.probe", "pi.setup.save":
		timeout = 35 * time.Second
	case "pi.check_update":
		timeout = 3 * time.Minute
	case "update.install":
		timeout = 15 * time.Minute
	case "pi.upgrade":
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var response api.WorkbenchResponse
	switch request.Action {
	case "pi.status", "pi.check_update", "pi.upgrade", "pi.settings.set", "pi.setup.save":
		return s.piRuntimeAction(ctx, request)
	case "mcp.list", "mcp.probe", "mcp.upsert", "mcp.remove", "mcp.trust":
		return s.mcp(ctx, request)
	case "reply.image":
		return s.replyImage(ctx, request)
	case "orchestration.environments":
		return s.orchestrationEnvironments(ctx)
	case "environment.info":
		info, err := s.environmentInfo()
		if err != nil {
			return response, err
		}
		return environmentOutput(info)
	case "environment.directories":
		listing, err := environmentDirectories(request.Workspace)
		if err != nil {
			return response, err
		}
		return environmentOutput(listing)
	case "models":
		directory := request.Workspace
		if request.ThreadID != "" {
			spec, err := s.threadWorkspace(ctx, request.ThreadID)
			if err != nil {
				return response, err
			}
			if spec != nil {
				path, err := workspace.ThreadDirectory(ctx, s.config.DataDir, spec, request.ThreadID)
				if err != nil {
					return response, err
				}
				directory = path
			} else {
				directory = ""
			}
		}
		var catalog api.ModelCatalog
		var err error
		piExe := s.effectivePiExecutable()
		if s.config.MagpieSettingsPath != "" {
			catalog, err = piprovider.MagpieCatalog(ctx, piExe, directory, request.Model, s.config.MagpieSettingsPath)
		} else {
			catalog, err = piprovider.Catalog(ctx, piExe, directory, request.Model)
		}
		response.Catalog = &catalog
		return response, err
	case "skills.list":
		return s.skillsList(ctx, request)
	case "attachment.read":
		var item api.Attachment
		if err := json.Unmarshal(request.Input, &item); err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: "附件信息无效"}
		}
		name, err := (attachments.Store{Directory: filepath.Join(s.config.DataDir, "attachments")}).Resolve(item)
		if err != nil {
			return response, err
		}
		response.Data, err = os.ReadFile(name)
		return response, err
	case "attachment.import":
		item, err := (attachments.Store{Directory: filepath.Join(s.config.DataDir, "attachments")}).Import(request.Name, request.Data)
		response.Attachment = &item
		return response, err
	case "clipboard.image":
		data, _, name, found, err := hostplatform.ReadClipboardMedia(ctx)
		if err != nil || !found {
			return response, err
		}
		item, err := (attachments.Store{Directory: filepath.Join(s.config.DataDir, "attachments")}).Import(name, data)
		response.Attachment = &item
		return response, err
	case "usage.summary":
		return s.usageSummary(ctx)
	case "sandbox.get", "sandbox.set":
		return s.sandboxAction(request)
	case "environments.list":
		cat, err := environments.New(s.config.DataDir)
		if err != nil {
			return response, err
		}
		entries, err := cat.Load()
		if err != nil {
			return response, err
		}
		raw, err := json.Marshal(map[string]any{"environments": entries})
		response.Output = string(raw)
		return response, err
	case "files.list", "files.read", "files.search":
		root, err := s.resolveRoot(ctx, request)
		if err != nil {
			return response, err
		}
		response.Workspace = root
		switch request.Action {
		case "files.list":
			relDir := ""
			if len(request.Paths) > 0 {
				relDir = request.Paths[0]
			}
			entries, err := files.ListDirectory(ctx, root, relDir, 500)
			if err != nil {
				return response, err
			}
			raw, err := json.Marshal(map[string]any{"entries": entries})
			response.Output = string(raw)
			return response, err
		case "files.read":
			if len(request.Paths) == 0 {
				return response, &api.Error{Code: api.InvalidCommand, Message: "缺少文件路径"}
			}
			view, err := files.ReadFilePreview(ctx, root, request.Paths[0], 1, 2000)
			if err != nil {
				return response, err
			}
			response.Output = view.Content
			return response, nil
		default:
			matches, err := files.SearchFiles(ctx, root, request.Message, 40)
			if err != nil {
				return response, err
			}
			raw, err := json.Marshal(map[string]any{"results": matches})
			response.Output = string(raw)
			return response, err
		}
	case "pr.status":
		path, err := s.resolveRoot(ctx, request)
		if err != nil {
			return response, err
		}
		info, err := pr.InspectBranchPR(ctx, path)
		if err != nil || info == nil {
			return response, err
		}
		checks := ""
		switch {
		case info.ChecksSummary.Failed > 0:
			checks = "failed"
		case info.ChecksSummary.Pending > 0:
			checks = "pending"
		case info.ChecksSummary.Total > 0:
			checks = "passed"
		}
		raw, err := json.Marshal(map[string]any{
			"number": info.Number, "title": info.Title, "state": info.State,
			"url": info.URL, "checks": checks, "pr": info,
		})
		response.Output = string(raw)
		return response, err
	case "preview.html":
		root, err := s.resolveRoot(ctx, request)
		if err != nil {
			return response, err
		}
		rendered, err := preview.RenderHTMLToPNG(ctx, root, request.Name, request.Message, 960, 640)
		if err != nil {
			return response, err
		}
		raw, err := json.Marshal(rendered)
		response.Output = string(raw)
		return response, err
	case "preview.ports":
		ports := preview.ScanActiveDevPorts(ctx, nil)
		raw, err := json.Marshal(map[string]any{"ports": ports})
		response.Output = string(raw)
		return response, err
	case "ssh.hosts":
		hosts, err := sshconfig.Discover("")
		response.SSHHosts = hosts
		return response, err
	case "ssh.probe":
		info, err := probeSSHHost(ctx, request)
		if err != nil {
			return response, err
		}
		response.SSHHosts = []api.SSHHostInfo{info}
		return response, nil
	case "update.check":
		mgr := s.updateManager()
		info, err := mgr.Check(ctx)
		if err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		response.Update = &info
		return response, nil
	case "update.install":
		mgr := s.updateManager()
		if _, err := mgr.Check(ctx); err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		staged, err := mgr.DownloadAndStage(ctx, nil)
		if err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		info := mgr.CurrentInfo()
		response.Update = &info
		switch {
		case staged.CanAutoRestart:
			response.Output = "更新已下载并完成签名校验，点击「重启并更新」即可安装。"
		case staged.InstallerPath != "":
			response.Output = "更新已下载并完成校验，当前应用未替换，请退出 Vantry 后运行安装命令。"
		default:
			response.Output = "更新包已下载并完成校验，当前应用未替换。"
		}
		return response, nil
	case "update.apply":
		var input struct {
			DesktopPID int `json:"desktopPid"`
		}
		if len(request.Input) > 0 && json.Unmarshal(request.Input, &input) != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: "更新重启参数无效"}
		}
		if s.Service != nil {
			health, err := s.Service.Health(ctx)
			if err != nil {
				return response, &api.Error{Code: api.InvalidCommand, Message: "无法核对 Core 状态，暂无法重启更新：" + err.Error()}
			}
			if health.State != "ready" {
				return response, &api.Error{Code: api.InvalidCommand, Message: "Core 当前状态为 " + health.State + "，暂无法重启更新"}
			}
			snapshot, err := s.Service.Snapshot(ctx)
			if err != nil {
				return response, &api.Error{Code: api.InvalidCommand, Message: "无法核对活动任务状态，暂无法重启更新：" + err.Error()}
			}
			if active := api.ActiveRunCount(snapshot); active > 0 {
				return response, &api.Error{Code: api.InvalidCommand, Message: fmt.Sprintf("Core 仍有 %d 个活动任务，请等待任务结束后再重启更新", active)}
			}
		}
		mgr := s.updateManager()
		if _, err := mgr.LaunchApply(ctx, strings.TrimSpace(request.Name), input.DesktopPID); err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		info := mgr.CurrentInfo()
		response.Update = &info
		response.Output = "正在退出并安装更新…"
		return response, nil
	case "access.status", "access.start", "access.stop", "access.pair", "access.peer.update", "access.remove", "access.rotate", "access.discover", "access.check", "access.configure", "access.code", "access.code.revoke", "access.client.revoke":
		manager, err := s.accessManager()
		if err != nil {
			return response, err
		}
		status := manager.Status()
		if request.Action == "access.status" {
			status = manager.RefreshPairing(ctx)
		}
		switch request.Action {
		case "access.start", "access.configure":
			var input struct {
				Name      string `json:"name"`
				Relay     string `json:"relay"`
				Key       string `json:"key"`
				PublicURL string `json:"publicUrl"`
			}
			if json.Unmarshal(request.Input, &input) != nil {
				return response, errors.New("远程会话设置无效")
			}
			if _, err = manager.SetPublicURL(input.PublicURL); err != nil {
				return response, err
			}
			if request.Action == "access.configure" {
				status, err = manager.Configure(ctx, input.Name, input.Relay, input.Key)
			} else {
				status, err = manager.Start(ctx, input.Name, input.Relay, input.Key)
			}
		case "access.stop":
			status, err = manager.Stop()
		case "access.pair":
			status, err = manager.PairWithContext(ctx, request.Message, request.Name)
		case "access.peer.update":
			var input remoteaccess.PeerUpdate
			if len(request.Input) > 0 && json.Unmarshal(request.Input, &input) != nil {
				return response, errors.New("远程环境配置参数无效")
			}
			if strings.TrimSpace(input.ID) == "" {
				input.ID = strings.TrimSpace(request.Name)
			}
			status, err = manager.UpdatePeer(input)
		case "access.remove":
			status, err = manager.Remove(request.Name)
		case "access.rotate":
			status, err = manager.Rotate(ctx)
		case "access.discover":
			status, err = manager.Discover(ctx)
		case "access.check":
			status, err = manager.Check(ctx, request.Name, request.Message)
		case "access.code":
			status, err = manager.GenerateCode(ctx)
		case "access.code.revoke":
			status, err = manager.RevokeCode(ctx)
		case "access.client.revoke":
			status, err = manager.RevokeClient(request.Name)
		}
		if err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		raw, err := json.Marshal(status)
		response.Output = string(raw)
		return response, err
	case "rd.permissions", "rd.permission.request", "rd.permission.settings":
		return s.remotePermissions(ctx, request)
	case "rd.status", "rd.host_start", "rd.host_stop", "rd.approve", "rd.reject", "rd.code":
		bridge := s.rdBridge()
		var status any
		var err error
		switch request.Action {
		case "rd.host_start":
			relay := strings.TrimSpace(request.Name)
			if relay == "" {
				relay = s.sharedDesktopRelay()
			}
			status, err = bridge.HostStartOwned(ctx, relay, request.Message)
		case "rd.host_stop":
			status, err = bridge.HostStopOwned(ctx, request.Message)
		case "rd.approve", "rd.reject":
			status, err = bridge.HostApprove(ctx, request.Action == "rd.approve")
		case "rd.code":
			status, err = bridge.HostCode(ctx)
		default:
			status, err = bridge.HostStatus(ctx)
		}
		if err != nil {
			return response, err
		}
		raw, err := json.Marshal(status)
		response.Output = string(raw)
		return response, err
	case "plugins.list", "plugins.install", "plugins.update", "plugins.remove", "plugins.grant", "plugins.setting", "plugins.page", "plugins.run":
		wsDir, _ := s.resolveRoot(ctx, request)
		host := plugins.Host{
			Directory:    filepath.Join(s.config.DataDir, "plugins"),
			DataDir:      s.config.DataDir,
			WorkspaceDir: wsDir,
		}
		switch request.Action {
		case "plugins.list":
			var (
				list []api.PluginInfo
				err  error
			)
			if request.Name == "check-updates" || request.Message == "check-updates" {
				list, err = host.CheckUpdates(ctx)
			} else {
				list, err = host.ListContext(ctx)
			}
			response.Plugins = list
			return response, err
		case "plugins.install":
			src := strings.TrimSpace(request.Name)
			if src == "" {
				src = strings.TrimSpace(request.Message)
			}
			var err error
			if _, statErr := os.Stat(src); statErr == nil {
				_, err = host.InstallFromLocal(ctx, src)
			} else {
				_, err = host.InstallFromGitHub(ctx, src)
			}
			if err != nil {
				return response, err
			}
			response.Plugins, err = host.ListContext(ctx)
			return response, err
		case "plugins.update":
			if _, err := host.UpdatePlugin(ctx, request.PluginID); err != nil {
				return response, err
			}
			list, err := host.ListContext(ctx)
			response.Plugins = list
			return response, err
		case "plugins.remove":
			if err := host.RemovePlugin(request.PluginID); err != nil {
				return response, err
			}
			if request.PluginID == "vantry-remote-desktop" {
				if _, err := s.rdBridge().HostStop(ctx); err != nil {
					return response, err
				}
			}
			list, err := host.ListContext(ctx)
			response.Plugins = list
			return response, err
		case "plugins.grant":
			granted := request.Amend || request.Message == "grant"
			if len(request.Input) > 0 {
				var payload struct {
					Granted *bool `json:"granted"`
				}
				if json.Unmarshal(request.Input, &payload) == nil && payload.Granted != nil {
					granted = *payload.Granted
				}
			}
			if err := host.SetGranted(request.PluginID, granted); err != nil {
				return response, err
			}
			list, err := host.ListContext(ctx)
			response.Plugins = list
			return response, err
		case "plugins.setting":
			key, val := request.Name, request.Message
			if len(request.Input) > 0 {
				var payload struct {
					Key   string `json:"key"`
					Value string `json:"value"`
				}
				if json.Unmarshal(request.Input, &payload) == nil && payload.Key != "" {
					key, val = payload.Key, payload.Value
				}
			}
			if err := host.SetSetting(ctx, request.PluginID, key, val); err != nil {
				return response, err
			}
			if request.PluginID == "vantry-remote-desktop" && key == "enabled" && val == "false" {
				if _, err := s.rdBridge().HostStop(ctx); err != nil {
					return response, err
				}
			}
			list, err := host.ListContext(ctx)
			response.Plugins = list
			return response, err
		case "plugins.page":
			output, err := host.CallPage(ctx, request.PluginID, request.Name, request.Input)
			response.Output = output
			return response, err
		default:
			output, err := host.Run(ctx, request.PluginID, request.Input)
			response.Output = output
			return response, err
		}
	case "git.checkpoints.list", "git.checkpoints.capture", "git.checkpoints.diff", "git.checkpoints.restore":
		return s.gitCheckpoints(ctx, request)
	case "git.worktree.status", "git.worktree.merge_cleanup", "git.worktree.cleanup":
		return s.gitWorktree(ctx, request)
	case "workspace", "git.status", "git.log", "git.commit_detail", "git.compare", "git.merge", "git.rebase", "git.stage", "git.unstage", "git.commit", "git.discard", "git.hunk.stage", "git.hunk.unstage", "git.hunk.revert", "git.checkout", "git.branch.create", "git.branch.delete", "git.branch.update", "git.branch.rename", "git.fetch", "git.pull", "git.push", "git.stash", "git.stash.pop", "git.stash_pop", "git.stash.drop", "git.stash_drop", "git.commit_op":
		var (
			path string
			err  error
		)
		if request.Action == "workspace" && request.ThreadID != "" {
			path, err = s.directory(ctx, request.ThreadID)
		} else {
			path, err = s.resolveRoot(ctx, request)
		}
		if err != nil {
			return response, err
		}
		response.Workspace = path
		if request.Action == "workspace" {
			return response, nil
		}
		if request.Action == "git.log" {
			filter := gitwork.LogFilter{Query: request.Message, Branch: request.Branch, Limit: 200}
			if len(request.Input) > 0 {
				if err := json.Unmarshal(request.Input, &filter); err != nil {
					return response, errors.New("日志筛选条件无效")
				}
				if filter.Query == "" {
					filter.Query = request.Message
				}
				if filter.Branch == "" {
					filter.Branch = request.Branch
				}
			}
			commits, err := gitwork.LogFiltered(ctx, path, filter)
			if err != nil {
				return response, err
			}
			response.Git = &api.GitState{Commits: commits}
			return response, nil
		}
		if request.Action == "git.commit_detail" {
			detail, err := gitwork.InspectCommit(ctx, path, request.Name)
			if err != nil {
				return response, err
			}
			raw, err := json.Marshal(detail)
			response.Output = string(raw)
			return response, err
		}
		if request.Action == "git.compare" {
			cmp, err := gitwork.CompareBranches(ctx, path, request.Branch, request.Name)
			if err != nil {
				return response, err
			}
			raw, err := json.Marshal(cmp)
			response.Output = string(raw)
			return response, err
		}
		if request.Action == "git.commit_op" && request.Message == "patch" {
			response.Output, err = gitwork.CommitOperation(ctx, path, request.Name, request.Message, request.Input)
			return response, err
		}
		if request.Action != "git.status" {
			if request.Action != "git.fetch" && request.ThreadID != "" {
				snapshot, err := s.Snapshot(ctx)
				if err != nil {
					return response, err
				}
				for _, run := range snapshot.Runs {
					if run.ThreadID == request.ThreadID && !run.Status.Terminal() {
						return response, errors.New("请等待会话任务结束后再修改 Git")
					}
				}
			}
			var output string
			switch request.Action {
			case "git.merge":
				output, err = gitwork.Merge(ctx, path, request.Branch)
			case "git.rebase":
				target := strings.TrimSpace(request.Name)
				if target == "" {
					target = strings.TrimSpace(request.Branch)
				}
				output, err = gitwork.RebaseBranch(ctx, path, target)
			case "git.checkout":
				target := strings.TrimSpace(request.Branch)
				if target == "" {
					target = strings.TrimSpace(request.Name)
				}
				output, err = gitwork.CheckoutBranch(ctx, path, target, false, "")
			case "git.branch.create":
				output, err = gitwork.CheckoutBranch(ctx, path, request.Name, true, request.Branch)
			case "git.branch.update":
				target := strings.TrimSpace(request.Name)
				if target == "" {
					target = strings.TrimSpace(request.Branch)
				}
				output, err = gitwork.UpdateBranch(ctx, path, target)
			case "git.branch.rename":
				output, err = gitwork.RenameBranch(ctx, path, request.Branch, request.Name)
			case "git.branch.delete":
				target := strings.TrimSpace(request.Branch)
				if target == "" {
					target = strings.TrimSpace(request.Name)
				}
				output, err = gitwork.DeleteBranch(ctx, path, target, request.Amend)
			case "git.fetch":
				output, err = gitwork.Fetch(ctx, path, "")
			case "git.pull":
				output, err = gitwork.PullFFOnly(ctx, path, "", request.Branch)
			case "git.push":
				output, err = gitwork.Push(ctx, path, "", request.Branch, false)
			case "git.stash":
				output, err = gitwork.StashPush(ctx, path, request.Message, true)
			case "git.stash.pop", "git.stash_pop":
				output, err = gitwork.StashPop(ctx, path, request.Name)
			case "git.stash.drop", "git.stash_drop":
				output, err = gitwork.StashDrop(ctx, path, request.Name)
			case "git.commit_op":
				output, err = gitwork.CommitOperation(ctx, path, request.Name, request.Message, request.Input)
			case "git.hunk.stage", "git.hunk.unstage", "git.hunk.revert":
				var req gitwork.HunkRequest
				if len(request.Input) == 0 || json.Unmarshal(request.Input, &req) != nil {
					return response, &api.Error{Code: api.InvalidCommand, Message: "块级差异参数无效"}
				}
				if req.Branch == "" {
					req.Branch = request.Branch
				}
				if req.FilePath == "" && len(request.Paths) > 0 {
					req.FilePath = request.Paths[0]
				}
				req.Action = strings.TrimPrefix(request.Action, "git.hunk.")
				output, err = gitwork.ApplyHunk(ctx, path, req)
			default:
				output, err = gitwork.Change(ctx, path, request)
			}
			response.Output = output
			if err != nil {
				message := err.Error()
				if output != "" && !strings.Contains(message, output) {
					message += "\n" + output
				}
				const limit = 16 << 10
				if len(message) > limit {
					message = message[:limit]
					for !utf8.ValidString(message) {
						message = message[:len(message)-1]
					}
					message += "\n[Git 诊断已截断，请在终端检查完整状态。]"
				}
				return response, errors.New(message)
			}
		}
		state, err := gitwork.Inspect(ctx, path)
		if err == nil {
			response.Git = &state
		}
		return response, err
	default:
		return response, &api.Error{Code: api.InvalidCommand, Message: "未知工作台操作"}
	}
}

func (s *workbenchService) threadWorkspace(ctx context.Context, threadID string) (*api.WorkspaceSpec, error) {
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	for _, thread := range snapshot.Threads {
		if thread.ID == threadID {
			return thread.Workspace, nil
		}
	}
	return nil, &api.Error{Code: api.NotFound, Message: "请先发送消息创建会话"}
}

func (s *workbenchService) directory(ctx context.Context, threadID string) (string, error) {
	spec, err := s.threadWorkspace(ctx, threadID)
	if err != nil {
		return "", err
	}
	return workspace.ThreadDirectory(ctx, s.config.DataDir, spec, threadID)
}

func (s *workbenchService) resolveRoot(ctx context.Context, request api.WorkbenchRequest) (string, error) {
	if request.ThreadID != "" {
		dir, err := s.directory(ctx, request.ThreadID)
		if err == nil {
			return dir, nil
		}
		if api.CodeOf(err) != api.NotFound || strings.TrimSpace(request.Workspace) == "" && (s.config.Workspace == nil || s.config.Workspace.Path == "") {
			return "", err
		}
	}
	if strings.TrimSpace(request.Workspace) != "" {
		return filepath.Abs(request.Workspace)
	}
	if s.config.Workspace != nil && s.config.Workspace.Path != "" {
		return filepath.Abs(s.config.Workspace.Path)
	}
	return "", &api.Error{Code: api.NotFound, Message: "尚未指定工作区目录"}
}

func (s *workbenchService) rdBridge() *RemoteDesktopBridge {
	s.rdMu.Lock()
	defer s.rdMu.Unlock()
	if s.rd == nil {
		s.rd = NewRemoteDesktopDriver(s.root, RemoteDesktopOptions{DataDir: s.config.DataDir})
	}
	return s.rd
}

func (s *workbenchService) updateManager() *updater.Manager {
	if s.upd == nil {
		opts := updater.Options{CurrentVersion: updater.CurrentVersion, DataDir: s.config.DataDir}
		if exe, err := os.Executable(); err == nil {
			if bundle, ok := updater.ResolveCurrentAppBundle(exe); ok {
				opts.TargetAppPath = bundle
			}
		}
		s.upd = updater.NewManager(opts)
	}
	return s.upd
}

// probeSSHHost asks a machine whether it is reachable: an alias of the SSH
// configuration, or the machine of a manual entry, which the add-host form
// tests before saving it. It never prompts and never returns credentials.
func probeSSHHost(ctx context.Context, request api.WorkbenchRequest) (api.SSHHostInfo, error) {
	var manual struct {
		Host     string `json:"host"`
		User     string `json:"user"`
		Port     int    `json:"port"`
		Identity string `json:"identity"`
	}
	if len(request.Input) > 0 {
		_ = json.Unmarshal(request.Input, &manual)
	}
	opts := sshconfig.ProbeOptions{
		Alias:        strings.TrimSpace(request.Name),
		Host:         strings.TrimSpace(manual.Host),
		User:         strings.TrimSpace(manual.User),
		Port:         manual.Port,
		IdentityFile: strings.TrimSpace(manual.Identity),
	}
	return sshconfig.ProbeWith(ctx, opts)
}

func (s *workbenchService) usageSummary(ctx context.Context) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	snapshot, err := s.Snapshot(ctx)
	if err != nil {
		return response, err
	}
	type modelStat struct {
		Model           string  `json:"model"`
		Runs            int     `json:"runs"`
		Completed       int     `json:"completed"`
		Failed          int     `json:"failed"`
		InputChars      int     `json:"inputChars"`
		OutputChars     int     `json:"outputChars"`
		InputTokens     int64   `json:"inputTokens,omitempty"`
		OutputTokens    int64   `json:"outputTokens,omitempty"`
		CacheReadTokens int64   `json:"cacheReadTokens,omitempty"`
		TotalTokens     int64   `json:"totalTokens,omitempty"`
		CacheHitRate    float64 `json:"cacheHitRate,omitempty"`
		CostUSD         float64 `json:"costUsd,omitempty"`
		AvgTTFTMs       int64   `json:"avgTtftMs,omitempty"`
	}
	byModel := map[string]*modelStat{}
	ensureModel := func(m string) *modelStat {
		if m == "" {
			m = s.config.Model
		}
		if m == "" {
			m = "pi/default"
		}
		st := byModel[m]
		if st == nil {
			st = &modelStat{Model: m}
			byModel[m] = st
		}
		return st
	}
	activeRuns, completedRuns, failedRuns := 0, 0, 0
	var (
		totalInput, totalOutput, totalCacheRead, totalCacheWrite, totalTokens int64
		totalCostUSD                                                          float64
		ttftSum, ttftCount                                                    int64
	)
	ttftByModel := map[string][2]int64{}
	for _, r := range snapshot.Runs {
		st := ensureModel(r.Model)
		st.Runs++
		st.InputChars += len(r.Input)
		st.OutputChars += len(r.Output)
		switch {
		case !r.Status.Terminal():
			activeRuns++
		case r.Status == api.RunCompleted:
			completedRuns++
			st.Completed++
		case r.Status == api.RunFailed:
			failedRuns++
			st.Failed++
		}
		if r.Telemetry != nil {
			inLedger := false
			if s.ledger != nil {
				_, inLedger = s.ledger.GetRun(r.ID)
			}
			if !inLedger {
				st.InputTokens += r.Telemetry.InputTokens
				st.OutputTokens += r.Telemetry.OutputTokens
				st.CacheReadTokens += r.Telemetry.CacheReadTokens
				st.TotalTokens += r.Telemetry.TotalTokens
				st.CostUSD += r.Telemetry.CostUSD
				totalInput += r.Telemetry.InputTokens
				totalOutput += r.Telemetry.OutputTokens
				totalCacheRead += r.Telemetry.CacheReadTokens
				totalCacheWrite += r.Telemetry.CacheWriteTokens
				totalTokens += r.Telemetry.TotalTokens
				totalCostUSD += r.Telemetry.CostUSD
				if r.Telemetry.TTFTMs > 0 {
					acc := ttftByModel[st.Model]
					acc[0] += r.Telemetry.TTFTMs
					acc[1]++
					ttftByModel[st.Model] = acc
					ttftSum += r.Telemetry.TTFTMs
					ttftCount++
				}
			}
		}
	}
	if s.ledger != nil {
		totals := s.ledger.GlobalTotals()
		totalInput += totals.InputTokens
		totalOutput += totals.OutputTokens
		totalCacheRead += totals.CacheReadTokens
		totalCacheWrite += totals.CacheWriteTokens
		totalTokens += totals.TotalTokens
		totalCostUSD += totals.TotalCostUSD
		if totals.AvgTTFTMs > 0 {
			n := int64(max(1, totals.AttemptCount))
			ttftSum += totals.AvgTTFTMs * n
			ttftCount += n
		}
		for _, bm := range s.ledger.ModelBreakdown(time.Time{}) {
			key := bm.Model
			if key == "" {
				key = bm.ProviderID
			} else if bm.ProviderID != "" && bm.ProviderID != "pi" && !strings.Contains(key, "/") && byModel[key] == nil {
				key = bm.ProviderID + "/" + key
			}
			st := ensureModel(key)
			st.InputTokens += bm.InputTokens
			st.OutputTokens += bm.OutputTokens
			st.CacheReadTokens += bm.CacheReadTokens
			st.TotalTokens += bm.TotalTokens
			st.CostUSD += bm.TotalCostUSD
			if st.CacheHitRate == 0 && bm.CacheHitRate > 0 {
				st.CacheHitRate = bm.CacheHitRate
			}
			if bm.AvgTTFTMs > 0 {
				acc := ttftByModel[st.Model]
				n := int64(max(1, bm.AttemptCount))
				acc[0] += bm.AvgTTFTMs * n
				acc[1] += n
				ttftByModel[st.Model] = acc
			}
		}
	}
	models := make([]modelStat, 0, len(byModel))
	for _, st := range byModel {
		if acc := ttftByModel[st.Model]; acc[1] > 0 {
			st.AvgTTFTMs = acc[0] / acc[1]
		}
		if st.CacheHitRate == 0 && st.CacheReadTokens > 0 {
			if denom := st.InputTokens + st.CacheReadTokens; denom > 0 {
				st.CacheHitRate = min(1.0, float64(st.CacheReadTokens)/float64(denom))
			}
		}
		models = append(models, *st)
	}
	var cacheHitRate float64
	if promptTotal := totalInput + totalCacheRead + totalCacheWrite; promptTotal > 0 && totalCacheRead > 0 {
		cacheHitRate = min(1.0, float64(totalCacheRead)/float64(promptTotal))
	}
	var avgTTFTMs int64
	if ttftCount > 0 {
		avgTTFTMs = ttftSum / ttftCount
	}
	payload := map[string]any{
		"threads": len(snapshot.Threads), "runs": len(snapshot.Runs),
		"activeRuns": activeRuns, "completedRuns": completedRuns, "failedRuns": failedRuns,
		"tasks": len(snapshot.Tasks), "models": models,
	}
	if totalTokens > 0 || totalCostUSD > 0 || avgTTFTMs > 0 {
		payload["inputTokens"] = totalInput
		payload["outputTokens"] = totalOutput
		payload["cacheReadTokens"] = totalCacheRead
		payload["cacheWriteTokens"] = totalCacheWrite
		payload["totalTokens"] = totalTokens
		payload["cacheHitRate"] = cacheHitRate
		payload["totalCostUsd"] = totalCostUSD
		payload["avgTtftMs"] = avgTTFTMs
	}
	raw, err := json.Marshal(payload)
	response.Output = string(raw)
	return response, err
}

func (s *workbenchService) gitCheckpoints(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	path, err := s.resolveRoot(ctx, request)
	if err != nil {
		return response, err
	}
	response.Workspace = path
	switch request.Action {
	case "git.checkpoints.list":
		if !gitwork.IsGitWorkspace(path) {
			response.Output = `{"checkpoints":[]}`
			return response, nil
		}
		list, err := gitwork.ListCheckpoints(ctx, path)
		if err != nil {
			return response, err
		}
		raw, err := json.Marshal(map[string]any{"checkpoints": list})
		response.Output = string(raw)
		return response, err
	case "git.checkpoints.capture":
		cp, err := gitwork.CaptureCheckpoint(ctx, path, request.Name, request.Message)
		if err != nil {
			return response, err
		}
		raw, err := json.Marshal(map[string]any{"checkpoint": cp})
		response.Output = string(raw)
		return response, err
	case "git.checkpoints.diff":
		diff, err := gitwork.DiffCheckpoint(ctx, path, request.Name)
		if err != nil {
			return response, err
		}
		response.Output = diff
		return response, nil
	case "git.checkpoints.restore":
		if request.ThreadID != "" && s.Service != nil {
			snapshot, err := s.Snapshot(ctx)
			if err != nil {
				return response, err
			}
			for _, run := range snapshot.Runs {
				if run.ThreadID == request.ThreadID && !run.Status.Terminal() {
					return response, errors.New("请等待会话任务结束后再恢复工作区快照")
				}
			}
		}
		opts := gitwork.CheckpointRestoreOptions{AllowPrimaryCheckout: request.Amend}
		if len(request.Input) > 0 {
			var body struct {
				AllowPrimaryCheckout *bool `json:"allowPrimaryCheckout"`
			}
			if err := json.Unmarshal(request.Input, &body); err != nil {
				return response, &api.Error{Code: api.InvalidCommand, Message: "checkpoint 恢复参数无效"}
			}
			if body.AllowPrimaryCheckout != nil {
				opts.AllowPrimaryCheckout = *body.AllowPrimaryCheckout
			}
		}
		res, err := gitwork.RestoreCheckpoint(ctx, path, request.Name, opts)
		if err != nil {
			return response, err
		}
		raw, err := json.Marshal(map[string]any{"backup": res.Backup})
		if err != nil {
			return response, err
		}
		response.Output = string(raw)
		if state, inspectErr := gitwork.Inspect(ctx, path); inspectErr == nil {
			response.Git = &state
		}
		return response, nil
	default:
		return response, &api.Error{Code: api.InvalidCommand, Message: "未知检查点操作"}
	}
}

func (s *workbenchService) gitWorktree(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	var spec *api.WorkspaceSpec
	if request.ThreadID != "" {
		spec, _ = s.threadWorkspace(ctx, request.ThreadID)
	}
	path, resolveErr := s.resolveRoot(ctx, request)
	if resolveErr != nil && spec == nil {
		return response, resolveErr
	}
	response.Workspace = path
	if request.Action == "git.worktree.status" {
		status, err := gitwork.InspectWorktree(ctx, s.config.DataDir, request.ThreadID, path, spec)
		if err != nil {
			return response, err
		}
		raw, err := json.Marshal(status)
		response.Output = string(raw)
		return response, err
	}
	if request.ThreadID != "" && s.Service != nil {
		snapshot, err := s.Snapshot(ctx)
		if err != nil {
			return response, err
		}
		for _, run := range snapshot.Runs {
			if run.ThreadID == request.ThreadID && !run.Status.Terminal() {
				return response, errors.New("请等待会话任务结束后再操作 Worktree")
			}
		}
	}
	switch request.Action {
	case "git.worktree.merge_cleanup":
		res, err := gitwork.MergeAndCleanupWorktree(ctx, s.config.DataDir, request.ThreadID, path, spec)
		if len(res.Output) > 0 || res.MergedBranch != "" {
			if raw, mErr := json.Marshal(res); mErr == nil {
				response.Output = string(raw)
			}
		}
		if err != nil {
			return response, err
		}
		if res.Repository != "" {
			if state, inspectErr := gitwork.Inspect(ctx, res.Repository); inspectErr == nil {
				response.Git = &state
			}
		}
		return response, nil
	case "git.worktree.cleanup":
		out, err := gitwork.CleanupWorktreeOnly(ctx, s.config.DataDir, request.ThreadID, path, spec, request.Amend)
		response.Output = out
		return response, err
	default:
		return response, &api.Error{Code: api.InvalidCommand, Message: "未知 Worktree 操作"}
	}
}

func (s *workbenchService) sandboxAction(request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	supported := hostprocess.SandboxSupported()
	if request.Action == "sandbox.set" {
		rawMode := strings.TrimSpace(request.Message)
		if len(request.Input) > 0 {
			var body struct {
				Mode string `json:"mode"`
			}
			if err := json.Unmarshal(request.Input, &body); err != nil {
				return response, &api.Error{Code: api.InvalidCommand, Message: "沙箱模式参数无效"}
			}
			if strings.TrimSpace(body.Mode) != "" {
				rawMode = body.Mode
			}
		}
		mode, err := hostprocess.NormalizeSandboxMode(rawMode)
		if err != nil {
			return response, &api.Error{Code: api.InvalidCommand, Message: err.Error()}
		}
		if mode == hostprocess.SandboxWorkspaceWrite && !supported {
			return response, &api.Error{Code: api.InvalidCommand, Message: "当前系统不支持 macOS Seatbelt 工作区沙箱"}
		}
		if err := piprovider.WriteSandboxMode(s.config.DataDir, mode); err != nil {
			return response, err
		}
	}
	mode, envOverride := piprovider.ReadSandboxMode(s.config.DataDir)
	raw, err := json.Marshal(map[string]any{
		"mode":        mode,
		"supported":   supported,
		"envOverride": envOverride,
	})
	response.Output = string(raw)
	return response, err
}
