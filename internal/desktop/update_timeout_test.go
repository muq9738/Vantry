package desktop

import (
	"context"
	"testing"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

type updateDeadlineService struct {
	*fakeService
	action string
	budget time.Duration
}

func (s *updateDeadlineService) Workbench(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	s.action = request.Action
	deadline, ok := ctx.Deadline()
	if ok {
		s.budget = time.Until(deadline)
	}
	return api.WorkbenchResponse{}, nil
}

func TestUpdateDownloadDoesNotUseOrdinaryToolTimeout(t *testing.T) {
	service := &updateDeadlineService{fakeService: &fakeService{}}
	w := newWorkbench(service)
	w.workbenchCall("download", api.WorkbenchRequest{Action: actionUpdateInstall}, func(api.WorkbenchResponse, error) {})
	if service.action != actionUpdateInstall || service.budget < 15*time.Minute {
		t.Fatalf("update download budget too short: %s %v", service.action, service.budget)
	}
	w.workbenchCall("check", api.WorkbenchRequest{Action: actionUpdateCheck}, func(api.WorkbenchResponse, error) {})
	if service.budget > time.Minute {
		t.Fatalf("ordinary check has download timeout: %v", service.budget)
	}
}
