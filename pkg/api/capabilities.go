package api

import (
	"context"
	"fmt"
	"time"
)

// Capability names advertise structured business behaviour, never a build or
// content identity. A client compares the names it needs against the list a
// Core reports, so a newer client can recognise an older Core instead of
// silently attaching to one that cannot serve it.
const (
	// CapabilityStatus means the Core answers ServerStatus: protocol, schema,
	// process, active runs and this capability list.
	CapabilityStatus = "status.v1"
	// CapabilityThreadSummaries means the Core serves per-thread summaries that
	// omit stored reply text.
	CapabilityThreadSummaries = "threads.summaries.v1"
	// CapabilityThreadDetail means the Core serves a single thread's runs, tasks,
	// inputs, tools and requests without shipping the whole snapshot.
	CapabilityThreadDetail = "threads.detail.v1"
	// CapabilityDrainShutdown means the Core can be asked to stop, and refuses
	// that request while any run is still active.
	CapabilityDrainShutdown = "admin.drain-shutdown.v1"
	// CapabilityInterThreadOrchestration means the Core serves Pi-to-Pi
	// inter-session orchestration, paginated thread reads, waits and messaging.
	CapabilityInterThreadOrchestration = "orchestration.inter-thread.v1"
	// CapabilityWorkbenchV3 means the Core serves OpenSSH discovery, IDEA Git
	// log/detail/discard, mygo-music style plugins, remote desktop and updates.
	CapabilityWorkbenchV3 = "workbench.v3"
	// CapabilitySkills means skills.list discovers the target workspace's Pi
	// skill commands for the composer instead of requiring a client-side scan.
	CapabilitySkills = "workbench.skills.v1"
	// CapabilityRemoteDesktopPermissions reports the explicit per-machine
	// permission probe/request/settings API, not a platform hardware guarantee.
	CapabilityRemoteDesktopPermissions = "remote-desktop.permissions.v1"
	// CapabilityProjectCoordinator means project.chat.open and durable project
	// milestones are available.
	CapabilityProjectCoordinator = "projects.coordinator.v1"
	// CapabilityEnvironmentOrchestration means linked-environment broker routing
	// and durable proxy tasks are wired into the running Core.
	CapabilityEnvironmentOrchestration = "orchestration.environments.v1"
	// CapabilityPiScriptedTools means Pi workers explicitly load native codemode,
	// tool search and the structured/deferred Vantry tool registry.
	CapabilityPiScriptedTools = "provider.pi.scripted-tools.v1"
	// CapabilityPiRuntime means Core supports runtime inspection, mirror-aware
	// Pi upgrades, and first-run ~/.pi/agent provider setup actions.
	CapabilityPiRuntime = "provider.pi.runtime.v1"
	// CapabilityNativePluginPages means plugin navigation can select native panels.
	CapabilityNativePluginPages   = "plugins.native-pages"
	CapabilityRemoteAccess        = "remote-access.alma-relay" // Stable wire ID used by existing paired clients.
	CapabilityNativeConnectivity  = "remote-access.native.v1"
	CapabilityT3Sessions          = "remote-access.t3.v2"
	CapabilityDirectDiscovery     = "remote-access.direct-discovery"
	CapabilityEnvironmentSessions = "environments.sessions"
)

// StatusSchemaVersion is the persisted projection schema the Core reports. It is
// a business version of the stored entities, not a build marker.
const StatusSchemaVersion = 1

// ServerStatus is the structured state a Core reports about itself. ActiveRuns
// is the number of runs that have not reached a terminal status, so it answers
// "may this Core stop without losing work?".
type ServerStatus struct {
	ProtocolVersion int      `json:"protocolVersion"`
	State           string   `json:"state"`
	SchemaVersion   int      `json:"schemaVersion"`
	PID             int      `json:"pid"`
	ActiveRuns      int      `json:"activeRuns"`
	Capabilities    []string `json:"capabilities"`
}

