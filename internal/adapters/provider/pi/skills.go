package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// Skills asks Pi's own command registry for the skills a worker in directory
// would expand. It never scans skill directories, never parses credentials,
// and never invokes a model. Extension and MCP loading mirror the durable
// worker's policy so the reported commands are exactly the ones a prompt can
// use; prompt templates and tools stay off because they are not skills and
// cannot be invoked through this catalog.
func Skills(ctx context.Context, executable, directory string) ([]api.SkillInfo, error) {
	userExtensions := resolveBoolEnvOverride(EnvUserExtensions, true)
	userMCP := resolveBoolEnvOverride(EnvUserMCP, true)
	return querySkills(ctx, executable, directory, userExtensions, userMCP)
}

func querySkills(ctx context.Context, executable, directory string, userExtensions, userMCP bool) ([]api.SkillInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	executable = resolvePiExecutable(executable)
	args := []string{"--mode", "rpc", "--no-session", "--no-tools", "--no-prompt-templates"}
	if !userExtensions {
		args = append(args, "--no-extensions")
	}
	if !userMCP {
		args = append(args, "--no-mcp")
	}
	cmd, err := newPiQueryCommand(ctx, executable, args...)
	if err != nil {
		return nil, err
	}
	defer cmd.Close()
	cmd.Env = piEnvironment(executable)
	cmd.Dir = directory
	cmd.WaitDelay = time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Pi may print setup diagnostics. They are discarded rather than surfaced:
	// a diagnostic stream can carry provider details the catalog must not leak.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 Pi 技能查询：%w", err)
	}
	defer func() { _ = stdin.Close(); cancel(); _ = cmd.Wait() }()
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64<<10), 16<<20)
	if err := writeRPC(stdin, map[string]any{"id": "skills-1", "type": "get_commands"}); err != nil {
		return nil, err
	}
	var commands struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
			SourceInfo  struct {
				Path   string `json:"path"`
				Source string `json:"source"`
				Scope  string `json:"scope"`
			} `json:"sourceInfo"`
		} `json:"commands"`
	}
	for scan.Scan() {
		var record struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Command string          `json:"command"`
			Success bool            `json:"success"`
			Error   string          `json:"error"`
			Data    json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("Pi RPC 响应无效：%w", err)
		}
		if record.Type != "response" || record.ID != "skills-1" {
			continue
		}
		if !record.Success {
			return nil, fmt.Errorf("Pi 拒绝 get_commands 查询：%s", nonEmpty(record.Error, "请检查 Pi 配置"))
		}
		if err := json.Unmarshal(record.Data, &commands); err != nil {
			return nil, fmt.Errorf("Pi 技能目录格式无效：%w", err)
		}
		skills := make([]api.SkillInfo, 0, len(commands.Commands))
		seen := make(map[string]bool, len(commands.Commands))
		for _, command := range commands.Commands {
			if command.Source != "skill" || seen[command.Name] {
				continue
			}
			seen[command.Name] = true
			source := command.SourceInfo.Scope
			if source == "" {
				source = command.SourceInfo.Source
			}
			if source == "" {
				source = command.Source
			}
			skills = append(skills, api.SkillInfo{
				Name:        strings.TrimPrefix(command.Name, "skill:"),
				Description: command.Description,
				FilePath:    command.SourceInfo.Path,
				Source:      source,
				Command:     command.Name,
			})
		}
		sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
		return skills, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if scan.Err() != nil {
		return nil, scan.Err()
	}
	return nil, errors.New("Pi 技能查询提前退出")
}
