package pi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/git"
	"github.com/muq9738/Vantry/internal/adapters/workspace"
	"github.com/muq9738/Vantry/internal/hostfs"
	"github.com/muq9738/Vantry/internal/hostprocess"
	"github.com/muq9738/Vantry/pkg/api"
	"github.com/muq9738/Vantry/pkg/provider"
)

// RunWorker is the implementation behind `vantry provider-worker --job-dir`.
// It never connects to or writes Core's database; all state stays in jobDir.
func RunWorker(ctx context.Context, jobDir string) error {
	absolute, err := filepath.Abs(jobDir)
	if err != nil {
		return err
	}
	lock, acquired, err := tryLock(filepath.Join(absolute, "worker.lock"))
	if err != nil {
		return err
	}
	if !acquired {
		return errors.New("provider worker is already running for this attempt")
	}
	defer unlock(lock)
	resultExists, err := fileExists(filepath.Join(absolute, "result.json"))
	if err != nil {
		return err
	}
	if resultExists {
		return nil
	}
	job, found, err := readJobRequest(absolute)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("provider worker request is missing")
	}
	var started startedMarker
	startedExists, err := readJSONFile(filepath.Join(absolute, "started.json"), &started)
	if err != nil {
		return err
	}
	if !startedExists || started.AttemptID != job.Request.AttemptID {
		return errors.New("provider worker launch intent is missing or mismatched; prompt will not be submitted")
	}
	// A worker is single-use even when it died before its first stream event.
	// Only the observer may clean up an exited worker; a new invocation must
	// never submit the original prompt or race its orphaned Pi execution.
	if claimed, err := fileExists(filepath.Join(absolute, "worker.json")); err != nil {
		return err
	} else if claimed {
		return errors.New("provider worker already claimed this attempt; prompt will not be resent")
	}
	attemptID := job.Request.AttemptID
	result := jobResult{AttemptID: attemptID, Status: "failed", UpdatedAt: time.Now().UTC()}
	defer func() {
		if result.Error != "" {
			result.Status = "failed"
		}
		if stopped, _ := fileExists(filepath.Join(absolute, "stop.json")); stopped && result.Status != "completed" {
			result.Status, result.Error = "cancelled", ""
		}
		last, controlErr := finalizeControls(absolute, "Pi attempt ended before control acknowledgement; control will not be resent")
		if controlErr != nil {
			result.Status, result.Error = "failed", controlErr.Error()
		} else {
			result.LastSequence = last
		}
		result.UpdatedAt = time.Now().UTC()
		_ = writeJSONFile(filepath.Join(absolute, "result.json"), result)
	}()
	// Observe cancellation during workspace preparation and lock acquisition as
	// well as Pi execution. A stopped job must not wait behind another writer.
	workerCtx, stopWorker := context.WithCancel(ctx)
	ctx = workerCtx
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(40 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				if stopped, _ := fileExists(filepath.Join(absolute, "stop.json")); stopped {
					stopWorker()
					return
				}
			}
		}
	}()
	defer func() { stopWorker(); <-monitorDone }()
	if err := writeJSONFile(filepath.Join(absolute, "worker.json"), map[string]any{
		"attemptId": attemptID, "pid": os.Getpid(), "updatedAt": time.Now().UTC(),
	}); err != nil {
		result.Error = err.Error()
		return err
	}
	if stopped, err := fileExists(filepath.Join(absolute, "stop.json")); err != nil {
		result.Error = err.Error()
		return err
	} else if stopped {
		result.Status = "cancelled"
		return context.Canceled
	}

	if job.Request.Workspace == nil {
		err := errors.New("provider worker workspace is missing")
		result.Error = err.Error()
		return err
	}
	prepared, err := workspace.Prepare(ctx, job.DataDir, *job.Request.Workspace, job.Request.ThreadID)
	if err != nil {
		result.Error = fmt.Sprintf("prepare pi workspace: %v", err)
		return err
	}
	capturePreRunCheckpoint(ctx, job, prepared.Path)
	workspaceLock, err := workspace.LockDirectory(ctx, prepared.Path)
	if err != nil {
		result.Error = fmt.Sprintf("lock pi workspace: %v", err)
		return err
	}
	defer workspace.Unlock(workspaceLock)
	if stopped, err := fileExists(filepath.Join(absolute, "stop.json")); err != nil {
		result.Error = err.Error()
		return err
	} else if stopped {
		result.Status = "cancelled"
		return context.Canceled
	}

	sessionsDir := filepath.Join(job.DataDir, "sessions")
	if err := makePrivateDir(sessionsDir); err != nil {
		result.Error = err.Error()
		return err
	}
	sessionPath := filepath.Join(sessionsDir, job.Request.ThreadID+".jsonl")
	journal, err := openJournal(filepath.Join(absolute, "events.jsonl"))
	if err != nil {
		result.Error = err.Error()
		if last, sequenceErr := journalLastSequence(absolute); sequenceErr == nil {
			result.LastSequence = last
		}
		return err
	}
	defer journal.Close()
	if job.Request.ProviderID != "" && job.Request.ProviderID != ProviderPI {
		err = fmt.Errorf("unsupported provider %q", job.Request.ProviderID)
	} else {
		result.LastSequence, err = executePi(ctx, job, absolute, prepared, sessionPath, journal, workspaceLock)
	}
	if err != nil {
		result.Error = err.Error()
		if errors.Is(err, context.Canceled) {
			result.Status = "cancelled"
			result.Error = ""
		}
		return err
	}
	result.Status = "completed"
	return nil
}

