package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/muq9738/Vantry/pkg/api"
)

const maxUntrackedPreviewBytes = 64 << 10

// appendUntrackedDiff reads a bounded prefix through an os.Root so that a
// concurrently replaced path cannot expose a file outside the checkout.
func appendUntrackedDiff(ctx context.Context, root string, files []api.GitFile, diff *limitedOutput) (bool, error) {
	directory, err := os.OpenRoot(root)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	truncated, labeled := false, false
	for _, file := range files {
		if file.Index != "?" || file.Worktree != "?" {
			continue
		}
		if err := ctx.Err(); err != nil {
			return truncated, err
		}
		if !labeled {
			_, _ = diff.Write([]byte("\n[未跟踪 / untracked]\n"))
			labeled = true
		}
		if diff.truncated || diff.Len() == maxOutputBytes || diff.lines == maxOutputLines {
			return true, nil
		}
		if err := appendUntrackedFile(directory, file.Path, diff, &truncated); err != nil && !errors.Is(err, os.ErrNotExist) {
			_, _ = fmt.Fprintf(diff, "[无法预览未跟踪文件 %s：%v]\n", strconv.Quote(file.Path), err)
		}
	}
	return truncated, nil
}

func appendUntrackedFile(directory *os.Root, name string, diff *limitedOutput, truncated *bool) error {
	info, err := directory.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	// Nonblocking open also avoids waiting on a FIFO swapped in after Lstat.
	file, err := directory.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(file, maxUntrackedPreviewBytes+1))
	if err != nil {
		return err
	}
	partial := len(data) > maxUntrackedPreviewBytes
	if partial {
		data = data[:maxUntrackedPreviewBytes]
		// A multibyte character may straddle the preview boundary.
		for i := 0; i < utf8.UTFMax && len(data) > 0 && !utf8.Valid(data); i++ {
			data = data[:len(data)-1]
		}
	}
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil
	}
	mode := "100644"
	if info.Mode()&0o111 != 0 {
		mode = "100755"
	}
	oldPath, newPath := unifiedPath("a/"+name), unifiedPath("b/"+name)
	_, _ = fmt.Fprintf(diff, "diff --git %s %s\nnew file mode %s\n--- /dev/null\n+++ %s\n", oldPath, newPath, mode, newPath)
	lines := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		lines++
	}
	if lines > 0 {
		_, _ = fmt.Fprintf(diff, "@@ -0,0 +1,%d @@\n", lines)
		for len(data) > 0 && !diff.truncated {
			line, rest, newline := bytes.Cut(data, []byte("\n"))
			_, _ = diff.Write([]byte("+"))
			_, _ = diff.Write(line)
			_, _ = diff.Write([]byte("\n"))
			if !newline && !partial {
				_, _ = diff.Write([]byte("\\ No newline at end of file\n"))
			}
			data = rest
		}
	}
	if partial {
		*truncated = true
		_, _ = diff.Write([]byte("[未跟踪文件预览已截断：每个文件最多显示前 64 KiB。]\n"))
	}
	return nil
}

func unifiedPath(path string) string {
	if strings.ContainsAny(path, "\t\r\n\"\\") {
		return strconv.Quote(path)
	}
	return path
}
