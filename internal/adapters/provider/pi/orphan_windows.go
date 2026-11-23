//go:build windows

package pi

import (
	"errors"
	"github.com/muq9738/Vantry/internal/hostprocess"
)

// Called only after acquiring this attempt's worker.lock. The workspace guard
// prevents overlapping execution, while the nested attempt job identifies only
// this execution. A later writer's job/lock cannot delay this attempt's result.
func cleanupOrphanedPi(jobDir, attemptID string) error {
	job, found, err := readJobRequest(jobDir)
	if err != nil {
		return err
	}
	if !found || job.Request.AttemptID != attemptID {
		return errors.New("missing orphan execution identity")
	}
	active, err := hostprocess.JobActive(piExecutionIdentity(jobDir, attemptID))
	if err != nil || active {
		return errOrphanPending
	}
	return nil
}