func executePi(ctx context.Context, job jobRequest, jobDir string, prepared api.WorkspaceSpec, sessionPath string, journal *eventJournal, workspaceLock *os.File) (int64, error) {
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	model := job.Request.Model
	args := []string{"--mode", "rpc", "--session", sessionPath, "--model", model}
	if providerName, _, ok := strings.Cut(model, "/"); ok && providerName != "" {
		args = append(args, "--provider", providerName)
	}
	instructionsPath, err := writeWorkerInstructionFiles(job, jobDir)
	if err != nil {
		return journal.sequence, err
	}
	if !effectiveUserExtensions(job) {
		args = append(args, "--no-extensions")
	}
	if !effectiveUserMCP(job) {
		args = append(args, "--no-mcp")
	} else if !effectiveUserExtensions(job) {
		// --no-extensions disables all built-ins too, so re-enable builtin:mcp
		// explicitly when user MCP stays enabled.
		args = append(args, "--extension", "builtin:mcp")
	}
	// These Pi built-ins are loaded explicitly even when automatic user
	// extensions are disabled. Add tools without replacing the user's defaults.
	args = append(args, "--extension", "builtin:codemode", "--extension", "builtin:tool-search", "--tools", "+codemode,+tool_search")
	if effectiveUserExtensions(job) && !effectiveNativeSubagents(job) {
		// User extensions stay loaded, but their child-agent tools do not:
		// a pi-subagents run is invisible to Core and cannot wake this thread
		// after Pi exits at the end of the turn. Vantry delegation replaces it.
		args = append(args, "--exclude-tools", nativeSubagentTools)
	}
	args = append(args, "--append-system-prompt", instructionsPath)
	vantryToolsPath := filepath.Join(jobDir, "vantry_tools.ts")
	if err := os.WriteFile(vantryToolsPath, vantryToolsExtension, 0o600); err != nil {
		return journal.sequence, err
	}
	args = append(args, "--extension", vantryToolsPath)
	if requiresApprovalExtension(job) {
		extensionPath := filepath.Join(jobDir, "approval.ts")
		if err := os.WriteFile(extensionPath, approvalExtension, 0o600); err != nil {
			return journal.sequence, err
		}
		args = append(args, "--extension", extensionPath)
	}
	execName, execArgs := job.PiExecutable, args
	env := workerPiEnvironment(job, jobDir)
	if job.SandboxMode == string(hostprocess.SandboxWorkspaceWrite) && hostprocess.SandboxSupported() {
		var extraWritable []string
		for _, dir := range []string{jobDir, filepath.Dir(sessionPath), piAgentDirectory()} {
			if strings.TrimSpace(dir) != "" {
				extraWritable = append(extraWritable, dir)
			}
		}
		wrappedExe, wrappedArgs, wrapErr := hostprocess.WrapSandboxArgs(hostprocess.SandboxConfig{
			Mode:              hostprocess.SandboxWorkspaceWrite,
			WorkspaceDir:      prepared.Path,
			ExtraWritableDirs: extraWritable,
			ProtectGitMeta:    true,
			AllowTempDirs:     true,
			AllowNetwork:      true,
		}, execName, execArgs)
		if wrapErr != nil {
			return journal.sequence, wrapErr
		}
		execName, execArgs = wrappedExe, wrappedArgs
		if !hasEnvVar(env, "GOCACHE") {
			env = append(env, "GOCACHE="+filepath.Join(os.TempDir(), "vantry-gocache"))
		}
	}
	command, err := newPiCommand(workerCtx, execName, execArgs, jobDir, job.Request.AttemptID, workspaceLock)
	if err != nil {
		return journal.sequence, err
	}
	defer command.Close()
	command.Dir = prepared.Path
	command.Env = env
	command.WaitDelay = 5 * time.Second
	stdin, err := command.StdinPipe()
	if err != nil {
		return journal.sequence, err
	}
	defer stdin.Close()
	stdout, err := command.StdoutPipe()
	if err != nil {
		return journal.sequence, err
	}
	stderr, err := os.OpenFile(filepath.Join(jobDir, "pi.stderr.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return journal.sequence, err
	}
	defer stderr.Close()
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return journal.sequence, fmt.Errorf("start pi: %w", err)
	}
	processStart, identityErr := processIdentity(command.Process.Pid)
	if identityErr != nil {
		_ = command.Cancel()
		_ = command.Wait()
		return journal.sequence, identityErr
	}
	if err := writeJSONFile(filepath.Join(jobDir, "pi-process.json"), piProcess{
		AttemptID: job.Request.AttemptID, PID: command.Process.Pid, ProcessStart: processStart,
	}); err != nil {
		_ = command.Cancel()
		_ = command.Wait()
		return journal.sequence, err
	}

	state, streamErr := runRPC(workerCtx, job, jobDir, stdin, stdout, journal)
	if streamErr != nil {
		cancel()
	}
	waitErr := command.Wait()
	cancel()
	stopped, stopErr := fileExists(filepath.Join(jobDir, "stop.json"))
	if stopErr != nil {
		return journal.sequence, stopErr
	}
	if streamErr != nil {
		return journal.sequence, streamErr
	}
	if stopped && (waitErr != nil || state.aborted || !state.settled) {
		return journal.sequence, context.Canceled
	}
	if ctx.Err() != nil {
		return journal.sequence, context.Canceled
	}
	if state.failure != "" {
		return journal.sequence, errors.New(state.failure)
	}
	if waitErr != nil {
		return journal.sequence, fmt.Errorf("pi process failed: %w", waitErr)
	}
	if state.aborted {
		return journal.sequence, errors.New("pi run aborted")
	}
	if !state.settled {
		return journal.sequence, errors.New("pi exited successfully without agent_settled; treating the turn as failed")
	}
	return journal.sequence, nil
}