// HasCapability reports whether the Core advertises name.
func (s ServerStatus) HasCapability(name string) bool {
	for _, capability := range s.Capabilities {
		if capability == name {
			return true
		}
	}
	return false
}

// MissingCapabilities returns the required capabilities the Core does not
// advertise, in the order they were required. An empty result means the Core is
// compatible.
func (s ServerStatus) MissingCapabilities(required []string) []string {
	var missing []string
	for _, name := range required {
		if !s.HasCapability(name) {
			missing = append(missing, name)
		}
	}
	return missing
}

// StatusReporter is implemented by a transport or service that can report
// ServerStatus. A Core that does not implement it is a legacy Core: its
// transport answers 404 for the status endpoint, which is how a client tells
// "this Core cannot report capabilities" apart from "this Core is unreachable".
type StatusReporter interface {
	ServerStatus(context.Context) (ServerStatus, error)
}

// AdminController stops a Core that is provably idle so a matching Core can take
// its socket. An implementation must return a Conflict error while any run is
// active; it must never interrupt work to satisfy a version change.
type AdminController interface {
	RequestDrainShutdown(context.Context) error
}

// IsMissingCapabilityReport reports whether err means the connected Core has no
// status endpoint at all. Only a legacy Core answers NotFound there, so this is
// the precise signal that capability verification is impossible.
func IsMissingCapabilityReport(err error) bool {
	return err != nil && CodeOf(err) == NotFound
}

// ThreadSummary is the bounded per-thread projection: metadata, the latest run
// status, the update time, the number of runs and unfinished inputs. It never
// carries Run.Output or Run.Reasoning, so listing conversations cannot pull
// every stored reply across the socket.
type ThreadSummary struct {
	ProjectPath       string         `json:"projectPath,omitempty"`
	ID                string         `json:"id"`
	Title             string         `json:"title"`
	ProviderID        string         `json:"providerId,omitempty"`
	Model             string         `json:"model,omitempty"`
	Workspace         *WorkspaceSpec `json:"workspace,omitempty"`
	ParentThreadID    string         `json:"parentThreadId,omitempty"`
	ParentTaskID      string         `json:"parentTaskId,omitempty"`
	CreatedByThreadID string         `json:"createdByThreadId,omitempty"`
	Pinned            bool           `json:"pinned,omitempty"`
	Archived          bool           `json:"archived,omitempty"`
	SnoozedUntil      *time.Time     `json:"snoozedUntil,omitempty"`
	ApprovalPolicy    string         `json:"approvalPolicy,omitempty"`
	CreatedAt         time.Time      `json:"createdAt"`
	UpdatedAt         time.Time      `json:"updatedAt"`
	RunCount          int            `json:"runCount"`
	ActiveRuns        int            `json:"activeRuns"`
	PendingInputs     int            `json:"pendingInputs"`
	PendingRequests   int            `json:"pendingRequests"`
	LatestRunID       string         `json:"latestRunId,omitempty"`
	LatestStatus      RunStatus      `json:"latestStatus,omitempty"`
}

// ThreadSummaries is the bounded conversation list. Sequence is the event
// sequence the projection is consistent with, so a client can subscribe from it
// without reading a full snapshot.
type ThreadSummaries struct {
	Sequence int64           `json:"sequence"`
	Threads  []ThreadSummary `json:"threads"`
}

// ThreadDetailQuery selects one thread. LimitRuns keeps only the most recent
// runs; zero means all of them.
type ThreadDetailQuery struct {
	ThreadID  string `json:"threadId"`
	LimitRuns int    `json:"limitRuns,omitempty"`
}

