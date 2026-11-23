// Package api defines Vantry's versioned local command and subscription contract.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const ProtocolVersion = 1

type CommandKind string

const (
	OpenProjectChat     CommandKind = "project.chat.open"
	CreateThread        CommandKind = "thread.create"
	DeleteThread        CommandKind = "thread.delete"
	ForkThread          CommandKind = "thread.fork"
	RewindThread        CommandKind = "thread.rewind"
	MergeBackThread     CommandKind = "thread.merge_back"
	EditInput           CommandKind = "input.edit"
	ReorderInput        CommandKind = "input.reorder"
	PromoteInputToSteer CommandKind = "input.promote_steer"
	SetThreadModel      CommandKind = "thread.model"
	CancelInput         CommandKind = "input.cancel"
	SteerRun            CommandKind = "run.steer"
	RespondRequest      CommandKind = "request.respond"
	SendMessage         CommandKind = "message.send"
	StopRun             CommandKind = "run.stop"
	DelegateTask        CommandKind = "task.delegate"
	FollowUpTask        CommandKind = "task.followup"
	AcknowledgeTask     CommandKind = "task.acknowledge"
	DisposeTask         CommandKind = "task.dispose"
	CancelTask          CommandKind = "task.cancel"
	UpgradeTaskWake     CommandKind = "task.wake_upgrade"
	InterThreadSend     CommandKind = "thread.inter_send"
	UpdateThread        CommandKind = "thread.update"
	QueueMessage        CommandKind = "message.queue"
	CancelMessage       CommandKind = "message.cancel"
	SteerMessage        CommandKind = "message.steer"
)

// CommandID identifies the complete command, including its payload. Reusing an
// ID with different content is a conflict. An identical retry returns its receipt.
type Command struct {
	Version           int         `json:"version"`
	CommandID         string      `json:"commandId"`
	Kind              CommandKind `json:"kind"`
	ThreadID          string      `json:"threadId,omitempty"`
	Title             string      `json:"title,omitempty"`
	Message           string      `json:"message,omitempty"`
	RunID             string      `json:"runId,omitempty"`
	SourceRunID       string      `json:"sourceRunId,omitempty"`
	TargetThreadID    string      `json:"targetThreadId,omitempty"`
	SenderThreadID    string      `json:"senderThreadId,omitempty"`
	CreatedByThreadID string      `json:"createdByThreadId,omitempty"`
	CreatedByRunID    string      `json:"createdByRunId,omitempty"`
	DeliveryMode      string      `json:"deliveryMode,omitempty"`    // auto | queue | steer for thread.inter_send
	InteractionMode   string      `json:"interactionMode,omitempty"` // default | plan; empty inherits thread settings
	Position          int         `json:"position,omitempty"`        // zero-based destination for input.reorder
	// Completed thread-level steers (no explicit attempt) fall back by default;
	// FallbackToQueue also enables this for a steer bound to a specific attempt.
	FallbackToQueue   bool           `json:"fallbackToQueue,omitempty"`
	ProviderID        string         `json:"providerId,omitempty"`
	Model             string         `json:"model,omitempty"`
	Workspace         *WorkspaceSpec `json:"workspace,omitempty"`
	ParentRunID       string         `json:"parentRunId,omitempty"`
	ParentAttemptID   string         `json:"parentAttemptId,omitempty"`
	TaskID            string         `json:"taskId,omitempty"`
	Generation        int64          `json:"generation,omitempty"`
	DeliveryMessageID string         `json:"deliveryMessageId,omitempty"`
	ResumePolicy      string         `json:"resumePolicy,omitempty"`
	Pinned            *bool          `json:"pinned,omitempty"`
	Archived          *bool          `json:"archived,omitempty"`
	SnoozedUntil      *time.Time     `json:"snoozedUntil,omitempty"`
	Force             bool           `json:"force,omitempty"`
	Reason            string         `json:"reason,omitempty"`
	MessageID         string         `json:"messageId,omitempty"`
	Attachments       []Attachment   `json:"attachments,omitempty"`
	ApprovalPolicy    string         `json:"approvalPolicy,omitempty"`
	InputID           string         `json:"inputId,omitempty"`
	RequestID         string         `json:"requestId,omitempty"`
	Value             string         `json:"value,omitempty"`
	Confirmed         bool           `json:"confirmed,omitempty"`
	Cancelled         bool           `json:"cancelled,omitempty"`
}

