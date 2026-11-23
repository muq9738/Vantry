//go:build unix

package pi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/muq9738/Vantry/internal/adapters/workspace"
)

type processGroupMember struct {
	pid, parent, group int
	command            string
}

func processGroups() ([]processGroupMember, error) {
	// Do not inspect or log process environments. The invocation's private
	// instructions path identifies this attempt without relying on a PID alone.
	output, err := exec.Command("ps", "-ww", "-axo", "pid=,ppid=,pgid=,command=").Output()
	if err != nil {
		return nil, fmt.Errorf("inspect provider process groups: %w", err)
	}
	var members []processGroupMember
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.Fields(line)
		if len(parts) < 4 {
			continue
		}
		pid, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		parent, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		group, err := strconv.Atoi(parts[2])
		if err != nil {
			continue
		}
		members = append(members, processGroupMember{pid: pid, parent: parent, group: group, command: line})
	}
	return members, nil
}

// Pi's bash tool starts detached groups and Node does not pass fd 3 to them.
// Freeze Pi before collecting its descendants so it cannot launch another tool
// between the snapshot and cancellation. Persist every owned group before
// signalling tools, and kill Pi last.
func terminateProcessTree(jobDir, attemptID string, root int) (map[int]bool, error) {
	if root <= 1 {
		return nil, errors.New("invalid provider process identity")
	}
	if err := syscall.Kill(-root, syscall.SIGSTOP); err != nil && !errors.Is(err, syscall.ESRCH) {
		return nil, err
	}
	finished := false
	defer func() {
		if !finished {
			_ = syscall.Kill(-root, syscall.SIGCONT)
		}
	}()
	members, err := processGroups()
	if err != nil {
		return nil, err
	}
	owned := map[int]bool{root: true}
	for changed := true; changed; {
		changed = false
		for _, member := range members {
			if owned[member.parent] && !owned[member.pid] {
				owned[member.pid], changed = true, true
			}
		}
	}
	groups := map[int]bool{root: true}
	for _, member := range members {
		if owned[member.pid] && owned[member.group] {
			groups[member.group] = true
		}
	}
	birth, _ := processIdentity(root)
	record := piProcess{AttemptID: attemptID, PID: root, ProcessStart: birth}
	for group := range groups {
		record.Groups = append(record.Groups, group)
	}
	if err := writeJSONFile(filepath.Join(jobDir, "cleanup.json"), record); err != nil {
		return nil, err
	}
	for group := range groups {
		if group == root {
			continue
		}
		if err := syscall.Kill(-group, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return groups, err
		}
	}
	if err := syscall.Kill(-root, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return groups, err
	}
	finished = true
	return groups, nil
}

// cleanupOrphanedPi runs only while worker.lock proves there is no live worker.
// It terminates positively identified attempt groups, and never publishes a
// terminal result while an uncertain group or inherited workspace lock remains.
func cleanupOrphanedPi(jobDir, attemptID string) error {
	job, found, err := readJobRequest(jobDir)
	if err != nil {
		return err
	}
	if !found || job.Request.AttemptID != attemptID {
		return errors.New("missing orphan execution identity")
	}
	members, err := processGroups()
	if err != nil {
		return errOrphanPending
	}
	var launched piProcess
	launchFound, err := readJSONFile(filepath.Join(jobDir, "pi-process.json"), &launched)
	if err != nil {
		return err
	}
	authorizedPID := 0
	if launchFound {
		if launched.AttemptID != attemptID || launched.PID <= 1 {
			return errors.New("orphan launch identity mismatch")
		}
		if birth, err := processIdentity(launched.PID); err == nil && launched.ProcessStart != "" && birth == launched.ProcessStart {
			authorizedPID = launched.PID
		}
	}
	instructions := filepath.Join(jobDir, "instructions.md")
	groups := map[int]bool{}
	identified := false
	for _, member := range members {
		pathMatch := strings.Contains(member.command, "--append-system-prompt "+instructions) &&
			(!launchFound || launched.PID != member.pid || authorizedPID == member.pid)
		if member.pid > 1 && member.pid == member.group && (authorizedPID == member.pid || pathMatch) {
			identified = true
			owned, err := terminateProcessTree(jobDir, attemptID, member.pid)
			if err != nil {
				return errOrphanPending
			}
			for group := range owned {
				groups[group] = true
			}
		}
	}
	for _, name := range []string{"pi-process.json", "cleanup.json"} {
		var recorded piProcess
		if found, err := readJSONFile(filepath.Join(jobDir, name), &recorded); err != nil {
			return err
		} else if found {
			if recorded.AttemptID != attemptID || recorded.PID <= 1 {
				return errors.New("orphan process identity mismatch")
			}
			groups[recorded.PID] = true
			for _, group := range recorded.Groups {
				if group <= 1 {
					return errors.New("invalid owned process group")
				}
				groups[group] = true
			}
			identified = identified || len(recorded.Groups) > 0
		}
	}
	// A leader may have exited before its tools. Keep the attempt unresolved
	// until that recorded group disappears; do not signal an unverified PID.
	if len(groups) > 0 {
		members, err = processGroups()
		if err != nil {
			return errOrphanPending
		}
		for _, member := range members {
			if groups[member.group] {
				return errOrphanPending
			}
		}
	}
	// The identified invocation and every detached tool group are gone. A new
	// legitimate writer may already own this shared directory; its lock must
	// not delay terminalizing the old attempt.
	if identified {
		return nil
	}
	if job.Request.Workspace == nil {
		return errors.New("orphan workspace identity is missing")
	}
	path := job.Request.Workspace.Path
	if path == "" {
		path = filepath.Join(job.DataDir, "workspaces", job.Request.ThreadID, "worktree")
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	lock, err := workspace.LockDirectory(ctx, path)
	if err != nil {
		return errOrphanPending
	}
	workspace.Unlock(lock)
	return nil
}