// ThreadDetail is one thread with the entities that belong to it: runs, tasks,
// inputs, tools and provider requests. Other threads are never included, and
// LimitRuns bounds the run history a client has to move.
type ThreadDetail struct {
	ProjectUpdates []ProjectUpdate   `json:"projectUpdates,omitempty"`
	Sequence       int64             `json:"sequence"`
	Thread         Thread            `json:"thread"`
	Runs           []Run             `json:"runs"`
	Tasks          []Task            `json:"tasks"`
	Inputs         []Input           `json:"inputs"`
	Tools          []ToolCall        `json:"tools"`
	Requests       []ProviderRequest `json:"requests"`
	RunCount       int               `json:"runCount"`
	TruncatedRuns  bool              `json:"truncatedRuns,omitempty"`
}

// ThreadReader is implemented by a Core or transport that serves the lightweight
// projections directly. A transport whose backend does not implement it derives
// the same projections from Snapshot, so the trimmed payload still applies.
type ThreadReader interface {
	ThreadSummaries(context.Context) (ThreadSummaries, error)
	ThreadDetail(context.Context, ThreadDetailQuery) (ThreadDetail, error)
}

// ActiveRunCount counts the runs that have not reached a terminal status.
func ActiveRunCount(snapshot Snapshot) int {
	count := 0
	for _, run := range snapshot.Runs {
		if !run.Status.Terminal() {
			count++
		}
	}
	return count
}

// BuildThreadSummaries projects a full snapshot into the bounded conversation
// list. Reply text, reasoning and tool payloads are dropped by construction.
func BuildThreadSummaries(snapshot Snapshot) ThreadSummaries {
	summaries := ThreadSummaries{Sequence: snapshot.Sequence, Threads: make([]ThreadSummary, 0, len(snapshot.Threads))}
	index := make(map[string]int, len(snapshot.Threads))
	for _, thread := range snapshot.Threads {
		index[thread.ID] = len(summaries.Threads)
		summaries.Threads = append(summaries.Threads, ThreadSummary{
			ProjectPath:       thread.ProjectPath,
			ID:                thread.ID,
			Title:             thread.Title,
			ProviderID:        thread.ProviderID,
			Model:             thread.Model,
			Workspace:         thread.Workspace,
			ParentThreadID:    thread.ParentThreadID,
			ParentTaskID:      thread.ParentTaskID,
			CreatedByThreadID: thread.CreatedByThreadID,
			Pinned:            thread.Pinned,
			Archived:          thread.Archived,
			SnoozedUntil:      thread.SnoozedUntil,
			ApprovalPolicy:    thread.ApprovalPolicy,
			CreatedAt:         thread.CreatedAt,
			UpdatedAt:         thread.CreatedAt,
		})
	}
	if len(index) == 0 {
		return summaries
	}

	type aggregate struct {
		runCount        int
		activeRuns      int
		pendingInputs   int
		pendingRequests int
		latestSeen      bool
		latestCreatedAt time.Time
		latestRunID     string
		latestStatus    RunStatus
	}
	aggregates := make([]aggregate, len(summaries.Threads))
	touch := func(position int, at time.Time) {
		if at.After(summaries.Threads[position].UpdatedAt) {
			summaries.Threads[position].UpdatedAt = at
		}
	}

	for _, run := range snapshot.Runs {
		position, ok := index[run.ThreadID]
		if !ok {
			continue
		}
		aggregates[position].runCount++
		if !run.Status.Terminal() {
			aggregates[position].activeRuns++
		}
		touch(position, run.UpdatedAt)
		if !aggregates[position].latestSeen || !run.CreatedAt.Before(aggregates[position].latestCreatedAt) {
			aggregates[position].latestSeen = true
			aggregates[position].latestCreatedAt = run.CreatedAt
			aggregates[position].latestRunID = run.ID
			aggregates[position].latestStatus = run.Status
		}
	}
	for _, input := range snapshot.Inputs {
		position, ok := index[input.ThreadID]
		if !ok {
			continue
		}
		if inputUnfinished(input.Status) {
			aggregates[position].pendingInputs++
		}
		touch(position, input.UpdatedAt)
	}
	for _, tool := range snapshot.Tools {
		if position, ok := index[tool.ThreadID]; ok {
			touch(position, tool.UpdatedAt)
		}
	}
	for _, request := range snapshot.Requests {
		position, ok := index[request.ThreadID]
		if !ok {
			continue
		}
		if request.Status == "pending" || request.Status == "responding" {
			aggregates[position].pendingRequests++
		}
		touch(position, request.UpdatedAt)
	}
	for _, task := range snapshot.Tasks {
		if position, ok := index[task.ParentThreadID]; ok {
			touch(position, task.UpdatedAt)
		}
		if position, ok := index[task.ChildThreadID]; ok {
			touch(position, task.UpdatedAt)
		}
	}

	for _, update := range snapshot.ProjectUpdates {
		if position, ok := index[update.ThreadID]; ok {
			touch(position, update.CreatedAt)
		}
	}

	for position := range summaries.Threads {
		summaries.Threads[position].RunCount = aggregates[position].runCount
		summaries.Threads[position].ActiveRuns = aggregates[position].activeRuns
		summaries.Threads[position].PendingInputs = aggregates[position].pendingInputs
		summaries.Threads[position].PendingRequests = aggregates[position].pendingRequests
		summaries.Threads[position].LatestRunID = aggregates[position].latestRunID
		summaries.Threads[position].LatestStatus = aggregates[position].latestStatus
	}
	return summaries
}