type Receipt struct {
	CommandID         string `json:"commandId"`
	Accepted          bool   `json:"accepted"`
	ThreadID          string `json:"threadId"`
	RunID             string `json:"runId,omitempty"`
	Sequence          int64  `json:"sequence"`
	MessageID         string `json:"messageId,omitempty"`
	InputID           string `json:"inputId,omitempty"`
	RequestID         string `json:"requestId,omitempty"`
	TaskID            string `json:"taskId,omitempty"`
	Generation        int64  `json:"generation,omitempty"`
	DeliveryMessageID string `json:"deliveryMessageId,omitempty"`
	Delivery          string `json:"delivery,omitempty"`
}

type Thread struct {
	// ProjectPath is set only on the unique coordinator chat for this project.
	ProjectPath        string         `json:"projectPath,omitempty"`
	ID                 string         `json:"id"`
	Title              string         `json:"title"`
	CreatedAt          time.Time      `json:"createdAt"`
	ProviderID         string         `json:"providerId,omitempty"`
	Model              string         `json:"model,omitempty"`
	Workspace          *WorkspaceSpec `json:"workspace,omitempty"`
	ParentThreadID     string         `json:"parentThreadId,omitempty"`
	ParentTaskID       string         `json:"parentTaskId,omitempty"`
	CreatedByThreadID  string         `json:"createdByThreadId,omitempty"`
	CreatedByRunID     string         `json:"createdByRunId,omitempty"`
	Pinned             bool           `json:"pinned,omitempty"`
	Archived           bool           `json:"archived,omitempty"`
	SnoozedUntil       *time.Time     `json:"snoozedUntil,omitempty"`
	ApprovalPolicy     string         `json:"approvalPolicy,omitempty"`
	InteractionMode    string         `json:"interactionMode,omitempty"`
	ForkedFromThreadID string         `json:"forkedFromThreadId,omitempty"`
	ForkedFromRunID    string         `json:"forkedFromRunId,omitempty"`
}

// WorkspaceSpec records the requested workspace. Worktrees are prepared before
// provider execution; a preparation failure never falls back to another cwd.
type WorkspaceSpec struct {
	// EnvironmentID routes execution to a linked Core. Paths belong to that
	// environment and must never be prepared or opened on the caller's host.
	EnvironmentID string `json:"environmentId,omitempty"`
	Kind          string `json:"kind"`
	Path          string `json:"path,omitempty"`
	Repository    string `json:"repository,omitempty"`
	Branch        string `json:"branch,omitempty"`
	BaseRef       string `json:"baseRef,omitempty"`
}

type RunStatus string

const (
	RunQueued      RunStatus = "queued"
	RunRunning     RunStatus = "running"
	RunWaiting     RunStatus = "waiting_children"
	RunCompleted   RunStatus = "completed"
	RunCancelled   RunStatus = "cancelled"
	RunInterrupted RunStatus = "interrupted"
	RunFailed      RunStatus = "failed"
)

func (s RunStatus) Terminal() bool {
	return s == RunCompleted || s == RunCancelled || s == RunInterrupted || s == RunFailed
}

type RunTelemetry struct {
	Phase             string  `json:"phase,omitempty"`
	Compacting        bool    `json:"compacting,omitempty"`
	CompactionReason  string  `json:"compactionReason,omitempty"`
	Retrying          bool    `json:"retrying,omitempty"`
	RetryAttempt      int     `json:"retryAttempt,omitempty"`
	RetryMaxAttempts  int     `json:"retryMaxAttempts,omitempty"`
	RetryReason       string  `json:"retryReason,omitempty"`
	RetryWaitSeconds  int     `json:"retryWaitSeconds,omitempty"`
	RetryCount        int     `json:"retryCount,omitempty"`
	TurnCount         int     `json:"turnCount,omitempty"`
	InputTokens       int64   `json:"inputTokens,omitempty"`
	OutputTokens      int64   `json:"outputTokens,omitempty"`
	CacheReadTokens   int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens  int64   `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens   int64   `json:"reasoningTokens,omitempty"`
	TotalTokens       int64   `json:"totalTokens,omitempty"`
	LastContextTokens int64   `json:"lastContextTokens,omitempty"`
	CostUSD           float64 `json:"costUsd,omitempty"`
	CostEstimated     bool    `json:"costEstimated,omitempty"`
	TTFTMs            int64   `json:"ttftMs,omitempty"`
	DurationMs        int64   `json:"durationMs,omitempty"`
}

type Run struct {
	ID              string         `json:"id"`
	ThreadID        string         `json:"threadId"`
	SenderThreadID  string         `json:"senderThreadId,omitempty"`
	AttemptID       string         `json:"attemptId"`
	Status          RunStatus      `json:"status"`
	Input           string         `json:"input"`
	Attachments     []Attachment   `json:"attachments,omitempty"`
	ApprovalPolicy  string         `json:"approvalPolicy,omitempty"`
	Output          string         `json:"output"`
	Reasoning       string         `json:"reasoning,omitempty"`
	InteractionMode string         `json:"interactionMode,omitempty"`
	Error           string         `json:"error,omitempty"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
	ProviderID      string         `json:"providerId,omitempty"`
	Model           string         `json:"model,omitempty"`
	Workspace       *WorkspaceSpec `json:"workspace,omitempty"`
	Telemetry       *RunTelemetry  `json:"telemetry,omitempty"`
}

