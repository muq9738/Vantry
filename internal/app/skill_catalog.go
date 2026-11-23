package app

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	piprovider "github.com/muq9738/Vantry/internal/adapters/provider/pi"
	"github.com/muq9738/Vantry/pkg/api"
)

// skillCatalogTTL keeps the composer menu from starting a Pi process for every
// keystroke while staying fresh enough for newly added skills. Lookups are
// keyed by the exact query inputs so a thread in another workspace never sees
// a neighboring thread's skills.
const skillCatalogTTL = 15 * time.Second

type skillCatalogEntry struct {
	skills  []api.SkillInfo
	expires time.Time
}

// skillsList resolves the thread's prepared workspace (never preparing one) or
// the explicitly passed directory, then asks the target Core's own Pi for the
// skills a worker there would expand. An invalid thread stays a hard error;
// there is no silent fallback to a generic directory.
func (s *workbenchService) skillsList(ctx context.Context, request api.WorkbenchRequest) (api.WorkbenchResponse, error) {
	var response api.WorkbenchResponse
	var directory string
	var err error
	switch {
	case request.ThreadID != "":
		// A stale or foreign thread must not inherit another project's skills.
		directory, err = s.directory(ctx, request.ThreadID)
	case strings.TrimSpace(request.Workspace) != "" || s.config.Workspace != nil && s.config.Workspace.Path != "":
		directory, err = s.resolveRoot(ctx, request)
	default:
		// A projectless draft has no prepared conversation directory yet. Query
		// within this Core's data directory rather than the launcher's checkout;
		// Pi can still discover its global skills without creating a workspace.
		directory = s.config.DataDir
		if strings.TrimSpace(directory) == "" {
			err = &api.Error{Code: api.NotFound, Message: "Core 技能目录不可用"}
		}
	}
	if err != nil {
		return response, err
	}
	skills, err := s.skillCatalog(ctx, directory)
	if err != nil {
		return response, err
	}
	raw, err := json.Marshal(map[string]any{"skills": skills})
	if err != nil {
		return response, err
	}
	response.Workspace = directory
	response.Output = string(raw)
	return response, nil
}

func (s *workbenchService) skillCatalog(ctx context.Context, directory string) ([]api.SkillInfo, error) {
	piExe := s.effectivePiExecutable()
	key := strings.Join([]string{
		piExe,
		directory,
		os.Getenv(piprovider.EnvUserExtensions),
		os.Getenv(piprovider.EnvUserMCP),
	}, "\x00")
	s.skillsMu.Lock()
	entry, cached := s.skills[key]
	s.skillsMu.Unlock()
	if cached && time.Now().Before(entry.expires) {
		return entry.skills, nil
	}
	skills, err := piprovider.Skills(ctx, piExe, directory)
	if err != nil {
		return nil, err
	}
	s.skillsMu.Lock()
	if s.skills == nil {
		s.skills = make(map[string]skillCatalogEntry)
	}
	s.skills[key] = skillCatalogEntry{skills: skills, expires: time.Now().Add(skillCatalogTTL)}
	s.skillsMu.Unlock()
	return skills, nil
}
