package api

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// FormatThreadLink formats a canonical markdown session reference aligned with
// the workbench's thread link convention, using Vantry's local thread scheme.
func FormatThreadLink(threadID, title string) string {
	cleanTitle := strings.TrimSpace(title)
	if cleanTitle == "" {
		cleanTitle = threadID
	}
	cleanTitle = strings.NewReplacer("[", "(", "]", ")", "\n", " ", "\r", " ").Replace(cleanTitle)
	return "[" + cleanTitle + "](vantry-thread://" + strings.TrimSpace(threadID) + ")"
}

// FormatThreadReference links a stored thread the way the workbench copies it:
// the label names the session alone, so the reference reads as an ordinary
// Markdown link and its scheme stays out of the way.
func FormatThreadReference(threadID, title, projectPath string) string {
	return FormatThreadLink(threadID, ThreadLinkLabel(title, projectPath))
}

// ThreadLinkLabel is the visible text of a thread reference: the session name
// on its own. A project coordinator's sidebar title carries its project
// ("项目协调员 · demo-app") to separate it from namesakes; a copied
// reference drops that qualifier, because the link already identifies the
// session and the project belongs to the surrounding UI.
func ThreadLinkLabel(title, projectPath string) string {
	label := strings.TrimSpace(title)
	project := strings.TrimSpace(projectPath)
	if label == "" || project == "" {
		return label
	}
	project = filepath.Base(project)
	if project == "" || project == "." || project == string(filepath.Separator) {
		return label
	}
	for _, gap := range []string{" · ", "· ", " ·", "·"} {
		if name := strings.TrimSpace(strings.TrimSuffix(label, gap+project)); name != label {
			if name != "" {
				return name
			}
		}
	}
	return label
}

// ThreadReference links a stored thread for the surfaces that show or copy it.
func ThreadReference(thread Thread) string {
	project := strings.TrimSpace(thread.ProjectPath)
	if project == "" {
		project = WorkspaceProjectPath(thread.Workspace)
	}
	return FormatThreadReference(thread.ID, thread.Title, project)
}

// FormatEnvironmentThreadLink preserves execution scope in links returned by
// a remote broker. A same-named local thread must never be selected by accident.
func FormatEnvironmentThreadLink(environmentID, link string) string {
	link = strings.TrimSpace(link)
	if environmentID == "" || link == "" {
		return link
	}
	return strings.TrimSuffix(link, ")") + "?environmentId=" + url.QueryEscape(environmentID) + ")"
}

// OrchestratorRunningTaskNext is returned on non-terminal delegated tasks and
// waits so Pi agents return control instead of busy-polling.
const OrchestratorRunningTaskNext = "Do not poll with task_status or sleep. End your turn if you are waiting on async child work; Vantry Core will automatically wake and continue this session when the child settles or asks a question."

type OrchestratorModelOption struct {
	ID      string   `json:"id"`
	Values  []string `json:"values"`
	Default string   `json:"default,omitempty"`
}

type OrchestratorModelCapability struct {
	ID      string                    `json:"id"`
	Label   string                    `json:"label,omitempty"`
	Options []OrchestratorModelOption `json:"options,omitempty"`
}

type OrchestratorCapabilitiesQuery struct {
	EnvironmentID  string `json:"environmentId,omitempty"`
	CallerThreadID string `json:"callerThreadId,omitempty"`
	Search         string `json:"search,omitempty"`
}

type OrchestratorCapabilities struct {
	ParentThreadID      string                        `json:"parentThreadId,omitempty"`
	InheritedProviderID string                        `json:"inheritedProviderId"`
	InheritedModel      string                        `json:"inheritedModel,omitempty"`
	InheritedThinking   string                        `json:"inheritedThinking,omitempty"`
	ApprovalPolicy      string                        `json:"approvalPolicy"`
	InteractionMode     string                        `json:"interactionMode"`
	Models              []OrchestratorModelCapability `json:"models"`
	ApprovalPolicies    []string                      `json:"approvalPolicies"`
	InteractionModes    []string                      `json:"interactionModes"`
	MaxBatchThreads     int                           `json:"maxBatchThreads"`
}

type PendingApprovalSummary struct {
	RequestID string `json:"requestId"`
	Method    string `json:"method"`
	Title     string `json:"title,omitempty"`
	RunID     string `json:"runId"`
}

