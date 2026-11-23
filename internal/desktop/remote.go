package desktop

import (
	"encoding/json"
	"strings"

	"github.com/muq9738/Vantry/pkg/api"
)

const actionEnvironmentsList = "environments.list"

// remoteFavorite is a saved SSH target: an alias, the host as the user's SSH
// configuration names it, and the remote Core socket. No credential is kept.
type remoteFavorite struct {
	Name   string `json:"name"`
	Host   string `json:"host"`
	Socket string `json:"socket"`
}

type remoteFavorites struct {
	saved   []remoteFavorite
	removed []string
	core    []remoteFavorite
	alias   string
	loaded  bool
}

func (f remoteFavorite) title() string {
	if strings.TrimSpace(f.Name) != "" {
		return f.Name
	}
	return f.Host
}

func (w *workbench) saveRemoteFavorite() bool {
	host, socket := strings.TrimSpace(w.features.remoteHost), strings.TrimSpace(w.features.remoteSocket)
	if host == "" || socket == "" {
		w.setToolFeedback("remote", notice{Text: "填写 SSH 主机与远程 socket 后再收藏。", Level: noticeWarn})
		return false
	}
	var removed []string
	for _, target := range w.remote.removed {
		if target != host {
			removed = append(removed, target)
		}
	}
	w.remote.removed = removed
	favorite := remoteFavorite{Name: strings.TrimSpace(w.remote.alias), Host: host, Socket: socket}
	for i, saved := range w.remote.saved {
		if saved.Host == host && saved.Socket == socket {
			if favorite.Name == "" {
				favorite.Name = saved.Name
			}
			w.remote.saved[i] = favorite
			w.remote.alias = ""
			w.saveState()
			w.setToolFeedback("remote", notice{Text: "已更新收藏：" + favorite.title(), Level: noticeInfo})
			return true
		}
	}
	w.remote.saved = append(w.remote.saved, favorite)
	w.remote.alias = ""
	w.saveState()
	w.setToolFeedback("remote", notice{Text: "已收藏：" + favorite.title(), Level: noticeInfo})
	return true
}

func (w *workbench) removeRemoteFavorite(index int) {
	if index < 0 || index >= len(w.remote.saved) {
		return
	}
	w.remote.saved = append(w.remote.saved[:index:index], w.remote.saved[index+1:]...)
	w.saveState()
}

// removeSSHHost removes saved targets and hides auto-discovered targets in
// Vantry. It leaves the user's shared OpenSSH configuration intact.
func (w *workbench) removeSSHHost(target string) {
	var saved []remoteFavorite
	for _, favorite := range w.remote.saved {
		if favorite.Host != target {
			saved = append(saved, favorite)
		}
	}
	w.remote.saved = saved
	if !containsString(w.remote.removed, target) {
		w.remote.removed = append(w.remote.removed, target)
	}
	if w.ssh.selected == target {
		w.ssh.selected = ""
	}
	w.saveState()
	w.setToolFeedback("remote", notice{Text: "已从主机列表移除 " + target + "；可在列表菜单中恢复。", Level: noticeInfo})
}

func (w *workbench) restoreSSHEntries() {
	w.remote.removed = nil
	w.saveState()
	w.setToolFeedback("remote", notice{Text: "已恢复自动发现的主机。", Level: noticeInfo})
}

func (w *workbench) useRemoteFavorite(f remoteFavorite) {
	w.features.remoteHost, w.features.remoteSocket = f.Host, f.Socket
}

func decodeEnvironments(response api.WorkbenchResponse) []remoteFavorite {
	text := strings.TrimSpace(response.Output)
	if text == "" {
		text = strings.TrimSpace(string(response.Data))
	}
	if text == "" {
		return nil
	}
	var wrapped struct {
		Environments json.RawMessage `json:"environments"`
	}
	data := []byte(text)
	if json.Unmarshal(data, &wrapped) == nil && len(wrapped.Environments) > 0 {
		data = wrapped.Environments
	}
	var raw []map[string]any
	if json.Unmarshal(data, &raw) != nil {
		return nil
	}
	var out []remoteFavorite
	for _, item := range raw {
		get := func(keys ...string) string {
			for _, key := range keys {
				if value, ok := item[key].(string); ok && strings.TrimSpace(value) != "" {
					return strings.TrimSpace(value)
				}
			}
			return ""
		}
		f := remoteFavorite{Name: get("name", "alias", "label", "id"), Host: get("host", "sshHost", "remote"), Socket: get("socket", "coreSocket", "remoteSocket")}
		if f.Host != "" {
			out = append(out, f)
		}
	}
	return out
}

func (w *workbench) loadEnvironments() {
	if w.remote.loaded {
		return
	}
	w.workbenchCall("environments", api.WorkbenchRequest{Action: actionEnvironmentsList}, func(response api.WorkbenchResponse, err error) {
		w.remote.loaded = true
		if err != nil {
			return // optional: local favorites remain the source
		}
		w.remote.core = decodeEnvironments(response)
	})
}