const (
	workerInstructionsFileName = "instructions.md"
	visualBlocksFileName       = "visual-blocks.md"
	orchestrationFileName      = "orchestration.md"
	cliFallbackFileName        = "cli-fallback.md"
	instructionsDirName        = "instructions"
)

// visualReplyInstructions holds the full on-demand syntax reference written to
// visual-blocks.md in jobDir so main-thread turns avoid paying its token cost
// until a structured visual block is actually needed.
const visualReplyInstructions = `## Native Visuals & Structured Replies (MyGo GPU + Sandboxed WKWebView)

Vantry renders structured Markdown fenced blocks directly in-app (L1 native GPU charts & math + L2 offline sandboxed inline WKWebView previews for full Mermaid, SVG, and interactive HTML). NEVER start a local HTTP server or fetch external CDNs just to show visuals.

Proactively include visual blocks whenever they clarify the answer (and always when the user asks for charts, comparisons, architecture, flows, interactive prototypes, or visual summaries). Put the core conclusion first; do not restate every number already shown in a visual block. Simple one-line factual answers stay plain text.

Choose the fenced block by the shape of the information:

1. **Trends, numeric comparisons, benchmarks, scatter/combo/heatmaps, shares, breakdowns** -> ` + "```chart" + `
   - Compact pipe-table form (preferred for brevity):
     ` + "```chart bar unit=ms title=\"延迟对比\"" + `
                | p50 | p95
     * Redis    | 12  | 40
     Memcached  | 9   | 31
     Postgres   | 28  | 95
     ` + "```" + `
     Types: ` + "`bar`" + ` (add ` + "`horizontal`" + `, ` + "`stacked`" + `, or ` + "`100%`" + `), ` + "`line`" + ` / ` + "`area`" + ` (auto ISO date/time axes), ` + "`pie`" + `, ` + "`donut`" + `, ` + "`combo`" + `, ` + "`heatmap`" + `, ` + "`breakdown`" + `. Multi-series ` + "`hbar`" + ` is supported. Prefix a row with ` + "`*`" + ` to highlight it; add ` + "`ref=100`" + ` or ` + "`unitRight=%`" + ` when useful.
   - Or Studio JSON form: ` + "`{\"type\":\"bar|stacked|stacked100|hbar|line|area|pie|donut|scatter|bubble|combo|heatmap|breakdown|stats\",\"title\":\"...\",\"hero\":\"...\",\"heroLabel\":\"...\",\"badge\":\"...\",\"badgeTone\":\"success|warning|danger|accent|neutral\",\"delta\":\"...\",\"style\":\"dots|tiles\",\"unit\":\"ms\",\"unitRight\":\"%\",\"labels\":[...],\"refLines\":[{\"value\":100,\"label\":\"SLA\"}],\"series\":[{\"name\":\"...\",\"subtitle\":\"...\",\"badge\":\"...\",\"kind\":\"bar|line|area\",\"axis\":\"left|right\",\"data\":[...],\"sparkline\":[...]}]}`" + `
     - Use Studio Hero KPI fields (` + "`hero`" + `, ` + "`heroLabel`" + `, ` + "`badge`" + `, ` + "`badgeTone`" + `, ` + "`delta`" + `) when summarizing a primary figure at the top of a metric card.
     - Use ` + "`\"type\":\"breakdown\"`" + ` for ranked distributions (renders a top proportional bar + rows with color dot, ` + "`subtitle`" + `, thin proportion rail, ` + "`sparkline`" + ` trend, total & ` + "`badge`" + `).
     - ` + "`\"type\":\"heatmap\"`" + ` defaults to ` + "`\"style\":\"dots\"`" + ` dot-matrix (` + "`null`" + ` renders a dashed hollow ring for missing slots, ` + "`0`" + ` a neutral muted dot, positive values a 4-tier thermal scale); set ` + "`\"style\":\"tiles\"`" + ` for filled rectangle cells.
     - ` + "`scatter`" + `/` + "`bubble`" + ` use ` + "`points:[{\"x\":1,\"y\":2,\"r\":6,\"label\":\"...\"}]`" + `; ` + "`stats`" + ` uses ` + "`items:[{\"label\":\"...\",\"value\":\"1.2s\",\"delta\":\"+12%\",\"note\":\"...\"}]`" + `.

2. **Architecture, flows, sequences, state machines, ER/class diagrams, Gantt, mindmaps, Sankey, XY charts**:
   - Standard ` + "```mermaid" + ` supports ALL Mermaid diagram types (` + "`flowchart`" + `, ` + "`sequenceDiagram`" + `, ` + "`stateDiagram-v2`" + `, ` + "`erDiagram`" + `, ` + "`classDiagram`" + `, ` + "`gantt`" + `, ` + "`mindmap`" + `, ` + "`gitGraph`" + `, ` + "`pie`" + `, ` + "`xychart-beta`" + `, ` + "`sankey-beta`" + `, ` + "`quadrantChart`" + `, ` + "`architecture-beta`" + `) with full ` + "`classDef`" + ` styling, nested ` + "`subgraph`" + `, pan/zoom, and SVG/PNG export.
   - Concise aliases also work: ` + "```flow [LR]" + `, ` + "```state [LR]" + `, ` + "```sequence [num]" + `, ` + "```er" + `, ` + "```class" + `.

3. **Custom vector diagrams & interactive prototypes** -> ` + "```svg" + ` / ` + "```html" + ` / ` + "```artifact" + `
   - Use ` + "```svg" + ` (` + "`title=\"...\"`" + `) for bespoke technical illustrations with ` + "`<text>`" + `, ` + "`<marker>`" + ` arrows, gradients, and exact geometry.
   - Use ` + "```html" + ` or ` + "```artifact" + ` (` + "`title=\"...\"`" + `) for self-contained offline interactive widgets rendered inline in a live sandboxed frame and expandable into Canvas.
   - **Studio HTML Craft Rules**:
     - Style exclusively with the injected Studio CSS variables (` + "`--v-bg`" + `, ` + "`--v-surface`" + `, ` + "`--v-surface-2`" + `, ` + "`--v-ink`" + `, ` + "`--v-muted`" + `, ` + "`--v-line`" + `, ` + "`--v-accent`" + `, ` + "`--v-good`" + `, ` + "`--v-warn`" + `, ` + "`--v-bad`" + `) so Light & Dark themes stay native, plus ` + "`.tnum`" + ` (` + "`font-variant-numeric: tabular-nums`" + `) on all numeric readouts.
     - Lead with a clear hierarchy (Hero KPI + compact controls + main visual), use 12–16px surface radii with subtle 1px hairline borders (` + "`var(--v-line)`" + `), and provide real interactive affordances (filters, tabs, sliders, hover cross-dimming, live readouts).
     - Keep zero external URLs/CDNs/fetches and avoid generic SaaS purple neon glow or unnecessary box-in-box clutter.

4. **Mathematical equations**:
   - Inline math ` + "`$E = mc^2$`" + ` and display blocks ` + "`$$\\sum_{i=1}^{n} x_i$$`" + ` render natively (with KaTeX in snapshots/canvas).

5. **Directory layout, module hierarchy & refactor change plans** -> ` + "```tree" + `
   - Indentation sets depth; ` + "`name | short note`" + `; prefix changed lines with ` + "`+ `" + ` (added), ` + "`- `" + ` (removed), or ` + "`~ `" + ` (modified) to enable interactive Changes / Before / After switching.

6. **Phases, milestones, incident or release chronology** -> ` + "```timeline [h|v]" + `
   - ` + "`time | title | optional detail`" + `; prefix ` + "`*`" + ` on the current/key milestone.

7. **Quotas, budgets, SLA / capacity thresholds** -> ` + "```limits" + `
   - ` + "`Latency p95 | 142 / 200 | ms`" + ` (renders a 20-segment discrete pill gauge, circular ` + "`%`" + ` completion ring, and automatic green/amber/red semantic status pill).

8. **Metadata / spec header** -> ` + "```kv [cols=2]" + ` (` + "`Key: Value`" + `, ` + "`* Wide Key: Value`" + `).
9. **Key takeaway / risk / warning** -> ` + "```callout <info|ok|warn|err> Title" + ` with Markdown body.
10. **HTTP / RPC interface contracts** -> ` + "```api" + ` (` + "`POST /v1/tasks`" + `, indented ` + "`request`" + ` / ` + "`response 200`" + ` and ` + "`field type required | description`" + `).
11. **Interactive multi-tab cards / sortable tables / action buttons** -> ` + "```vantry-ui" + ` JSON (` + "`metrics`" + `, ` + "`table`" + `, ` + "`tabs`" + `, ` + "`steps`" + `, ` + "`kv`" + `, ` + "`progress`" + `, ` + "`actions`" + `).
12. **Structured user decision** -> ` + "```ask [multi]" + ` (question on line 1, then ` + "`* Recommended option | note`" + ` and ` + "`- Alternative | note`" + `; the user can click to reply directly in Vantry).
13. **Semi-automatic orchestration confirmation** -> ` + "```dispatch" + ` (optional title on line 1, then ` + "`* Task title | role=research|design|implementation|test|review|general | model=<model-id> | count=1 | isolation=shared|worktree | short note`" + `; prefix ` + "`* `" + ` for checked or ` + "`- `" + ` for unchecked. Users can toggle tasks, adjust parallel worker counts, switch models from the local catalog, and click Confirm to launch or Run directly).`

