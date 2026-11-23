package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/egoist/mygo"
	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/internal/adapters/transport/local"
	"github.com/muq9738/Vantry/internal/adapters/transport/sshrelay"
	"github.com/muq9738/Vantry/internal/app"
	"github.com/muq9738/Vantry/internal/desktop"
	"github.com/muq9738/Vantry/internal/relaycmd"
	"github.com/muq9738/Vantry/internal/remoteaccess"
	"github.com/muq9738/Vantry/internal/updater"
	"github.com/muq9738/Vantry/pkg/api"
	remotedesktop "github.com/muq9738/vantry-plugin-remote-desktop"
)

func init() { runtime.LockOSThread() }

type desktopStartupError struct{ error }

func (e *desktopStartupError) Unwrap() error { return e.error }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "vantry:", err)
		var startupErr *desktopStartupError
		if errors.As(err, &startupErr) && os.Getenv("MYGO_AUTOMATION") == "" {
			mygo.App.WhenReady(func() {
				mygo.Dialog.Error("Vantry 无法启动", startupErr.Error())
				mygo.App.Quit()
			})
			if dialogErr := mygo.App.Run(); dialogErr != nil {
				fmt.Fprintln(os.Stderr, "vantry: 显示启动错误：", dialogErr)
			}
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, output, diagnostics io.Writer) error {
	mode := "desktop"
	if len(args) > 0 {
		mode, args = args[0], args[1:]
	}
	if mode == "help" || mode == "--help" || mode == "-h" {
		_, err := fmt.Fprint(output, usage)
		return err
	}
	if mode == "relay" {
		return relaycmd.Run(ctx, args, diagnostics)
	}
	if mode == "apply-update" {
		return updater.RunApply(ctx, args, output)
	}
	if mode == "rd-relay" {
		relayFlags := flag.NewFlagSet(mode, flag.ContinueOnError)
		relayFlags.SetOutput(diagnostics)
		listenAddr := relayFlags.String("listen", ":8788", "远程桌面中继 HTTP/MJPEG 监听地址（例如 :8788）")
		if err := relayFlags.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return err
		}
		if relayFlags.NArg() != 0 {
			return errors.New("rd-relay 不接受额外参数")
		}
		return runRemoteDesktopRelay(ctx, *listenAddr, diagnostics)
	}
	if mode == "provider-worker" {
		workerFlags := flag.NewFlagSet(mode, flag.ContinueOnError)
		workerFlags.SetOutput(diagnostics)
		jobDir := workerFlags.String("job-dir", "", "持久执行目录")
		if err := workerFlags.Parse(args); err != nil {
			return err
		}
		if *jobDir == "" || workerFlags.NArg() != 0 {
			return errors.New("provider-worker 需要 --job-dir")
		}
		return piprovider.RunWorker(ctx, *jobDir)
	}
	defaultDir, err := app.DefaultDataDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet(mode, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	dataDir := flags.String("data-dir", defaultDir, "Core 私有数据目录（权限 0700）")
	delay := flags.Duration("controlled-delay", app.DefaultDelay, "受控 Provider 每段输出的间隔")
	remoteHost := flags.String("remote", "", "SSH 远程主机")
	peerID := flags.String("peer", "", "通过会话配对码添加的主机 ID")
	peerMode := flags.String("peer-mode", "auto", "远程连接方式：auto、direct 或 relay")
	remoteSocket := flags.String("remote-socket", "", "远端 Core socket 绝对路径")
	attachOnly := flags.Bool("attach-only", false, "仅连接现有 Core，不自动启动（desktop）")
	providerID := flags.String("provider", envDefault("VANTRY_PROVIDER", "pi"), "执行器：pi 或 controlled")
	model := flags.String("model", envDefault("VANTRY_MODEL", piprovider.ConfiguredModel()), "Pi 模型 provider/model")
	piExecutable := flags.String("pi-executable", envDefault("VANTRY_PI_EXECUTABLE", "pi"), "Pi CLI 可执行文件")
	magpieSettings := flags.String("magpie-settings", "", "可选：限制为指定 Magpie settings.json 中的 Pi 已选模型；默认使用 Pi 模型目录")
	workspaceFlags := addWorkspaceFlags(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *delay < 0 {
		return errors.New("controlled-delay 不能为负数")
	}
	absoluteDir, err := filepath.Abs(*dataDir)
	if err != nil {
		return err
	}
	if *providerID != "pi" && *providerID != "controlled" {
		return errors.New("provider 只能为 pi 或 controlled")
	}
	workspace, err := workspaceFlags.spec()
	if err != nil {
		return err
	}
	config := app.Config{DataDir: absoluteDir, Delay: *delay, ProviderID: *providerID,
		Model: *model, PiExecutable: *piExecutable, Workspace: workspace, MagpieSettingsPath: *magpieSettings}
	switch mode {
	case "core":
		if flags.NArg() != 0 {
			return errors.New("core 不接受额外参数")
		}
		return app.RunCore(ctx, config)
	case "desktop":
		viewerFactory := func(ctx context.Context) desktop.RemoteDesktopDriver {
			return app.NewRemoteDesktopDriver(ctx, app.RemoteDesktopOptions{DataDir: config.DataDir})
		}
		if flags.NArg() != 0 {
			return errors.New("desktop 不接受额外参数")
		}
		if *peerID != "" {
			client, err := remoteaccess.ConnectWithMode(ctx, remoteaccess.Options{DataDir: config.DataDir}, *peerID, *peerMode)
			if err != nil {
				return err
			}
			defer client.Close()
			required := app.RequiredRemoteDesktopCapabilities()
			if client.Protocol == "t3" {
				required = app.RequiredCoreCapabilities()
			}
			if _, err := app.VerifyCoreCapabilities(ctx, client, required); err != nil {
				return err
			}
			return desktop.RunWithOptions(ctx, client, desktop.Options{ProviderID: "pi", RemoteName: client.Name, RemotePeer: true, ConnectionMode: client.Mode, Version: updater.CurrentVersion, RemoteDesktopFactory: viewerFactory})
		}
		if *remoteHost != "" {
			relay, err := sshrelay.Connect(ctx, *remoteHost, *remoteSocket)
			if err != nil {
				return err
			}
			defer relay.Close()
			if _, err := app.VerifyCoreCapabilities(ctx, relay, app.RequiredRemoteDesktopCapabilities()); err != nil {
				return fmt.Errorf("远端 Vantry 需要更新并重启：%w", err)
			}
			return desktop.RunWithOptions(ctx, relay, desktop.Options{
				ProviderID: "pi", Workspace: workspace, RemoteHost: *remoteHost, RemotePeer: true,
				Version: updater.CurrentVersion, RemoteDesktopFactory: viewerFactory,
			})
		}
		if config.DataDir == defaultDir && os.Getenv("MYGO_AUTOMATION") == "" {
			mygo.App.SetName("Vantry")
			if !mygo.App.RequestSingleInstanceLock() {
				return nil
			}
		}
		client, err := app.EnsureCore(ctx, config, *attachOnly)
		if err != nil {
			return &desktopStartupError{err}
		}
		defer client.Close()
		return desktop.RunWithOptions(ctx, client, desktop.Options{
			ProviderID: config.ProviderID, Model: config.Model, Workspace: config.Workspace,
			StatePath: filepath.Join(config.DataDir, "desktop-state.json"), Version: updater.CurrentVersion, RemoteDesktopFactory: viewerFactory,
			ConnectEnvironment: func(ctx context.Context, id string) (*desktop.EnvironmentSession, error) {
				return app.ConnectEnvironment(ctx, config.DataDir, id)
			},
		})
	case "ctl":
		client := local.Dial(config.SocketPath())
		defer client.Close()
		return control(ctx, client, flags.Args(), output, diagnostics)
	default:
		return fmt.Errorf("未知模式 %q；运行 vantry --help 查看用法", mode)
	}
}

func control(ctx context.Context, service api.Service, args []string, output, diagnostics io.Writer) error {
	if len(args) == 0 {
		return errors.New("ctl 需要 health、status、create、send、steer、stop、delete、fork、merge-back、snooze、unsnooze、cancel-task、threads、thread-read、thread-send、thread-wait、delegate、followup、ack、dispose、queue 或 watch")
	}
	action := args[0]
	flags := flag.NewFlagSet("ctl "+action, flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	commandID := flags.String("command-id", "", "命令幂等 ID（可重试同一命令）")
	threadID := flags.String("thread", "", "线程 ID")
	runID := flags.String("run", "", "运行 ID")
	title := flags.String("title", "", "线程或子任务标题")
	message := flags.String("message", "", "发送内容")
	after := flags.Int64("after", 0, "从该事件序列之后订阅")
	providerID := flags.String("provider", "", "执行器（新线程默认 pi，子任务默认继承）")
	model := flags.String("model", "", "Pi 模型 provider/model")
	parentRun := flags.String("parent-run", "", "父 Run ID")
	parentAttempt := flags.String("parent-attempt", "", "预期父 Attempt ID，防止过期派发")
	taskID := flags.String("task", "", "子任务 ID")
	generation := flags.Int64("generation", 0, "交付 generation")
	deliveryID := flags.String("delivery", "", "交付消息 ID")
	resumePolicy := flags.String("resume-policy", "", "父任务续行：auto 或 manual")
	messageID := flags.String("message-id", "", "排队消息 ID")
	file := flags.String("file", "", "附件文件路径")
	pluginID := flags.String("plugin", "", "命令插件 ID")
	approvalPolicy := flags.String("approval", "", "工具执行策略：auto、auto-accept-edits 或 ask")
	targetThread := flags.String("target", "", "merge-back 目标线程 ID")
	senderThread := flags.String("from", "", "跨线程发送的发起线程 ID")
	deliveryMode := flags.String("delivery-mode", "", "跨线程发送方式：auto、queue 或 steer")
	reason := flags.String("reason", "", "取消任务原因")
	duration := flags.Duration("duration", 0, "snooze 时长，如 1h 或 90m")
	until := flags.String("until", "", "snooze 绝对时间（RFC3339）")
	limit := flags.Int("limit", 0, "列表或读取的返回数量上限")
	settled := flags.Bool("settled", false, "按已归档状态过滤线程：--settled 仅已归档，--settled=false 仅活动线程")
	view := flags.String("view", "", "thread-read 视图：messages 或 activity")
	afterPosition := flags.Int("after-pos", 0, "thread-read 从该时间线位置之后开始")
	waitTimeout := flags.Duration("timeout", 60*time.Second, "thread-wait 等待上限")
	force := flags.Bool("force", true, "delete 强制终止活动运行与子任务；--force=false 时要求线程已静止")
	inputID := flags.String("input", "", "排队输入 ID")
	attemptID := flags.String("attempt", "", "运行中的 Attempt ID；省略时使用兼容的当前 Run 调整")
	requestID := flags.String("request", "", "Pi 对话框请求 ID")
	responseValue := flags.String("value", "", "对话框回复内容")
	confirmed := flags.Bool("confirm", false, "确认或拒绝对话框")
	cancelled := flags.Bool("cancel", false, "取消对话框")
	workspaceFlags := addWorkspaceFlags(flags)
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("存在未识别的参数；消息请通过 --message 传入")
	}
	var workspace *api.WorkspaceSpec
	if action != "merge" {
		var err error
		workspace, err = workspaceFlags.spec()
		if err != nil {
			return err
		}
	}
	if *providerID != "" && *providerID != "pi" && *providerID != "controlled" {
		return errors.New("provider 只能为 pi 或 controlled")
	}
	if *resumePolicy != "" && *resumePolicy != "auto" && *resumePolicy != "manual" {
		return errors.New("resume-policy 只能为 auto 或 manual")
	}
	switch *approvalPolicy {
	case "", "auto", "auto-accept-edits", "ask":
	default:
		return errors.New("approval 只能为 auto、auto-accept-edits 或 ask")
	}
	if *deliveryMode != "" && *deliveryMode != "auto" && *deliveryMode != "queue" && *deliveryMode != "steer" {
		return errors.New("delivery-mode 只能为 auto、queue 或 steer")
	}
	if *view != "" && *view != "messages" && *view != "activity" {
		return errors.New("view 只能为 messages 或 activity")
	}
	if *limit < 0 {
		return errors.New("limit 不能为负数")
	}
	if *waitTimeout <= 0 {
		return errors.New("timeout 必须为正数")
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch action {
	case "diff", "terminal":
		queries, ok := service.(api.WorkbenchQueries)
		if !ok {
			return errors.New("服务不支持工作区查询")
		}
		if *threadID == "" {
			return errors.New("需要 --thread")
		}
		if action == "diff" {
			changes, err := queries.InspectWorkspace(requestCtx, *threadID)
			if err != nil {
				return err
			}
			return encoder.Encode(changes)
		}
		if err := queries.OpenTerminal(requestCtx, *threadID); err != nil {
			return err
		}
		return encoder.Encode(map[string]bool{"opened": true})
	case "models", "plugins", "plugin-run", "git", "merge", "workspace", "upload":
		workbench, ok := service.(api.WorkbenchService)
		if !ok {
			return errors.New("服务不支持工作台操作")
		}
		request := api.WorkbenchRequest{ThreadID: *threadID, Model: *model, PluginID: *pluginID, Branch: *workspaceFlags.branch}
		if workspace != nil {
			request.Workspace = workspace.Path
		}
		switch action {
		case "models":
			request.Action = "models"
		case "plugins":
			request.Action = "plugins.list"
		case "plugin-run":
			request.Action = "plugins.run"
			request.Input, _ = json.Marshal(map[string]string{"message": *message, "threadId": *threadID})
		case "git":
			request.Action = "git.status"
		case "merge":
			request.Action = "git.merge"
		case "workspace":
			request.Action = "workspace"
		case "upload":
			input, err := os.Open(*file)
			if err != nil {
				return err
			}
			defer input.Close()
			request.Data, err = io.ReadAll(io.LimitReader(input, (8<<20)+1))
			if err != nil {
				return err
			}
			request.Name = filepath.Base(*file)
			request.Action = "attachment.import"
		}
		response, err := workbench.Workbench(requestCtx, request)
		if err != nil {
			return err
		}
		return encoder.Encode(response)
	case "health":
		health, err := service.Health(requestCtx)
		if err != nil {
			return err
		}
		return encoder.Encode(health)
	case "status":
		snapshot, err := service.Snapshot(requestCtx)
		if err != nil {
			return err
		}
		return encoder.Encode(snapshot)
	case "watch":
		if *after < 0 {
			return errors.New("after 不能为负数")
		}
		return watch(ctx, service, *after, encoder)
	case "threads":
		result, err := listThreads(requestCtx, service, api.ThreadListQuery{
			Limit: *limit, Settled: providedBool(flags, "settled", *settled),
		})
		if err != nil {
			return err
		}
		return encoder.Encode(result)
	case "thread-read":
		if *threadID == "" {
			return errors.New("thread-read 需要 --thread")
		}
		result, err := readThread(requestCtx, service, api.ThreadReadQuery{
			ThreadID: *threadID, View: *view, Limit: *limit,
			AfterPosition: providedInt(flags, "after-pos", *afterPosition),
		})
		if err != nil {
			return err
		}
		return encoder.Encode(result)
	case "thread-send":
		if *threadID == "" || strings.TrimSpace(*message) == "" {
			return errors.New("thread-send 需要 --thread（目标线程）和非空 --message")
		}
		payload, err := sendToThread(requestCtx, service, api.InterThreadSendRequest{
			ClientRequestID: commandIdentity(*commandID), CallerThreadID: *senderThread,
			TargetThreadID: *threadID, Message: *message, Mode: *deliveryMode,
		})
		if err != nil {
			return err
		}
		return encoder.Encode(payload)
	case "thread-wait":
		if *threadID == "" {
			return errors.New("thread-wait 需要 --thread")
		}
		// The wait reports its own timeout instead of failing, so allow the
		// request to outlive it and answer with timedOut: true first.
		waitCtx, waitCancel := context.WithTimeout(ctx, *waitTimeout+30*time.Second)
		defer waitCancel()
		result, err := waitThread(waitCtx, service, api.ThreadWaitQuery{
			ThreadID: *threadID, RunID: *runID, TimeoutMs: int(*waitTimeout / time.Millisecond),
		})
		if err != nil {
			return err
		}
		return encoder.Encode(result)
	case "create", "send", "stop", "delete", "fork", "merge-back", "snooze", "unsnooze", "cancel-task", "delegate", "followup", "ack", "dispose", "rename", "pin", "unpin", "archive", "restore", "model", "queue", "cancel-message", "steer", "cancel-input", "respond":
		command := api.Command{Version: api.ProtocolVersion, CommandID: *commandID, ThreadID: *threadID,
			ProviderID: *providerID, Model: *model, Workspace: workspace,
			ParentRunID: *parentRun, ParentAttemptID: *parentAttempt,
			TaskID: *taskID, Generation: *generation, DeliveryMessageID: *deliveryID,
			ResumePolicy: *resumePolicy, ApprovalPolicy: *approvalPolicy}
		command.CommandID = commandIdentity(command.CommandID)
		switch action {
		case "delete":
			if command.ThreadID == "" {
				return errors.New("delete 需要 --thread")
			}
			command.Kind, command.Force = api.DeleteThread, *force
		case "cancel-task":
			if *taskID == "" {
				return errors.New("cancel-task 需要 --task")
			}
			command.Kind, command.Reason = api.CancelTask, *reason
		case "snooze":
			if command.ThreadID == "" {
				return errors.New("snooze 需要 --thread")
			}
			deadline, err := snoozeDeadline(*until, *duration, time.Now())
			if err != nil {
				return err
			}
			command.Kind, command.SnoozedUntil = api.UpdateThread, &deadline
		case "unsnooze":
			if command.ThreadID == "" {
				return errors.New("unsnooze 需要 --thread")
			}
			zero := time.Time{}
			command.Kind, command.SnoozedUntil = api.UpdateThread, &zero
		case "fork":
			if command.ThreadID == "" {
				return errors.New("fork 需要 --thread")
			}
			command.Kind, command.Title, command.SourceRunID = api.ForkThread, *title, *runID
		case "merge-back":
			if command.ThreadID == "" || strings.TrimSpace(*targetThread) == "" {
				return errors.New("merge-back 需要 --thread（来源线程）和 --target（目标线程）")
			}
			command.Kind, command.TargetThreadID, command.SourceRunID = api.MergeBackThread, *targetThread, *runID
		case "create":
			command.Kind, command.Title = api.CreateThread, *title
			if command.ThreadID == "" {
				command.ThreadID = "thread_" + command.CommandID
			}
		case "rename", "pin", "unpin", "archive", "restore", "model":
			if command.ThreadID == "" {
				return errors.New("需要 --thread")
			}
			command.Kind = api.UpdateThread
			if action == "rename" {
				if *title == "" {
					return errors.New("rename 需要 --title")
				}
				command.Title = *title
			}
			if action == "pin" || action == "unpin" {
				value := action == "pin"
				command.Pinned = &value
			}
			if action == "archive" || action == "restore" {
				value := action == "archive"
				command.Archived = &value
			}
			if action == "model" {
				if *model == "" {
					return errors.New("model 需要 --model")
				}
				command.Kind = api.SetThreadModel
			}
		case "queue", "cancel-message":
			if command.ThreadID == "" {
				return errors.New("需要 --thread")
			}
			command.Kind, command.Message = api.QueueMessage, *message
			if action == "queue" && *message == "" {
				return errors.New("queue 需要 --message")
			}
			if action == "cancel-message" {
				if *messageID == "" {
					return errors.New("需要 --message-id")
				}
				command.Kind, command.MessageID = api.CancelMessage, *messageID
			}
		case "send":
			if command.ThreadID == "" || *message == "" {
				return errors.New("send 需要 --thread 和非空 --message")
			}
			command.Kind, command.Message = api.SendMessage, *message
		case "cancel-input":
			if *inputID == "" {
				return errors.New("cancel-input 需要 --input")
			}
			command.Kind, command.InputID = api.CancelInput, *inputID
		case "respond":
			provided := 0
			flags.Visit(func(flag *flag.Flag) {
				if flag.Name == "confirm" || flag.Name == "value" || flag.Name == "cancel" {
					provided++
				}
			})
			if *requestID == "" || provided != 1 {
				return errors.New("respond 需要 --request，以及 --confirm、--value 或 --cancel 中的一项")
			}
			command.Kind, command.RequestID = api.RespondRequest, *requestID
			command.Value, command.Confirmed, command.Cancelled = *responseValue, *confirmed, *cancelled
		case "steer":
			if command.ThreadID == "" || *runID == "" || strings.TrimSpace(*message) == "" {
				return errors.New("steer 需要 --thread、--run 和非空 --message")
			}
			command.Kind, command.RunID, command.Message = api.SteerMessage, *runID, *message
			if *attemptID != "" {
				command.Kind, command.ParentAttemptID = api.SteerRun, *attemptID
			}
		case "stop":
			if command.ThreadID == "" || *runID == "" {
				return errors.New("stop 需要 --thread 和 --run")
			}
			command.Kind, command.RunID = api.StopRun, *runID
		case "delegate":
			if *parentRun == "" || *message == "" {
				return errors.New("delegate 需要 --parent-run 和非空 --message")
			}
			command.Kind, command.Title, command.Message = api.DelegateTask, *title, *message
		case "followup":
			if *taskID == "" || *message == "" {
				return errors.New("followup 需要 --task 和非空 --message")
			}
			command.Kind, command.Message = api.FollowUpTask, *message
		case "ack":
			if *parentRun == "" || *generation <= 0 || *deliveryID == "" {
				return errors.New("ack 需要 --parent-run、--generation 和 --delivery")
			}
			command.Kind = api.AcknowledgeTask
		case "dispose":
			if *taskID == "" || *generation <= 0 || *deliveryID == "" {
				return errors.New("dispose 需要 --task、--generation 和 --delivery")
			}
			command.Kind = api.DisposeTask
		}
		receipt, err := service.Dispatch(requestCtx, command)
		if err != nil {
			return err
		}
		return encoder.Encode(receipt)
	default:
		return fmt.Errorf("未知 ctl 操作 %q", action)
	}
}

func watch(ctx context.Context, service api.Service, after int64, encoder *json.Encoder) error {
	subscription, err := service.Subscribe(ctx, after)
	if err != nil {
		return err
	}
	defer subscription.Close()
	events, failures := subscription.Events, subscription.Errors
	for events != nil || failures != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if err := encoder.Encode(event); err != nil {
				return err
			}
		case err, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			if err != nil {
				return err
			}
		}
	}
	return errors.New("Core 事件连接已关闭；重新查询快照后连接")
}

func envDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

type workspaceArguments struct {
	kind, path, repository, branch, base *string
}

func addWorkspaceFlags(flags *flag.FlagSet) workspaceArguments {
	return workspaceArguments{
		kind:       flags.String("workspace-kind", "", "工作区类型：directory 或 worktree"),
		path:       flags.String("workspace", "", "工作区目录"),
		repository: flags.String("repository", "", "worktree 的 Git 仓库"),
		branch:     flags.String("branch", "", "worktree 分支"),
		base:       flags.String("base-ref", "", "worktree 基准引用（默认 HEAD）"),
	}
}

func (arguments workspaceArguments) spec() (*api.WorkspaceSpec, error) {
	if *arguments.kind == "" && *arguments.path == "" && *arguments.repository == "" && *arguments.branch == "" && *arguments.base == "" {
		return nil, nil
	}
	kind := *arguments.kind
	if kind == "" {
		kind = "directory"
		if *arguments.repository != "" {
			kind = "worktree"
		}
	}
	if kind != "directory" && kind != "worktree" {
		return nil, errors.New("workspace-kind 只能为 directory 或 worktree")
	}
	spec := &api.WorkspaceSpec{Kind: kind, Branch: *arguments.branch, BaseRef: *arguments.base}
	var err error
	if *arguments.path != "" {
		spec.Path, err = filepath.Abs(*arguments.path)
		if err != nil {
			return nil, err
		}
	}
	if *arguments.repository != "" {
		spec.Repository, err = filepath.Abs(*arguments.repository)
		if err != nil {
			return nil, err
		}
	}
	if kind == "directory" && spec.Path == "" {
		return nil, errors.New("directory 工作区需要 --workspace")
	}
	if kind == "worktree" && spec.Repository == "" {
		return nil, errors.New("worktree 工作区需要 --repository")
	}
	return spec, nil
}

