package desktop

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
	"github.com/muq9738/Vantry/pkg/api"
)

// Options own MyGo's image cache, so each thread gets an independent one.
// Loader closures capture the immutable service and thread, never view state.
func (w *workbench) replyMarkdown(threadID string) *markdown.Options {
	if w.visuals.markdowns == nil {
		w.visuals.markdowns = map[string]*markdown.Options{}
	}
	options := w.visuals.markdowns[threadID]
	if options == nil {
		// Bound retained decoded images across thread switches. Existing visible
		// views retain their options until they unmount.
		if len(w.visuals.markdowns) >= 8 {
			clear(w.visuals.markdowns)
		}
		options = &markdown.Options{}
		service, ok := w.service.(api.WorkbenchService)
		if ok {
			options.LoadImage = func(address string) (ui.ImageSource, error) {
				name, err := replyImagePath(address)
				if err != nil {
					return nil, err
				}
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				reply, err := service.Workbench(ctx, api.WorkbenchRequest{Action: "reply.image", ThreadID: threadID, Name: name})
				if err != nil {
					return nil, err
				}
				if strings.EqualFold(filepath.Ext(name), ".svg") {
					return ui.ParseSVG(reply.Data)
				}
				return ui.DecodeBitmap(reply.Data)
			}
		}
		w.visuals.markdowns[threadID] = options
	}
	if options.OnLink == nil {
		options.OnLink = func(address string) { w.replyThreadLink(threadID, address) }
	}
	if options.Block == nil {
		options.Block = w.replyTableBlock(options)
	}
	options.Theme = w.widgets.markdown.Theme
	options.Reveal = w.widgets.markdown.Reveal
	options.Code = w.widgets.markdown.Code
	options.Extensions = w.widgets.markdown.Extensions
	options.Math = w.widgets.markdown.Math
	options.InlineMath = w.widgets.markdown.InlineMath
	return options
}

// threadLinkScheme is the scheme of api.FormatThreadLink's references.
const threadLinkScheme = "vantry-thread://"

// replyLink handles a link from the selected thread. Markdown callbacks bind
// their original thread so file requests use the workspace that owns the reply.
func (w *workbench) replyLink(address string) {
	w.replyThreadLink(w.selected, address)
}

func (w *workbench) replyThreadLink(threadID, address string) {
	if strings.HasPrefix(address, threadLinkScheme) {
		link, err := url.Parse(address)
		if err != nil || link.Host == "" {
			w.setAction("会话链接无效。")
			return
		}
		if environmentID := link.Query().Get("environmentId"); environmentID != "" {
			w.openEnvironmentThread(environmentID, link.Host)
		} else {
			w.openThreadLink(link.Host)
		}
		return
	}
	if ui.SafeToOpen(address) {
		w.widgets.services.OpenURL(address)
		return
	}
	file, line, ok := replyFileLocation(address)
	if !ok {
		return
	}
	w.openReplyFile(threadID, file, line)
}

var replyFileLineSuffix = regexp.MustCompile(`^(.*?):([1-9][0-9]*)(?::[1-9][0-9]*)?$`)
var replyFileLineFragment = regexp.MustCompile(`^L?([1-9][0-9]*)(?:[-,:].*)?$`)

// replyFileLocation accepts workspace paths and local file URLs, including the
// :line and #Lline forms agents use. URL paths are decoded once by url.Parse.
func replyFileLocation(address string) (string, int, bool) {
	line := 0
	if match := replyFileLineSuffix.FindStringSubmatch(address); match != nil {
		address = match[1]
		line, _ = strconv.Atoi(match[2])
	}
	link, err := url.Parse(address)
	if err != nil || link.Path == "" || link.Scheme != "" && link.Scheme != "file" || link.Host != "" && link.Host != "localhost" {
		return "", 0, false
	}
	if match := replyFileLineFragment.FindStringSubmatch(link.Fragment); match != nil {
		line, _ = strconv.Atoi(match[1])
	}
	return link.Path, line, true
}

// File reads stay on this workbench's Core. Convert an absolute link to the
// workspace-relative path expected by files.read; Core retains its existing
// symlink and workspace boundary checks.
func (w *workbench) openReplyFile(threadID, file string, line int) {
	var spec *api.WorkspaceSpec
	if thread, ok := w.threadByID(threadID); ok {
		spec = thread.Workspace
	} else {
		spec = w.defaults.Workspace
	}
	if filepath.IsAbs(file) {
		root := ""
		if spec != nil {
			root = spec.Path
			if root == "" {
				root = spec.Repository
			}
		}
		if root == "" {
			w.setAction("请先选择项目目录，再打开文件链接。")
			return
		}
		var err error
		file, err = filepath.Rel(root, file)
		if err != nil {
			w.setAction("无法解析文件链接。")
			return
		}
	}
	file = filepath.Clean(file)
	if !filepath.IsLocal(file) || file == "." {
		w.setAction("文件链接不在当前会话的工作区内。")
		return
	}
	if _, _, ok := w.fileScope(threadID); !ok {
		w.setAction("请先选择项目目录，再打开文件链接。")
		return
	}
	if w.selected != threadID {
		w.selectThread(threadID)
	}
	file = filepath.ToSlash(file)
	if w.files.dir == nil {
		w.files.dir = map[string]string{}
	}
	dir := filepath.ToSlash(filepath.Dir(file))
	if dir == "." {
		dir = ""
	}
	w.files.dir[threadID] = dir
	w.openDock("files")
	w.openFileAt(threadID, file, line)
}

// A remote link is never resolved against local thread IDs, even when the
// requested environment has not been connected in this window yet.
func (w *workbench) openEnvironmentThread(environmentID, threadID string) {
	if environmentID == w.environmentKey && w.environmentKey != "" {
		w.openThreadLink(threadID)
		return
	}
	if hub := w.environments; hub != nil {
		for _, record := range hub.records {
			if record.key != environmentID && record.connectionID != environmentID {
				continue
			}
			if record.view == nil {
				w.setAction("请先在运行机器中连接目标环境，再打开此会话。")
				return
			}
			if !record.view.hasSession(threadID) {
				w.setAction("目标环境没有此会话，请刷新目标机器的会话列表。")
				return
			}
			hub.current = record.view
			record.view.openThreadLink(threadID)
			return
		}
	}
	w.setAction("目标环境尚未在此窗口连接；请先连接目标机器。SSH 环境链接暂不支持自动导航。")
}

func replyImagePath(address string) (string, error) {
	parsed, err := url.Parse(address)
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "" && parsed.Scheme != "file" || parsed.Host != "" {
		return "", errors.New("仅加载会话工作区图片")
	}
	if parsed.Path == "" {
		return "", errors.New("图片路径为空")
	}
	return parsed.Path, nil
}