type workerInstructionPaths struct {
	visual        string
	orchestration string
	cliFallback   string
}

func isSubpath(base, target string) bool {
	absBase, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absBase, absTarget)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

func safeThreadSegment(threadID string) string {
	trimmed := strings.TrimSpace(threadID)
	if trimmed != "" && validID(trimmed) == nil {
		return trimmed
	}
	return "default"
}

// resolveWorkerReferencePaths places parent reference files under a
// thread-stable directory (<dataDir>/instructions/...) when jobDir lives under
// DataDir so instructions.md stays byte-for-byte identical across turns of a
// resumed Pi session. Standalone callers with external temp dirs stay hermetic
// inside jobDir.
func resolveWorkerReferencePaths(job jobRequest, jobDir string) workerInstructionPaths {
	if strings.TrimSpace(jobDir) == "" {
		return workerInstructionPaths{
			visual:        visualBlocksFileName,
			orchestration: orchestrationFileName,
			cliFallback:   cliFallbackFileName,
		}
	}
	if trimmedDataDir := strings.TrimSpace(job.DataDir); trimmedDataDir != "" && isSubpath(trimmedDataDir, jobDir) {
		instDir := filepath.Join(trimmedDataDir, instructionsDirName)
		threadDir := filepath.Join(instDir, safeThreadSegment(job.Request.ThreadID))
		return workerInstructionPaths{
			visual:        filepath.Join(instDir, visualBlocksFileName),
			orchestration: filepath.Join(threadDir, orchestrationFileName),
			cliFallback:   filepath.Join(threadDir, cliFallbackFileName),
		}
	}
	return workerInstructionPaths{
		visual:        filepath.Join(jobDir, visualBlocksFileName),
		orchestration: filepath.Join(jobDir, orchestrationFileName),
		cliFallback:   filepath.Join(jobDir, cliFallbackFileName),
	}
}

func writePrivateFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := makePrivateDir(dir); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	tmpPath := file.Name()
	defer os.Remove(tmpPath)
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return hostfs.RenameDurable(tmpPath, path)
}

func writeWorkerInstructionFiles(job jobRequest, jobDir string) (string, error) {
	if !isDelegatedChild(job) {
		refs := resolveWorkerReferencePaths(job, jobDir)
		if err := writePrivateFileAtomic(refs.visual, []byte(visualReplyInstructions)); err != nil {
			return "", err
		}
		if err := writePrivateFileAtomic(refs.orchestration, []byte(orchestrationReferenceInstructions(job, refs))); err != nil {
			return "", err
		}
		if err := writePrivateFileAtomic(refs.cliFallback, []byte(cliFallbackInstructions(job))); err != nil {
			return "", err
		}
	}
	instructionsPath := filepath.Join(jobDir, workerInstructionsFileName)
	if err := writePrivateFileAtomic(instructionsPath, []byte(workerInstructionsWithDir(job, jobDir))); err != nil {
		return "", err
	}
	return instructionsPath, nil
}

func isDelegatedChild(job jobRequest) bool {
	return strings.TrimSpace(job.Request.ParentTaskID) != ""
}

func workerInstructions(job jobRequest) string {
	return workerInstructionsWithDir(job, "")
}

func workerInstructionsWithDir(job jobRequest, jobDir string) string {
	var body string
	if isDelegatedChild(job) {
		body = childDelegationInstructions(job)
	} else {
		refs := resolveWorkerReferencePaths(job, jobDir)
		body = mainWorkerInstructions(job, refs) + "\n\n" + visualReplyIndex(refs.visual)
	}
	if plan := planModeInstructions(job); plan != "" {
		return plan + "\n\n" + body
	}
	return body
}

func planModeInstructions(job jobRequest) string {
	if effectiveInteractionMode(job) != InteractionModePlan {
		return ""
	}
	return `## Plan Mode (Read-Only Deep Investigation)

- You are in read-only Plan mode (` + "`interaction_mode == \"plan\"`" + `): write/edit tools are blocked and mutating shell commands require explicit approval. Investigate strictly read-only without modifying files or state.
- Trace the complete chain before concluding or proposing edits: **configuration & data sources** (defaults, env/config files, loaders, persistence/API state) -> **form / UI / handler state & validation** (parsing, normalization, error paths, edge cases) -> **submission, side effects & persistence** (dispatch, IPC/RPC, storage, rollback/refresh).
- Never stop at surface symptoms or guess unread layers; inspect the concrete files and symbols across every stage first.`
}