type DeliveryStatus string

const (
	DeliveryPending      DeliveryStatus = "pending"
	DeliveryDelivered    DeliveryStatus = "delivered"
	DeliveryAcknowledged DeliveryStatus = "acknowledged"
	DeliveryDisposed     DeliveryStatus = "disposed"
)

// A task remains identifiable across follow-ups; Generation and the delivery
// message identity reject acknowledgements from an earlier cohort.
type Task struct {
	ID                string         `json:"id"`
	ParentThreadID    string         `json:"parentThreadId"`
	ParentRunID       string         `json:"parentRunId"`
	ChildThreadID     string         `json:"childThreadId"`
	ChildRunID        string         `json:"childRunId"`
	Title             string         `json:"title"`
	Generation        int64          `json:"generation"`
	Status            RunStatus      `json:"status"`
	DeliveryStatus    DeliveryStatus `json:"deliveryStatus"`
	DeliveryMessageID string         `json:"deliveryMessageId,omitempty"`
	Result            string         `json:"result,omitempty"`
	Error             string         `json:"error,omitempty"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
}

type Cohort struct {
	ParentRunID           string         `json:"parentRunId"`
	Generation            int64          `json:"generation"`
	ResumePolicy          string         `json:"resumePolicy"`
	DeliveryMessageID     string         `json:"deliveryMessageId,omitempty"`
	Status                DeliveryStatus `json:"status"`
	ContinuationAttemptID string         `json:"continuationAttemptId,omitempty"`
	CreatedAt             time.Time      `json:"createdAt"`
	UpdatedAt             time.Time      `json:"updatedAt"`
}

type Snapshot struct {
	ProjectUpdates []ProjectUpdate   `json:"projectUpdates,omitempty"`
	Sequence       int64             `json:"sequence"`
	Threads        []Thread          `json:"threads"`
	Runs           []Run             `json:"runs"`
	Tasks          []Task            `json:"tasks"`
	Cohorts        []Cohort          `json:"cohorts"`
	Messages       []QueuedMessage   `json:"messages,omitempty"`
	Steering       []SteeringMessage `json:"steering,omitempty"`
	Inputs         []Input           `json:"inputs,omitempty"`
	Tools          []ToolCall        `json:"tools,omitempty"`
	Requests       []ProviderRequest `json:"requests,omitempty"`
}

// Event sequences are global and strictly increasing. The first milestone does
// not prune events. Data contains committed entity data or an output delta.
type Event struct {
	Sequence  int64           `json:"sequence"`
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	ThreadID  string          `json:"threadId"`
	RunID     string          `json:"runId,omitempty"`
	AttemptID string          `json:"attemptId,omitempty"`
	Timestamp time.Time       `json:"timestamp"`
	Data      json.RawMessage `json:"data"`
}

type Health struct {
	Version int    `json:"version"`
	State   string `json:"state"`
}

// Subscribe must replay committed events after the cursor and then continue
// live without a gap. A slow consumer receives resync_required and disconnects.
// Close must be safe to call repeatedly and must release subscription resources.
type Subscription struct {
	Events <-chan Event
	Errors <-chan error
	Close  func()
}

type Service interface {
	Health(context.Context) (Health, error)
	Dispatch(context.Context, Command) (Receipt, error)
	Snapshot(context.Context) (Snapshot, error)
	Subscribe(context.Context, int64) (*Subscription, error)
}

type ErrorCode string

const (
	InvalidCommand  ErrorCode = "invalid_command"
	VersionMismatch ErrorCode = "version_mismatch"
	NotReady        ErrorCode = "not_ready"
	NotFound        ErrorCode = "not_found"
	Conflict        ErrorCode = "conflict"
	ResyncRequired  ErrorCode = "resync_required"
	InternalError   ErrorCode = "internal_error"
)

type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

func CodeOf(err error) ErrorCode {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return InternalError
}