type TaskDetail struct {
	ChildAttemptID      string                  `json:"childAttemptId,omitempty"`
	EnvironmentID       string                  `json:"environmentId,omitempty"`
	RemoteThreadID      string                  `json:"remoteThreadId,omitempty"`
	RemoteRunID         string                  `json:"remoteRunId,omitempty"`
	TaskID              string                  `json:"taskId"`
	ParentThreadID      string                  `json:"parentThreadId"`
	ParentRunID         string                  `json:"parentRunId"`
	ChildThreadID       string                  `json:"childThreadId"`
	Link                string                  `json:"link"`
	ChildRunID          string                  `json:"childRunId,omitempty"`
	Title               string                  `json:"title"`
	Status              RunStatus               `json:"status"`
	WorkState           string                  `json:"workState"` // working | waiting_for_children | result_available
	DeliveryStatus      DeliveryStatus          `json:"deliveryStatus"`
	HasPendingChildRuns bool                    `json:"hasPendingChildRuns"`
	HasPendingQuestion  bool                    `json:"hasPendingQuestion"`
	PendingApproval     *PendingApprovalSummary `json:"pendingApproval,omitempty"`
	Model               string                  `json:"model,omitempty"`
	Summary             string                  `json:"summary,omitempty"`
	Error               string                  `json:"error,omitempty"`
	WaitTimedOut        bool                    `json:"waitTimedOut,omitempty"`
	Next                string                  `json:"next,omitempty"`
	UpdatedAt           time.Time               `json:"updatedAt"`
}

// ResolvedDelegateTarget is supplied inside the app after resolving a saved
// target. It is excluded from the client request identity: retries retain the
// settings already committed by the first successful delegation.
type ResolvedDelegateTarget struct {
	ProviderID string
	Model      string
	Workspace  *WorkspaceSpec
}

type DelegateTaskRequest struct {
	ResolvedTarget  *ResolvedDelegateTarget `json:"-"`
	EnvironmentID   string                  `json:"environmentId,omitempty"`
	Workspace       *WorkspaceSpec          `json:"workspace,omitempty"`
	ProviderID      string                  `json:"providerId,omitempty"`
	ClientRequestID string                  `json:"clientRequestId,omitempty"`
	CallerThreadID  string                  `json:"callerThreadId,omitempty"`
	ParentRunID     string                  `json:"parentRunId"`
	ParentAttemptID string                  `json:"parentAttemptId,omitempty"`
	Title           string                  `json:"title,omitempty"`
	Task            string                  `json:"task"`
	Role            string                  `json:"role,omitempty"` // implementation | research | review | design | test | general
	Model           string                  `json:"model,omitempty"`
	Thinking        string                  `json:"thinking,omitempty"`
	Isolation       string                  `json:"isolation,omitempty"` // shared | worktree
	Mode            string                  `json:"mode,omitempty"`      // async | wait
	TimeoutMs       int                     `json:"timeoutMs,omitempty"`
	ApprovalPolicy  string                  `json:"approvalPolicy,omitempty"`
	InteractionMode string                  `json:"interactionMode,omitempty"`
	ResumePolicy    string                  `json:"resumePolicy,omitempty"`
}

type TaskStatusQuery struct {
	CallerThreadID string `json:"callerThreadId,omitempty"`
	ParentRunID    string `json:"parentRunId,omitempty"`
	TaskID         string `json:"taskId,omitempty"`
	Acknowledge    bool   `json:"acknowledge,omitempty"`
}

type TaskStatusResult struct {
	ThreadID string       `json:"threadId,omitempty"`
	RunID    string       `json:"runId,omitempty"`
	Tasks    []TaskDetail `json:"tasks"`
	Cohorts  []Cohort     `json:"cohorts,omitempty"`
}

type CancelTaskRequest struct {
	ClientRequestID string `json:"clientRequestId,omitempty"`
	CallerThreadID  string `json:"callerThreadId,omitempty"`
	TaskID          string `json:"taskId"`
	Reason          string `json:"reason,omitempty"`
}

type CancelTaskResult struct {
	TaskID string `json:"taskId"`
	Status string `json:"status"`
}