func visualReplyIndex(refPath string) string {
	return fmt.Sprintf(`## Native Visuals & Structured Replies (On-Demand)

Vantry renders native GPU blocks & offline sandboxed inline previews—NEVER start a local HTTP server or load external CDNs for visuals. Simple factual answers stay plain text; lead with conclusions.
Triggers: charts (`+"`chart`"+`); full diagrams (`+"`mermaid`"+`, `+"`flow`"+`, `+"`state`"+`, `+"`sequence`"+`, `+"`er`"+`, `+"`class`"+`); custom vector/interactive prototypes (`+"`svg`"+`, offline `+"`html`"+`/`+"`artifact`"+`); math (`+"`$...$`"+`, `+"`$$...$$`"+`); `+"`tree`"+`, `+"`timeline`"+`, `+"`limits`"+`, `+"`kv`"+`, `+"`callout`"+`, `+"`api`"+`, `+"`vantry-ui`"+`, `+"`ask`"+`, `+"`dispatch`"+`.
Before emitting any visual or `+"`dispatch`"+` block, read %s for exact syntax and schemas.`, refPath)
}

func cliFallbackInstructions(job jobRequest) string {
	executable := shellQuote(job.Executable)
	dataDir := shellQuote(job.DataDir)
	request := job.Request
	return fmt.Sprintf(`## CLI Fallback (Only If Native vantry_* Tools Are Unavailable)

%s ctl --data-dir %s delegate --parent-run %s --parent-attempt %s --message 'clear child instructions' --provider pi --resume-policy auto
Add --title 'child title' and --model provider/model when needed. The parent attempt ID prevents stale callbacks.

Useful fallback follow-up commands:
%s ctl --data-dir %s status
%s ctl --data-dir %s followup --task TASK_ID --message 'follow-up instructions'
%s ctl --data-dir %s ack --parent-run %s --generation N --delivery DELIVERY_MESSAGE_ID
Use the same --data-dir for every command. Do not write Core's database or job journals directly.`,
		executable, dataDir, shellQuote(request.RunID), shellQuote(request.AttemptID),
		executable, dataDir, executable, dataDir, executable, dataDir, shellQuote(request.RunID),
	)
}

// childDelegationInstructions keeps delegated child prompts compact to save
// input tokens while preserving shared codemode/tool_search and secret safety.
func childDelegationInstructions(job jobRequest) string {
	mcpNote := ""
	if effectiveUserMCP(job) {
		mcpNote = "\n- External MCP servers use codemode by default: discover tools with describeNamespace(\"mcp__<server>\") / searchTools, call tools.mcp__<server>__<tool>(...), and filter/trim large responses inside the script before calling text()."
	}
	return fmt.Sprintf(`You are a Vantry delegated child session (task %s, thread %s).
- Complete the assigned task directly; do not spawn further subagents unless explicitly instructed.
- For ordinary multi-file read / grep / find / ls, call native tools directly in parallel—do NOT wrap routine file reads or greps in codemode.
- Use codemode only when scripting/filtering large JSON or large lists (e.g. list_projects, large API/MCP payloads) or batching multi-subtask orchestration (Promise.allSettled), and always filter/trim data inside the script before calling text(). Use tool_search for deferred tools.%s
- Return a concise summary of results, changed file paths, and verification evidence (avoid dumping raw logs or redundant visual blocks).
- For credentials/passwords, call vantry_request_secret; never ask for plaintext secrets or print secret-file contents.`,
		job.Request.ParentTaskID, job.Request.ThreadID, mcpNote,
	)
}

func mcpCodemodeGuidance(job jobRequest) string {
	if !effectiveUserMCP(job) {
		return ""
	}
	return `
External MCP (codemode default): check <mcp_servers>, discover schemas via describeNamespace("mcp__<server>") / searchTools, call tools.mcp__<server>__<tool>(...), and filter/trim large payloads before text().`
}

func mainWorkerInstructions(job jobRequest, refs workerInstructionPaths) string {
	ecosystemNote := "User extensions and MCP are disabled unless enabled; Vantry provides native orchestration/secret tools and optional approval gating."
	if effectiveUserExtensions(job) || effectiveUserMCP(job) {
		ecosystemNote = fmt.Sprintf(
			"User Pi compatibility: extensions=%t, mcp=%t (codemode default); Vantry provides native orchestration/secret tools and optional approval gating.",
			effectiveUserExtensions(job), effectiveUserMCP(job),
		)
	}
	return fmt.Sprintf(`You are executing inside Vantry's durable Pi session worker. %s%s

Workspace & delegation:
- Ordinary coding tasks need no Vantry orchestration tools. Work directly in this workspace.
%s
- Read %s before delegating, showing a `+"```dispatch"+` card, managing peer threads, or targeting another machine (remote environmentId, low-frequency tools, thread links, CLI fallback).
- On orchestration/model error, quote it and stop. Never fall back to native subagents, scripted workflows, other CLIs, or another model. Run a scripted workflow only when the user asks for one.
- Secrets: call vantry_request_secret directly (0600 file; scripts cannot invoke it).

Tools & codemode:
- For ordinary multi-file read / grep / find / ls, call native tools directly in parallel—do NOT wrap routine file reads or greps in codemode.
- Web/HTTP: inspect status, headers, and charset (e.g. GBK/GB18030) via short curl/python3 bash scripts and extract only needed text—never dump raw HTML.
- Use codemode only to filter large JSON/lists (e.g. list_projects, large API/MCP payloads), batch multi-subtask orchestration (Promise.allSettled), or call codemode MCP/Vantry tools; filter/trim inside the script before text().
- Vantry context: thread=%s`,
		ecosystemNote, mcpCodemodeGuidance(job), nativeSubagentPolicy(job), refs.orchestration, job.Request.ThreadID,
	)
}

