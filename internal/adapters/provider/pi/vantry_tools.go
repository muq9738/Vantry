package pi

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
)

//go:embed vantry_tools.ts
var vantryToolsExtension []byte

const (
	ApprovalPolicyAuto             = "auto"
	ApprovalPolicyFullAccess       = "full-access"
	ApprovalPolicyAutoAcceptEdits  = "auto-accept-edits"
	ApprovalPolicyAsk              = "ask"
	ApprovalPolicyApprovalRequired = "approval-required"

	InteractionModeDefault = "default"
	InteractionModePlan    = "plan"

	EnvUserExtensions = "VANTRY_PI_USER_EXTENSIONS"
	EnvUserMCP        = "VANTRY_PI_USER_MCP"
	// EnvNativeSubagents opts back into Pi-native child tools such as
	// pi-subagents. They are off by default: their runs are invisible to Core,
	// and a detached run cannot wake a Vantry thread whose Pi exited.
	EnvNativeSubagents = "VANTRY_PI_NATIVE_SUBAGENTS"
	EnvApprovalPolicy  = "VANTRY_APPROVAL_POLICY"
	EnvInteractionMode = "VANTRY_INTERACTION_MODE"
	EnvExecutable      = "VANTRY_EXECUTABLE"
	EnvDataDir         = "VANTRY_DATA_DIR"
	EnvSocketPath      = "VANTRY_SOCKET_PATH"
	EnvJobDir          = "VANTRY_JOB_DIR"
	EnvRunID           = "VANTRY_RUN_ID"
	EnvAttemptID       = "VANTRY_ATTEMPT_ID"
	EnvThreadID            = "VANTRY_THREAD_ID"
	EnvParentTaskID        = "VANTRY_PARENT_TASK_ID"
	EnvDirectOrchestration = "VANTRY_DIRECT_ORCHESTRATION"
	EnvToolIdleTimeout     = "VANTRY_PI_TOOL_IDLE_TIMEOUT"
	EnvStreamIdleTimeout   = "VANTRY_PI_STREAM_IDLE_TIMEOUT"
)

func normalizeApprovalPolicyValue(value string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch trimmed {
	case "":
		return ApprovalPolicyAuto, nil
	case ApprovalPolicyAuto,
		ApprovalPolicyFullAccess,
		ApprovalPolicyAutoAcceptEdits,
		ApprovalPolicyAsk,
		ApprovalPolicyApprovalRequired:
		return trimmed, nil
	default:
		return "", fmt.Errorf("unsupported approval policy %q", value)
	}
}

func normalizeInteractionModeValue(value string) (string, error) {
	trimmed := strings.ToLower(strings.TrimSpace(value))
	switch trimmed {
	case "":
		return "", nil
	case InteractionModeDefault, InteractionModePlan:
		return trimmed, nil
	default:
		return "", fmt.Errorf("unsupported interaction mode %q", value)
	}
}

func resolveBoolEnvOverride(envKey string, fallback bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(envKey)))
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func effectiveUserExtensions(job jobRequest) bool {
	return resolveBoolEnvOverride(EnvUserExtensions, job.EnableUserExtensions)
}

func effectiveUserMCP(job jobRequest) bool {
	return resolveBoolEnvOverride(EnvUserMCP, job.EnableUserMCP)
}

// nativeSubagentTools are the tools user Pi extensions (pi-subagents) add for
// provider-native delegation. Vantry excludes them unless explicitly enabled.
const nativeSubagentTools = "subagent,subagent_supervisor,bg_wait,contact_supervisor"

func effectiveNativeSubagents(job jobRequest) bool {
	return effectiveUserExtensions(job) && resolveBoolEnvOverride(EnvNativeSubagents, job.EnableNativeSubagents)
}

func effectiveInteractionMode(job jobRequest) string {
	if mode, err := normalizeInteractionModeValue(job.Request.InteractionMode); err == nil && mode != "" {
		return mode
	}
	if mode, err := normalizeInteractionModeValue(job.InteractionMode); err == nil && mode != "" {
		return mode
	}
	if mode, err := normalizeInteractionModeValue(os.Getenv(EnvInteractionMode)); err == nil && mode != "" {
		return mode
	}
	return ""
}

func requiresApprovalExtension(job jobRequest) bool {
	if effectiveInteractionMode(job) == InteractionModePlan {
		return true
	}
	policy, err := normalizeApprovalPolicyValue(job.Request.ApprovalPolicy)
	if err != nil {
		return true
	}
	switch policy {
	case ApprovalPolicyAsk, ApprovalPolicyApprovalRequired, ApprovalPolicyAutoAcceptEdits:
		return true
	default:
		return false
	}
}
