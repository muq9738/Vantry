package updater

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var githubTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{8,255}$`)

// UpdateTokenFile is separate from development Git/gh credentials. Configure a
// fine-grained GitHub token restricted to this repository, Contents: read-only
// (Metadata: read-only is implicit). The client cannot reduce a token's grants.
func UpdateTokenFile() string {
	if p := strings.TrimSpace(os.Getenv("VANTRY_UPDATE_TOKEN_FILE")); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "Vantry", "update-token")
}

// FindGitHubToken prefers the local dedicated credential, then the read-only
// credential shipped with Vantry. It never consults development credentials.
func FindGitHubToken(context.Context) string {
	token, _ := readUpdateCredentials(UpdateTokenFile(), bundledUpdateTokenFile())
	return token
}

func bundledUpdateTokenFile() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	return updateTokenBesideExecutable(executable)
}

func updateTokenBesideExecutable(executable string) string {
	if bundle, ok := ResolveCurrentAppBundle(executable); ok {
		return filepath.Join(bundle, "Contents", "Resources", "update-token")
	}
	// MyGo puts resources beside the executable on Windows and Linux.
	return filepath.Join(filepath.Dir(executable), "update-token")
}

func readUpdateCredentials(local, bundled string) (string, error) {
	token, err := readUpdateToken(local)
	if err != nil || token != "" {
		return token, err
	}
	// A distributed app's resources may be readable by all of its users.
	// Only the locally configured override requires private permissions.
	return readTokenFile(bundled, false)
}

func readUpdateToken(path string) (string, error) {
	return readTokenFile(path, true)
}

func readTokenFile(path string, private bool) (string, error) {
	if path == "" {
		return "", nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("无法读取 Vantry 专用更新凭据文件")
	}
	if !info.Mode().IsRegular() || info.Size() > 512 {
		return "", errors.New("Vantry 更新凭据文件格式无效")
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("Vantry 更新凭据文件须仅当前用户可读写（0600）")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("无法读取 Vantry 专用更新凭据文件")
	}
	token := cleanToken(string(data))
	if token == "" {
		return "", errors.New("Vantry 更新凭据文件为空或格式无效")
	}
	return token, nil
}

func cleanToken(value string) string {
	value = strings.TrimSpace(value)
	if !githubTokenPattern.MatchString(value) {
		return ""
	}
	return value
}