func orchestrationReferenceInstructions(job jobRequest, refs workerInstructionPaths) string {
	return fmt.Sprintf(`## Vantry Multi-Agent Orchestration & Peer Sessions

%s
- Ordinary sessions defer orchestration tools by default: call tools.vantry_* directly in codemode or discover them with tool_search.
- Delegate only when requested or when clearly multi-part work benefits from parallel execution; keep simple work in this thread. Run a scripted workflow only when the user asks for one.
- Dispatch confirmation: when delegation is warranted or models/roles/parallelism are specified, filter models via codemode before text() and show a `+"```dispatch"+` card (check %s for schema). Wait for confirmation unless told to dispatch immediately.
- Batch dispatch: after confirmation, launch parallel children in ONE codemode call via Promise.allSettled over tools.vantry_delegate_task(...) and trim returned fields before text(). Put shared context in one file; children share builtin codemode and tool_search.
- Keep local work local; saved machine links are no reason to move it. For another machine, query vantry_environment_list and pass environmentId to vantry_orchestrator_capabilities and vantry_delegate_task (remote defaults/paths belong to that target).
- Call vantry_orchestrator_capabilities once per thread before first delegation (again only after an error or user-named provider change). Never switch provider/model or machine unasked.
- If vantry_orchestrator_capabilities or vantry_delegate_task fails, or the model is unavailable, quote the error and stop that path. Never fall back to native subagents, scripted workflows, other CLIs, or another model.
- Use vantry_thread_launch and vantry_create_threads only for requested top-level sessions, not subagents. When fewer or no delegates are requested, cancel extras without replacement; follow a running delegate's next instruction.
- Durable children: vantry_delegate_task defaults to async (do NOT poll vantry_task_status or sleep; wait for Core wakeup). Inspect via vantry_task_status; cancel with vantry_task_cancel, never thread interrupt.
- Low-frequency tools (discover via tool_search or codemode searchTools/describeTool/describeNamespace("vantry")): vantry_environment_list, vantry_thread_list, vantry_thread_search, vantry_thread_read, vantry_thread_send, vantry_thread_wait, vantry_thread_launch, vantry_create_threads, vantry_thread_transfer, vantry_thread_manage, vantry_task_followup, vantry_task_ack.
- Codemode orchestration rules: await all calls, keep dependent actions sequential, preserve environmentId, request IDs, approvals, and cancellation. Scripts do not replace Core's durable task lifecycle.
- Thread references: use the returned markdown Link (including ?environmentId=); local form [Title](vantry-thread://<threadId>).
- Thread context: thread=%s, socket=%s
- CLI fallback (only if native vantry_* tools are unavailable): read %s for exact vantry ctl commands. Do not write Core's database or job journals directly.`,
		nativeSubagentPolicy(job), refs.visual, job.Request.ThreadID, job.SocketPath, refs.cliFallback,
	)
}

