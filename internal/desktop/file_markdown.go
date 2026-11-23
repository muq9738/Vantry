package desktop

import (
	"fmt"
	"path"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/markdown"
)

func isMarkdownFile(file string) bool {
	ext := strings.ToLower(path.Ext(file))
	return ext == ".md" || ext == ".markdown"
}

// The file reader owns its Markdown cache and selection. Relative links and
// images resolve beside this document on the same Core that owns the thread.
func (w *workbench) fileMarkdownOptions(target, file, key string) *markdown.Options {
	if w.files.markdowns == nil {
		w.files.markdowns = map[string]*markdown.Options{}
	}
	options := w.files.markdowns[key]
	base := w.replyMarkdown(target)
	if options == nil {
		options = &markdown.Options{WrapCode: true}
		options.OnLink = func(address string) {
			if strings.HasPrefix(address, "#") {
				options.Anchor = address
				options.AnchorVersion++
				return
			}
			if name, line, ok := replyFileLocation(address); ok && !path.IsAbs(name) {
				address = path.Join(path.Dir(file), name)
				if line > 0 {
					address = fmt.Sprintf("%s:%d", address, line)
				}
			}
			w.replyThreadLink(target, address)
		}
		if base.LoadImage != nil {
			options.LoadImage = func(address string) (ui.ImageSource, error) {
				if name, err := replyImagePath(address); err == nil && !path.IsAbs(name) {
					address = path.Join(path.Dir(file), name)
				}
				return base.LoadImage(address)
			}
		}
		w.files.markdowns[key] = options
	}
	options.Theme = base.Theme
	options.Code = base.Code
	return options
}