func inputUnfinished(status string) bool {
	switch status {
	case "queued", "pending", "started":
		return true
	default:
		return false
	}
}

// BuildThreadDetail projects one thread out of a full snapshot. LimitRuns keeps
// the most recent runs in chronological order and reports the truncation
// explicitly instead of silently dropping history.
func BuildThreadDetail(snapshot Snapshot, query ThreadDetailQuery) (ThreadDetail, error) {
	if query.ThreadID == "" {
		return ThreadDetail{}, &Error{Code: InvalidCommand, Message: "threadId is required for the thread detail query"}
	}
	detail := ThreadDetail{
		Sequence: snapshot.Sequence,
		Runs:     []Run{},
		Tasks:    []Task{},
		Inputs:   []Input{},
		Tools:    []ToolCall{},
		Requests: []ProviderRequest{},
	}
	found := false
	for _, thread := range snapshot.Threads {
		if thread.ID == query.ThreadID {
			detail.Thread = thread
			found = true
			break
		}
	}
	if !found {
		return ThreadDetail{}, &Error{Code: NotFound, Message: fmt.Sprintf("thread %q not found", query.ThreadID)}
	}
	for _, run := range snapshot.Runs {
		if run.ThreadID == query.ThreadID {
			detail.Runs = append(detail.Runs, run)
		}
	}
	detail.RunCount = len(detail.Runs)
	if query.LimitRuns > 0 && len(detail.Runs) > query.LimitRuns {
		detail.Runs = detail.Runs[len(detail.Runs)-query.LimitRuns:]
		detail.TruncatedRuns = true
	}
	for _, task := range snapshot.Tasks {
		if task.ParentThreadID == query.ThreadID || task.ChildThreadID == query.ThreadID {
			detail.Tasks = append(detail.Tasks, task)
		}
	}
	for _, input := range snapshot.Inputs {
		if input.ThreadID == query.ThreadID {
			detail.Inputs = append(detail.Inputs, input)
		}
	}
	for _, tool := range snapshot.Tools {
		if tool.ThreadID == query.ThreadID {
			detail.Tools = append(detail.Tools, tool)
		}
	}
	for _, request := range snapshot.Requests {
		if request.ThreadID == query.ThreadID {
			detail.Requests = append(detail.Requests, request)
		}
	}
	for _, update := range snapshot.ProjectUpdates {
		if update.ThreadID == query.ThreadID {
			detail.ProjectUpdates = append(detail.ProjectUpdates, update)
		}
	}
	return detail, nil
}
