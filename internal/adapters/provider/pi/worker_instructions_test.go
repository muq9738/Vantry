package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/muq9738/Vantry/pkg/provider"
)

func testInstructionJob() jobRequest {
	return jobRequest{
		Executable: "/Applications/Vantry.app/Contents/MacOS/Vantry",
		DataDir:    "/tmp/vantry-data",
		SocketPath: "/tmp/vantry-data/core.sock",
		Request: provider.Request{
			ThreadID:  "th_parent",
			RunID:     "run_parent",
			AttemptID: "att_parent",
		},
	}
}

func TestWorkerInstructionsParentVsChild(t *testing.T) {
	parentJob := testInstructionJob()
	parentPrompt := workerInstructions(parentJob)
	t.Logf("slimmed sizes: visualReplyInstructions=%d parent=%d", len(visualReplyInstructions), len(parentPrompt))
	for _, want := range []string{
		"Native Visuals & Structured Replies (On-Demand)",
		"orchestration.md",
		"visual-blocks.md",
		"```dispatch",
		"vantry_delegate_task",
		"vantry_request_secret",
		"GBK/GB18030",
		"directly in parallel",
		"list_projects",
		"text()",
	} {
		if !strings.Contains(parentPrompt, want) {
			t.Fatalf("parent instructions missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"Memcached",
		"Latency p95",
		"POST /v1/tasks",
		"ctl --data-dir",
		"vantry_thread_transfer",
		"vantry_thread_manage",
		"run_parent",
		"att_parent",
	} {
		if strings.Contains(parentPrompt, forbidden) {
			t.Fatalf("parent instructions should keep heavy/volatile reference %q out of permanent prompt", forbidden)
		}
	}
	if len(visualReplyInstructions) <= 3000 {
		t.Fatalf("expected visualReplyInstructions to hold full reference, got %d bytes", len(visualReplyInstructions))
	}
	if len(parentPrompt) >= 2400 {
		t.Fatalf("expected slimmed parent instructions under 2400 bytes, got %d", len(parentPrompt))
	}

	childJob := parentJob
	childJob.Request.ThreadID = "th_child"
	childJob.Request.ParentTaskID = "task_123"
	childPrompt := workerInstructions(childJob)
	t.Logf("child size: %d", len(childPrompt))

	for _, want := range []string{
		"task_123",
		"codemode",
		"tool_search",
		"vantry_request_secret",
		"directly in parallel",
		"list_projects",
		"text()",
	} {
		if !strings.Contains(childPrompt, want) {
			t.Fatalf("child instructions missing %q", want)
		}
	}
	for _, forbidden := range []string{
		"Native Visuals & Structured Replies",
		"CLI fallback",
		"```dispatch",
	} {
		if strings.Contains(childPrompt, forbidden) {
			t.Fatalf("child instructions should not include heavy parent prompt %q", forbidden)
		}
	}
	if len(childPrompt) >= 1300 || len(childPrompt)*2 >= len(parentPrompt) {
		t.Fatalf("expected child instructions to stay compact vs parent: child=%d parent=%d", len(childPrompt), len(parentPrompt))
	}

	parentJob.EnableUserMCP = true
	childJob.EnableUserMCP = true
	mcpParent := workerInstructions(parentJob)
	mcpChild := workerInstructions(childJob)
	t.Logf("MCP sizes: parent=%d child=%d", len(mcpParent), len(mcpChild))
	for _, want := range []string{"describeNamespace", "searchTools", "tools.mcp__"} {
		if !strings.Contains(mcpParent, want) || !strings.Contains(mcpChild, want) {
			t.Fatalf("EnableUserMCP=true missing %q", want)
		}
	}
	if len(mcpParent) >= 2700 {
		t.Fatalf("expected MCP parent instructions under 2700 bytes, got %d", len(mcpParent))
	}
	if len(mcpChild) >= 1600 || len(mcpChild) >= len(mcpParent) {
		t.Fatalf("expected MCP child instructions to remain compact: child=%d parent=%d", len(mcpChild), len(mcpParent))
	}
}

func TestWorkerInstructionsPlanMode(t *testing.T) {
	baseParent := testInstructionJob()
	for _, mode := range []string{"", InteractionModeDefault} {
		t.Run("non-plan-"+mode, func(t *testing.T) {
			job := baseParent
			job.Request.InteractionMode = mode
			if strings.Contains(workerInstructions(job), "Plan Mode (Read-Only Deep Investigation)") {
				t.Fatalf("mode %q should not inject plan mode instructions", mode)
			}
		})
	}

	sources := []struct {
		name  string
		setup func(t *testing.T, job *jobRequest)
	}{
		{
			name:  "request-interaction-mode",
			setup: func(_ *testing.T, job *jobRequest) { job.Request.InteractionMode = InteractionModePlan },
		},
		{
			name:  "job-interaction-mode",
			setup: func(_ *testing.T, job *jobRequest) { job.InteractionMode = InteractionModePlan },
		},
		{
			name:  "env-interaction-mode",
			setup: func(t *testing.T, _ *jobRequest) { t.Setenv(EnvInteractionMode, InteractionModePlan) },
		},
	}
	for _, tc := range sources {
		t.Run(tc.name, func(t *testing.T) {
			parent := baseParent
			tc.setup(t, &parent)
			child := parent
			child.Request.ThreadID = "th_child"
			child.Request.ParentTaskID = "task_plan"
			for role, prompt := range map[string]string{
				"parent": workerInstructions(parent),
				"child":  workerInstructions(child),
			} {
				for _, want := range []string{
					"Plan Mode (Read-Only Deep Investigation)",
					"configuration & data sources",
					"form / UI / handler state & validation",
					"submission, side effects & persistence",
				} {
					if !strings.Contains(prompt, want) {
						t.Fatalf("%s prompt in plan mode missing %q", role, want)
					}
				}
			}
		})
	}
}

func TestWriteWorkerInstructionFiles(t *testing.T) {
	t.Run("parent", func(t *testing.T) {
		jobDir := t.TempDir()
		job := testInstructionJob()
		instructionsPath, err := writeWorkerInstructionFiles(job, jobDir)
		if err != nil {
			t.Fatalf("writeWorkerInstructionFiles parent: %v", err)
		}
		wantVisualPath := filepath.Join(jobDir, visualBlocksFileName)
		wantOrchestrationPath := filepath.Join(jobDir, orchestrationFileName)
		wantCLIPath := filepath.Join(jobDir, cliFallbackFileName)
		if instructionsPath != filepath.Join(jobDir, workerInstructionsFileName) {
			t.Fatalf("unexpected instructions path %q", instructionsPath)
		}
		for _, path := range []string{instructionsPath, wantVisualPath, wantOrchestrationPath, wantCLIPath} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat %s: %v", path, err)
			}
			if perm := info.Mode().Perm(); perm != 0o600 {
				t.Fatalf("unexpected permissions for %s: %o", path, perm)
			}
		}
		instructions := readFile(t, instructionsPath)
		if !strings.Contains(instructions, wantVisualPath) || !strings.Contains(instructions, wantOrchestrationPath) {
			t.Fatalf("instructions.md missing exact on-demand paths (%q, %q):\n%s", wantVisualPath, wantOrchestrationPath, instructions)
		}
		for _, forbidden := range []string{"run_parent", "att_parent", wantCLIPath} {
			if strings.Contains(instructions, forbidden) {
				t.Fatalf("instructions.md should not embed %q:\n%s", forbidden, instructions)
			}
		}
		orchestration := readFile(t, wantOrchestrationPath)
		for _, want := range []string{wantCLIPath, wantVisualPath, "vantry_thread_transfer", "vantry_thread_manage", "Promise.allSettled"} {
			if !strings.Contains(orchestration, want) {
				t.Fatalf("orchestration.md missing %q", want)
			}
		}
		visuals := readFile(t, wantVisualPath)
		for _, want := range []string{"Memcached", "Latency p95", "POST /v1/tasks", "```svg", "```html", "stacked100", "sankey-beta"} {
			if !strings.Contains(visuals, want) {
				t.Fatalf("visual-blocks.md missing %q", want)
			}
		}
		cliFallback := readFile(t, wantCLIPath)
		for _, want := range []string{"ctl --data-dir", "--parent-run 'run_parent'", "--parent-attempt 'att_parent'"} {
			if !strings.Contains(cliFallback, want) {
				t.Fatalf("cli-fallback.md missing %q", want)
			}
		}
	})

	t.Run("child", func(t *testing.T) {
		jobDir := t.TempDir()
		job := testInstructionJob()
		job.Request.ThreadID = "th_child"
		job.Request.ParentTaskID = "task_123"
		instructionsPath, err := writeWorkerInstructionFiles(job, jobDir)
		if err != nil {
			t.Fatalf("writeWorkerInstructionFiles child: %v", err)
		}
		if _, err := os.Stat(instructionsPath); err != nil {
			t.Fatalf("missing child instructions.md: %v", err)
		}
		for _, forbidden := range []string{
			filepath.Join(jobDir, visualBlocksFileName),
			filepath.Join(jobDir, orchestrationFileName),
			filepath.Join(jobDir, cliFallbackFileName),
		} {
			if _, err := os.Stat(forbidden); !os.IsNotExist(err) {
				t.Fatalf("child job should not create %s (err=%v)", forbidden, err)
			}
		}
	})
}

