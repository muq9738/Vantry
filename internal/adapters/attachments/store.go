package attachments

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/muq9738/Vantry/internal/hostfs"
	"github.com/muq9738/Vantry/pkg/api"
)

const MaxSize = 8 << 20
const maxMetadataSize = 4096

type Store struct{ Directory string }

func validName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 &&
		utf8.ValidString(name) && !strings.ContainsAny(name, "/\\") &&
		strings.IndexFunc(name, unicode.IsControl) == -1
}

func validAttachment(item api.Attachment) error {
	if !strings.HasPrefix(item.ID, "file_") || len(item.ID) <= len("file_") || len(item.ID) > 128 {
		return errors.New("附件 ID 无效")
	}
	for _, c := range strings.TrimPrefix(item.ID, "file_") {
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return errors.New("附件 ID 无效")
	}
	if !validName(item.Name) {
		return errors.New("附件名无效")
	}
	if item.Size < 1 || item.Size > MaxSize {
		return errors.New("附件大小超出允许范围")
	}
	if _, _, err := mime.ParseMediaType(item.MIME); err != nil || len(item.MIME) > 1024 {
		return errors.New("附件 MIME 无效")
	}
	return nil
}

func privateDirectory(path string, info os.FileInfo) bool {
	return info.IsDir() && info.Mode()&os.ModeSymlink == 0 && hostfs.CheckPrivateDir(path, false) == nil
}

func (s Store) open(create bool) (*os.Root, error) {
	if s.Directory == "" {
		return nil, errors.New("附件目录不能为空")
	}
	path, err := filepath.Abs(s.Directory)
	if err != nil {
		return nil, err
	}
	if create {
		if err := hostfs.EnsurePrivateDir(path); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !privateDirectory(path, info) {
		return nil, errors.New("附件存储必须是私有目录，不能是符号链接")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		_ = root.Close()
		return nil, errors.New("附件存储目录已改变")
	}
	return root, nil
}

func writeNew(root *os.Root, path string, data []byte) error {
	file, err := root.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	return errors.Join(writeErr, file.Close())
}

func (s Store) Import(name string, data []byte) (api.Attachment, error) {
	// Keep the basename behavior for clients passing an original POSIX path.
	// The resulting name is a display/extension field, never an ID or directory.
	name = filepath.Base(name)
	if !validName(name) {
		return api.Attachment{}, errors.New("附件名无效")
	}
	if len(data) == 0 || len(data) > MaxSize {
		return api.Attachment{}, fmt.Errorf("附件必须在 1 字节至 %d MiB 之间", MaxSize>>20)
	}
	item := api.Attachment{ID: "file_" + rand.Text(), Name: name, Size: int64(len(data)), MIME: http.DetectContentType(data)}
	if declared := mime.TypeByExtension(filepath.Ext(name)); declared != "" {
		item.MIME = declared
	}
	if err := validAttachment(item); err != nil {
		return api.Attachment{}, err
	}
	root, err := s.open(true)
	if err != nil {
		return api.Attachment{}, err
	}
	defer root.Close()
	if err := root.Mkdir(item.ID, 0700); err != nil {
		return api.Attachment{}, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = root.RemoveAll(item.ID)
		}
	}()
	if err := root.Mkdir(filepath.Join(item.ID, "content"), 0700); err != nil {
		return api.Attachment{}, err
	}
	if err := writeNew(root, filepath.Join(item.ID, "content", name), data); err != nil {
		return api.Attachment{}, err
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return api.Attachment{}, err
	}
	if err := writeNew(root, filepath.Join(item.ID, "metadata.json"), raw); err != nil {
		return api.Attachment{}, err
	}
	complete = true
	return item, nil
}

func openRegular(root *os.Root, path string, limit int64) (*os.File, os.FileInfo, error) {
	info, err := root.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, nil, errors.New("附件文件类型或大小无效")
	}
	// Root confines directory traversal; NOFOLLOW rejects a substituted final
	// symlink and NONBLOCK prevents a substituted FIFO from hanging Core.
	file, err := hostfs.OpenRootNoFollow(root, path)
	if err != nil {
		return nil, nil, err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		_ = file.Close()
		return nil, nil, errors.New("附件文件已不可用")
	}
	return file, info, nil
}

func (s Store) Resolve(item api.Attachment) (string, error) {
	if err := validAttachment(item); err != nil {
		return "", err
	}
	root, err := s.open(false)
	if err != nil {
		return "", err
	}
	defer root.Close()
	for _, directory := range []string{item.ID, filepath.Join(item.ID, "content")} {
		info, err := root.Lstat(directory)
		if err != nil {
			return "", err
		}
		if !privateDirectory(filepath.Join(root.Name(), directory), info) {
			return "", errors.New("附件目录已不可用")
		}
	}
	metadata, _, err := openRegular(root, filepath.Join(item.ID, "metadata.json"), maxMetadataSize)
	if err != nil {
		return "", err
	}
	raw, readErr := io.ReadAll(io.LimitReader(metadata, maxMetadataSize+1))
	if err := errors.Join(readErr, metadata.Close()); err != nil {
		return "", err
	}
	if len(raw) > maxMetadataSize {
		return "", errors.New("附件元数据过大")
	}
	var stored api.Attachment
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", err
	}
	if err := validAttachment(stored); err != nil {
		return "", err
	}
	if stored != item {
		return "", errors.New("附件信息与上传记录不符")
	}
	path := filepath.Join(item.ID, "content", stored.Name)
	file, info, err := openRegular(root, path, MaxSize)
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if info.Size() != stored.Size {
		return "", errors.New("附件大小与上传记录不符")
	}
	return filepath.Join(root.Name(), path), nil
}

func (s Store) Validate(items []api.Attachment) error {
	for _, item := range items {
		if _, err := s.Resolve(item); err != nil {
			return &api.Error{Code: api.InvalidCommand, Message: "附件不可用：" + err.Error()}
		}
	}
	return nil
}