func runRemoteDesktopRelay(ctx context.Context, listenAddr string, diagnostics io.Writer) error {
	relay := remotedesktop.NewRelayServer()
	defer relay.Close()
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听远程桌面中继 %s：%w", listenAddr, err)
	}
	srv := &http.Server{Handler: relay}
	stop := context.AfterFunc(ctx, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	})
	defer stop()
	fmt.Fprintf(diagnostics, "Vantry Remote Desktop Relay ready on %s\n", ln.Addr().String())
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
		return nil
	}
	return err
}

const usage = `Vantry — 原生桌面 AI 编程与持久任务编排工作台

用法：
  vantry                                  打开原生界面，自动附着或启动 Core
  vantry desktop [--data-dir PATH] [--attach-only]
  vantry core [--data-dir PATH] [--provider pi|controlled] [--model PROVIDER/MODEL] [--workspace PATH]
  vantry ctl [--data-dir PATH] health
  vantry ctl [--data-dir PATH] status
  vantry ctl [--data-dir PATH] create --title 标题 [--provider pi|controlled] [--model PROVIDER/MODEL] [--workspace PATH] [--thread ID] [--command-id ID]
  vantry ctl [--data-dir PATH] send --thread ID --message 内容 [--command-id ID]
  vantry ctl [--data-dir PATH] stop --thread ID --run ID [--command-id ID]
  vantry ctl [--data-dir PATH] steer --thread ID --run ID [--attempt ID] --message 指令 [--command-id ID]
  vantry ctl [--data-dir PATH] cancel-input --input ID
  vantry ctl [--data-dir PATH] respond --request ID [--confirm=true|false | --value 内容 | --cancel]
  vantry ctl [--data-dir PATH] diff|terminal --thread ID
  vantry ctl [--data-dir PATH] delegate --parent-run ID --message 内容 [--parent-attempt ID] [--model PROVIDER/MODEL] [--resume-policy auto|manual]
  vantry ctl [--data-dir PATH] followup --task ID --message 内容
  vantry ctl [--data-dir PATH] ack --parent-run ID --generation N --delivery ID
  vantry ctl [--data-dir PATH] dispose --task ID --generation N --delivery ID
  vantry ctl [--data-dir PATH] watch [--after SEQUENCE]
  vantry ctl [--data-dir PATH] delete --thread ID [--force=false]
  vantry ctl [--data-dir PATH] fork --thread ID [--run RUN_ID] [--title 标题]
  vantry ctl [--data-dir PATH] merge-back --thread 来源ID --target 目标ID [--run RUN_ID]
  vantry ctl [--data-dir PATH] snooze --thread ID (--duration 1h | --until RFC3339)
  vantry ctl [--data-dir PATH] unsnooze --thread ID
  vantry ctl [--data-dir PATH] cancel-task --task ID [--thread ID] [--reason 原因]
  vantry ctl [--data-dir PATH] threads [--limit N] [--settled|--settled=false]
  vantry ctl [--data-dir PATH] thread-read --thread ID [--view messages|activity] [--after-pos N] [--limit N]
  vantry ctl [--data-dir PATH] thread-send --thread 目标ID [--from 发起ID] --message 内容 [--delivery-mode auto|queue|steer]
  vantry ctl [--data-dir PATH] thread-wait --thread ID [--run RUN_ID] [--timeout 60s]
  vantry ctl [--data-dir PATH] models [--workspace PATH] [--model PROVIDER/MODEL]
  vantry ctl [--data-dir PATH] queue --thread ID --message 内容
  vantry ctl [--data-dir PATH] cancel-message --thread ID --message-id ID
  vantry ctl [--data-dir PATH] rename --thread ID --title 标题
  vantry ctl [--data-dir PATH] pin|unpin|archive|restore --thread ID
  vantry ctl [--data-dir PATH] model --thread ID --model PROVIDER/MODEL
  vantry ctl [--data-dir PATH] git --thread ID
  vantry ctl [--data-dir PATH] merge --thread ID --branch 来源分支
  vantry ctl [--data-dir PATH] plugins
  vantry ctl [--data-dir PATH] plugin-run --plugin ID --message 输入
  vantry ctl [--data-dir PATH] upload --file PATH
  vantry desktop --remote SSH_HOST --remote-socket /absolute/path/core.sock
  vantry relay init --config PATH [--desktop]   生成原生中继私有配置
  vantry relay serve --config PATH              运行内置 HTTPS/WSS 会话与桌面中继
  vantry rd-relay [--listen :8788]               旧版 MJPEG 兼容中继

VANTRY_DATA_DIR 可覆盖默认数据目录。ctl 输出 JSON，watch 输出 NDJSON。
默认执行器为真实 Pi，使用已配置的本机 Pi 凭据；--provider controlled 可运行本地演示。
--approval 支持 auto、auto-accept-edits 与 ask。
threads、thread-read、thread-send、thread-wait 优先使用 Core 的会话编排接口，不可用时自动改用快照或命令，因此对任意 Core 均可工作。
等待超时后 thread-wait 返回 timedOut=true 而非报错。
VANTRY_MODEL、VANTRY_PROVIDER、VANTRY_PI_EXECUTABLE 可设置默认执行配置。
worktree 使用 --workspace-kind worktree --repository PATH [--workspace PATH] [--branch NAME] [--base-ref REF]。
关闭界面不停止任务；Pi worker 独立于 Core，重启 Core 后重新附着同一 Attempt。
steer 回执只确认 Core 已保存；status.steering / message.steering 表示 Pi 接纳、拒绝或交付不明，交付不明不会自动重发。
前台运行 core 时用 Ctrl+C 停止服务；显式 ctl stop 才持久取消指定运行。
`