func TestWriteWorkerInstructionFilesTurnStability(t *testing.T) {
	dataDir := t.TempDir()
	jobDir1 := filepath.Join(dataDir, "jobs", "att-1")
	jobDir2 := filepath.Join(dataDir, "jobs", "att-2")

	turn1 := testInstructionJob()
	turn1.DataDir = dataDir
	turn1.SocketPath = filepath.Join(dataDir, "core.sock")
	turn1.Request.ThreadID = "th_stable"
	turn1.Request.RunID = "run-1"
	turn1.Request.AttemptID = "att-1"

	turn2 := turn1
	turn2.Request.RunID = "run-2"
	turn2.Request.AttemptID = "att-2"

	inst1, err := writeWorkerInstructionFiles(turn1, jobDir1)
	if err != nil {
		t.Fatalf("turn 1 writeWorkerInstructionFiles: %v", err)
	}
	inst2, err := writeWorkerInstructionFiles(turn2, jobDir2)
	if err != nil {
		t.Fatalf("turn 2 writeWorkerInstructionFiles: %v", err)
	}

	if got1, got2 := readFile(t, inst1), readFile(t, inst2); got1 != got2 {
		t.Fatalf("instructions.md changed across turns on same thread:\n--- turn 1 ---\n%s\n--- turn 2 ---\n%s", got1, got2)
	}
	wantVisual := filepath.Join(dataDir, instructionsDirName, visualBlocksFileName)
	wantOrch := filepath.Join(dataDir, instructionsDirName, "th_stable", orchestrationFileName)
	wantCLI := filepath.Join(dataDir, instructionsDirName, "th_stable", cliFallbackFileName)
	for _, path := range []string{wantVisual, wantOrch, wantCLI} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected stable reference file %s: %v", path, err)
		}
	}
	if cli := readFile(t, wantCLI); !strings.Contains(cli, "--parent-run 'run-2' --parent-attempt 'att-2'") {
		t.Fatalf("cli-fallback.md did not refresh for turn 2:\n%s", cli)
	}
}