// nativeSubagentPolicy is the delegation rule matching the tools this Pi
// process actually has. By default native child tools are excluded, so every
// child goes through Core, where it is durable, shown under its parent thread
// and can wake the parent after this Pi process exits.
func nativeSubagentPolicy(job jobRequest) string {
	if effectiveNativeSubagents(job) {
		return `- Native child tools (pi-subagents) are enabled by the user for this Core. Use them only in the foreground for same-provider, same-model work inside this turn, and wait with their native tools. This Pi process exits when the turn ends, so never start an async or background native child: it cannot wake this thread. Background, long, cross-model, isolated-checkout or other-machine work uses vantry_delegate_task.`
	}
	return `- Delegation goes through Vantry only. Pi-native child tools (subagent, subagent_supervisor, bg_wait) are disabled in Vantry sessions; do not look for or emulate them. Use vantry_delegate_task: the child is durable, shown under this thread, and Core wakes this thread when it settles. Use isolation: "worktree" for independent parallel edits.`
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

type piStreamState struct {
	settled bool
	aborted bool
	failure string
}

type piRecord struct {
	Type                  string `json:"type"`
	Aborted               bool   `json:"aborted"`
	Success               *bool  `json:"success"`
	FinalError            string `json:"finalError"`
	ErrorMessage          string `json:"errorMessage"`
	AssistantMessageEvent struct {
		Type   string          `json:"type"`
		Delta  string          `json:"delta"`
		Reason string          `json:"reason"`
		Error  json.RawMessage `json:"error"`
	} `json:"assistantMessageEvent"`
	Message struct {
		Role         string `json:"role"`
		StopReason   string `json:"stopReason"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"message"`
}

func (s *piStreamState) consume(line []byte) (provider.Event, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return provider.Event{}, fmt.Errorf("decode pi JSONL event: %w", err)
	}
	// UI records have a string message, whereas session events carry a message
	// object. Preserve the real UI record without decoding it as a session event.
	if envelope.Type == "extension_ui_request" {
		return provider.Event{Type: envelope.Type, Data: append(json.RawMessage(nil), line...)}, nil
	}
	var record piRecord
	if err := json.Unmarshal(line, &record); err != nil {
		return provider.Event{}, fmt.Errorf("decode pi JSONL event: %w", err)
	}
	if record.Type == "" {
		return provider.Event{}, errors.New("pi JSONL event has no type")
	}
	data := append(json.RawMessage(nil), line...)
	event := provider.Event{Type: record.Type, Data: data}
	if record.Type == "message_update" && record.AssistantMessageEvent.Type == "text_delta" {
		event.Type = "text_delta"
		event.Delta = record.AssistantMessageEvent.Delta
	}
	// Reasoning keeps its raw record: Core stores it beside the reply, and a
	// consumer that only reads Delta never mistakes it for reply text.
	if record.Type == "message_update" && record.AssistantMessageEvent.Type == "thinking_delta" {
		event.Type = "thinking_delta"
	}
	var failure string
	switch record.Type {
	case "agent_settled":
		s.settled = true
		s.aborted = record.Aborted
	case "message_end", "turn_end":
		if record.Message.Role == "assistant" {
			switch record.Message.StopReason {
			case "error":
				failure = nonEmpty(record.Message.ErrorMessage, "pi assistant turn failed")
			case "aborted":
				failure = "pi assistant turn aborted"
			}
		}
	case "message_update":
		if record.AssistantMessageEvent.Type == "error" {
			failure = rawError(record.AssistantMessageEvent.Error, record.AssistantMessageEvent.Reason)
		}
	case "auto_retry_end":
		if record.Success != nil {
			if *record.Success {
				// A successful retry replaces the earlier failed assistant turn.
				// agent_end alone is not authoritative because willRetry may be true.
				s.failure = ""
			} else {
				failure = nonEmpty(record.FinalError, "pi automatic retry failed")
			}
		}
	case "compaction_end":
		if record.ErrorMessage != "" {
			failure = record.ErrorMessage
		}
	}
	if failure != "" {
		// Later retry/finalization records are more authoritative than the
		// transient provider error they replace.
		s.failure = failure
		event.Type = "error"
	}
	return event, nil
}

func rawError(raw json.RawMessage, fallback string) string {
	if len(raw) > 0 && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return text
		}
		var object struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &object) == nil && object.Message != "" {
			return object.Message
		}
		const limit = 4096
		text = string(raw)
		if len(text) > limit {
			text = text[:limit] + "..."
		}
		return text
	}
	return nonEmpty(fallback, "pi provider stream failed")
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

type eventJournal struct {
	file     *os.File
	sequence int64
}

func openJournal(path string) (*eventJournal, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	journal := &eventJournal{file: file}
	// A worker is never restarted after a crash. This check also protects a
	// direct duplicate RunWorker invocation from appending a second execution.
	if info, err := file.Stat(); err != nil {
		_ = file.Close()
		return nil, err
	} else if info.Size() != 0 {
		_ = file.Close()
		return nil, errors.New("provider event journal already exists; prompt will not be resent")
	}
	return journal, nil
}

func (j *eventJournal) Append(event provider.Event) error {
	next := j.sequence + 1
	event.Sequence = next
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if _, err := j.file.Write(data); err != nil {
		return err
	}
	if err := j.file.Sync(); err != nil {
		return err
	}
	j.sequence = next
	return nil
}

func (j *eventJournal) Close() error {
	if j.file == nil {
		return nil
	}
	return j.file.Close()
}

func capturePreRunCheckpoint(ctx context.Context, job jobRequest, workspacePath string) {
	if strings.TrimSpace(os.Getenv("VANTRY_AUTO_CHECKPOINT")) == "0" {
		return
	}
	if effectiveInteractionMode(job) == InteractionModePlan || strings.TrimSpace(job.Request.RunID) == "" {
		return
	}
	if !git.IsGitWorkspace(workspacePath) {
		return
	}
	cpCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	summary := singleLineSummary(job.Request.Message, 80)
	label := "Before turn: " + summary
	if summary == "" {
		label = "Before turn: " + job.Request.RunID
	}
	_, _ = git.CaptureCheckpoint(cpCtx, workspacePath, git.RunCheckpointID(job.Request.RunID), label)
}

func singleLineSummary(text string, maxRunes int) string {
	clean := strings.Map(func(r rune) rune {
		if r == '\x00' || r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, text)
	clean = strings.Join(strings.Fields(clean), " ")
	if maxRunes <= 0 {
		return clean
	}
	runes := []rune(clean)
	if len(runes) <= maxRunes {
		return clean
	}
	return string(runes[:maxRunes]) + "..."
}

func hasEnvVar(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}
