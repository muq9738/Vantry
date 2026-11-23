package app

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

func (s *workbenchService) environmentInfo() (api.EnvironmentInfo, error) {
	s.environmentMu.Lock()
	defer s.environmentMu.Unlock()
	path := filepath.Join(s.config.DataDir, "environment-id")
	id, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		value := make([]byte, 24)
		if _, err = rand.Read(value); err != nil {
			return api.EnvironmentInfo{}, err
		}
		id = []byte("env_" + base64.RawURLEncoding.EncodeToString(value))
		file, createErr := os.CreateTemp(s.config.DataDir, ".environment-id-*")
		if createErr != nil {
			return api.EnvironmentInfo{}, createErr
		}
		defer os.Remove(file.Name())
		_, err = file.Write(id)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(file.Name(), path)
		}
	}
	if err != nil {
		return api.EnvironmentInfo{}, err
	}
	if strings.TrimSpace(string(id)) == "" {
		return api.EnvironmentInfo{}, errors.New("执行环境身份无效，请检查本机数据目录")
	}
	name, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	provider := s.config.ProviderID
	if provider == "" {
		provider = "pi"
	}
	return api.EnvironmentInfo{ID: strings.TrimSpace(string(id)), Name: strings.TrimSuffix(name, ".local"), OS: runtime.GOOS, Arch: runtime.GOARCH, Home: home, ProviderID: provider, DefaultModel: s.config.Model, Workspace: s.config.Workspace}, nil
}

func environmentDirectories(path string) (api.EnvironmentDirectories, error) {
	if strings.TrimSpace(path) == "" {
		path, _ = os.UserHomeDir()
	}
	if !filepath.IsAbs(path) {
		return api.EnvironmentDirectories{}, errors.New("请选择目标机器上的绝对路径")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return api.EnvironmentDirectories{}, errors.New("无法读取目标机器的目录，请检查路径和权限")
	}
	result := api.EnvironmentDirectories{Path: filepath.Clean(path), Parent: filepath.Dir(path), Entries: []api.EnvironmentDirectory{}}
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			result.Entries = append(result.Entries, api.EnvironmentDirectory{Name: entry.Name(), Path: filepath.Join(path, entry.Name())})
		}
	}
	return result, nil
}

func environmentOutput(value any) (api.WorkbenchResponse, error) {
	data, err := json.Marshal(value)
	return api.WorkbenchResponse{Output: string(data)}, err
}