type ThreadListItem struct {
	EnvironmentID     string     `json:"environmentId,omitempty"`
	ThreadID          string     `json:"threadId"`
	Title             string     `json:"title"`
	Link              string     `json:"link"`
	Status            string     `json:"status"` // idle | queued | running | waiting_children | completed | failed | cancelled | interrupted
	Model             string     `json:"model,omitempty"`
	ApprovalPolicy    string     `json:"approvalPolicy,omitempty"`
	InteractionMode   string     `json:"interactionMode,omitempty"`
	WorkspacePath     string     `json:"workspacePath,omitempty"`
	Branch            string     `json:"branch,omitempty"`
	IsWorktree        bool       `json:"isWorktree,omitempty"`
	ParentThreadID    string     `json:"parentThreadId,omitempty"`
	CreatedByThreadID string     `json:"createdByThreadId,omitempty"`
	Pinned            bool       `json:"pinned,omitempty"`
	Settled           bool       `json:"settled"`
	Snoozed           bool       `json:"snoozed"`
	SnoozedUntil      *time.Time `json:"snoozedUntil,omitempty"`
	RunCount          int        `json:"runCount"`
	PendingInputs     int        `json:"pendingInputs"`
	PendingRequests   int        `json:"pendingRequests"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

type ThreadListQuery struct {
	EnvironmentID    string   `json:"environmentId,omitempty"`
	CallerThreadID   string   `json:"callerThreadId,omitempty"`
	Workspace        string   `json:"workspace,omitempty"`
	AllProjects      bool     `json:"allProjects,omitempty"`
	IncludeSubagents bool     `json:"includeSubagents,omitempty"`
	Settled          *bool    `json:"settled,omitempty"`
	Snoozed          *bool    `json:"snoozed,omitempty"`
	Statuses         []string `json:"statuses,omitempty"`
	TitleContains    string   `json:"titleContains,omitempty"`
	Cursor           int      `json:"cursor,omitempty"`
	Limit            int      `json:"limit,omitempty"`
}

type ThreadListResult struct {
	CurrentThreadID string           `json:"currentThreadId,omitempty"`
	Threads         []ThreadListItem `json:"threads"`
	NextCursor      *int             `json:"nextCursor,omitempty"`
	Total           int              `json:"total"`
}

type ThreadTimelineItem struct {
	Position       int       `json:"position"`
	ItemID         string    `json:"itemId"`
	RunID          string    `json:"runId,omitempty"`
	CreatedBy      string    `json:"createdBy"` // user | agent | system
	SenderThreadID string    `json:"senderThreadId,omitempty"`
	Type           string    `json:"type"` // user_message | assistant_message | reasoning | tool_call | approval_request | subagent | error | project_update
	Status         string    `json:"status"`
	Title          string    `json:"title,omitempty"`
	Text           string    `json:"text,omitempty"`
	TextTruncated  bool      `json:"textTruncated,omitempty"`
	NextTextOffset *int      `json:"nextTextOffset,omitempty"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type ThreadRunSummary struct {
	RunID          string    `json:"runId"`
	Ordinal        int       `json:"ordinal"`
	Status         RunStatus `json:"status"`
	Model          string    `json:"model,omitempty"`
	SenderThreadID string    `json:"senderThreadId,omitempty"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

type ThreadReadQuery struct {
	EnvironmentID   string `json:"environmentId,omitempty"`
	CallerThreadID  string `json:"callerThreadId,omitempty"`
	ThreadID        string `json:"threadId"`
	View            string `json:"view,omitempty"` // messages | activity
	AfterPosition   *int   `json:"afterPosition,omitempty"`
	BeforePosition  *int   `json:"beforePosition,omitempty"`
	Limit           int    `json:"limit,omitempty"`
	RunLimit        int    `json:"runLimit,omitempty"`
	MaxCharsPerItem int    `json:"maxCharsPerItem,omitempty"`
	ItemID          string `json:"itemId,omitempty"`
	TextOffset      int    `json:"textOffset,omitempty"`
}

type ThreadReadResult struct {
	Thread          ThreadListItem       `json:"thread"`
	RecentRuns      []ThreadRunSummary   `json:"recentRuns"`
	Items           []ThreadTimelineItem `json:"items"`
	NextPosition    *int                 `json:"nextPosition,omitempty"`
	EarlierPosition *int                 `json:"earlierPosition,omitempty"`
	HasMore         bool                 `json:"hasMore"`
}

type ThreadLaunchSpec struct {
	EnvironmentID   string         `json:"environmentId,omitempty"`
	Title           string         `json:"title,omitempty"`
	Message         string         `json:"message,omitempty"`
	Model           string         `json:"model,omitempty"`
	Thinking        string         `json:"thinking,omitempty"`
	ApprovalPolicy  string         `json:"approvalPolicy,omitempty"`
	InteractionMode string         `json:"interactionMode,omitempty"`
	Workspace       *WorkspaceSpec `json:"workspace,omitempty"`
	Scratch         bool           `json:"scratch,omitempty"`
}

type ThreadLaunchRequest struct {
	ClientRequestID  string `json:"clientRequestId,omitempty"`
	CallerThreadID   string `json:"callerThreadId,omitempty"`
	CallerRunID      string `json:"callerRunId,omitempty"`
	NotifyOnComplete bool   `json:"notifyOnComplete,omitempty"`
	ThreadLaunchSpec
}

type LaunchedThread struct {
	EnvironmentID string    `json:"environmentId,omitempty"`
	ThreadID      string    `json:"threadId"`
	Link          string    `json:"link"`
	Title         string    `json:"title"`
	RunID         string    `json:"runId,omitempty"`
	Status        RunStatus `json:"status"`
	Model         string    `json:"model,omitempty"`
}

type CreateThreadsRequest struct {
	ClientRequestID string             `json:"clientRequestId,omitempty"`
	CallerThreadID  string             `json:"callerThreadId,omitempty"`
	CallerRunID     string             `json:"callerRunId,omitempty"`
	Threads         []ThreadLaunchSpec `json:"threads"`
}

type CreateThreadsResult struct {
	Threads []LaunchedThread `json:"threads"`
}

type InterThreadSendRequest struct {
	EnvironmentID    string `json:"environmentId,omitempty"`
	ClientRequestID  string `json:"clientRequestId,omitempty"`
	CallerThreadID   string `json:"callerThreadId,omitempty"`
	TargetThreadID   string `json:"targetThreadId"`
	Message          string `json:"message"`
	Mode             string `json:"mode,omitempty"` // auto | queue | steer
	NotifyOnComplete bool   `json:"notifyOnComplete,omitempty"`
}

type InterThreadSendResult struct {
	EnvironmentID string    `json:"environmentId,omitempty"`
	ThreadID      string    `json:"threadId"`
	Link          string    `json:"link"`
	RunID         string    `json:"runId,omitempty"`
	InputID       string    `json:"inputId,omitempty"`
	TaskID        string    `json:"taskId,omitempty"`
	Status        RunStatus `json:"status,omitempty"`
	Delivery      string    `json:"delivery"` // started | queued | steered
	Sequence      int64     `json:"sequence"`
}

type ThreadWaitQuery struct {
	EnvironmentID             string `json:"environmentId,omitempty"`
	CallerThreadID            string `json:"callerThreadId,omitempty"`
	ThreadID                  string `json:"threadId"`
	RunID                     string `json:"runId,omitempty"`
	TimeoutMs                 int    `json:"timeoutMs,omitempty"`
	UntilDelegatedWorkSettled bool   `json:"untilDelegatedWorkSettled,omitempty"`
}

type ThreadWaitResult struct {
	EnvironmentID      string                  `json:"environmentId,omitempty"`
	ThreadID           string                  `json:"threadId"`
	Link               string                  `json:"link"`
	RunID              string                  `json:"runId,omitempty"`
	Status             string                  `json:"status"`
	TimedOut           bool                    `json:"timedOut"`
	AutoWakeRegistered bool                    `json:"autoWakeRegistered,omitempty"`
	Summary            string                  `json:"summary,omitempty"`
	Error              string                  `json:"error,omitempty"`
	HasPendingQuestion bool                    `json:"hasPendingQuestion,omitempty"`
	PendingApproval    *PendingApprovalSummary `json:"pendingApproval,omitempty"`
	Next               string                  `json:"next,omitempty"`
}

// OrchestrationBroker exposes high-level Pi-to-Pi session orchestration,
// conversation, paginated timeline reads, and event-driven waits.
type OrchestrationBroker interface {
	OrchestratorCapabilities(context.Context, OrchestratorCapabilitiesQuery) (OrchestratorCapabilities, error)
	DelegateTask(context.Context, DelegateTaskRequest) (TaskDetail, error)
	TaskStatus(context.Context, TaskStatusQuery) (TaskStatusResult, error)
	CancelTask(context.Context, CancelTaskRequest) (CancelTaskResult, error)
	ListThreads(context.Context, ThreadListQuery) (ThreadListResult, error)
	ReadThread(context.Context, ThreadReadQuery) (ThreadReadResult, error)
	LaunchThread(context.Context, ThreadLaunchRequest) (LaunchedThread, error)
	CreateThreads(context.Context, CreateThreadsRequest) (CreateThreadsResult, error)
	SendToThread(context.Context, InterThreadSendRequest) (InterThreadSendResult, error)
	WaitThread(context.Context, ThreadWaitQuery) (ThreadWaitResult, error)
}