func TestWorkerPiEnvironmentParentTaskID(t *testing.T) {
	t.Setenv(EnvParentTaskID, "leaked_parent_task")

	parentJob := testInstructionJob()
	for _, entry := range workerPiEnvironment(parentJob, t.TempDir()) {
		if strings.HasPrefix(entry, EnvParentTaskID+"=") {
			t.Fatalf("parent worker env leaked %q", entry)
		}
	}

	childJob := parentJob
	childJob.Request.ParentTaskID = "task_123"
	foundChildEnv := false
	for _, entry := range workerPiEnvironment(childJob, t.TempDir()) {
		if entry == EnvParentTaskID+"=task_123" {
			foundChildEnv = true
		}
	}
	if !foundChildEnv {
		t.Fatal("child worker env missing VANTRY_PARENT_TASK_ID=task_123")
	}
}

func TestWorkerPiEnvironmentDirectOrchestration(t *testing.T) {
	t.Setenv(EnvDirectOrchestration, "1")

	normalJob := testInstructionJob()
	for _, entry := range workerPiEnvironment(normalJob, t.TempDir()) {
		if strings.HasPrefix(entry, EnvDirectOrchestration+"=") {
			t.Fatalf("normal worker env leaked %q", entry)
		}
	}

	childJob := normalJob
	childJob.Request.ParentTaskID = "task_child_1"
	for _, entry := range workerPiEnvironment(childJob, t.TempDir()) {
		if strings.HasPrefix(entry, EnvDirectOrchestration+"=") {
			t.Fatalf("delegated child worker env must not set %q", entry)
		}
	}
}
