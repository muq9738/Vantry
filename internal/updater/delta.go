package updater

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
)

const (
	deltaMagic     = "vantry-delta-1\n"
	maxDeltaDiff   = 256 << 20
	maxDeltaIndex  = 16 << 20
	stapledTicket  = "Contents/CodeResources"
)

var errDamagedDelta = errors.New("增量更新包已损坏")

type deltaIndex struct {
	From    string       `json:"from"`
	Version string       `json:"version"`
	Entries []deltaEntry `json:"entries"`
}

type deltaEntry struct {
	Path   string `json:"path"`
	Dir    bool   `json:"dir,omitempty"`
	Link   string `json:"link,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
	From   string `json:"from,omitempty"`
	Data   int64  `json:"data,omitempty"`
}

func (e *deltaEntry) isFile() bool { return !e.Dir && e.Link == "" }

// WriteDelta writes a self-verifying binary delta patch transforming oldDir
// (version from) into newDir (version to).
func WriteDelta(w io.Writer, from, to, oldDir, newDir string) error {
	from = normalizeVersion(from)
	to = normalizeVersion(to)
	if from == "" || to == "" {
		return errors.New("生成增量包需要明确的源版本与目标版本")
	}

	type oldFile struct {
		sum  string
		size int64
	}
	oldFiles := make(map[string]oldFile)
	bySum := make(map[string]string)
	err := filepath.WalkDir(oldDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || !d.Type().IsRegular() {
			return walkErr
		}
		rel := deltaRelPath(oldDir, p)
		if rel == "." || rel == stapledTicket {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		size, err := io.Copy(h, f)
		if err != nil {
			return err
		}
		sum := hex.EncodeToString(h.Sum(nil))
		oldFiles[rel] = oldFile{sum: sum, size: size}
		if _, ok := bySum[sum]; !ok {
			bySum[sum] = rel
		}
		return nil
	})
	if err != nil {
		return err
	}

	topEntries, err := os.ReadDir(newDir)
	if err != nil {
		return err
	}
	slices.SortFunc(topEntries, func(a, b os.DirEntry) int {
		if a.Name() < b.Name() {
			return -1
		}
		if a.Name() > b.Name() {
			return 1
		}
		return 0
	})

	index := deltaIndex{From: from, Version: to}
	var blobs [][]byte
	for _, top := range topEntries {
		err := filepath.WalkDir(filepath.Join(newDir, top.Name()), func(p string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			e := deltaEntry{Path: deltaRelPath(newDir, p)}
			switch {
			case d.IsDir():
				e.Dir = true
			case d.Type()&fs.ModeSymlink != 0:
				link, err := os.Readlink(p)
				if err != nil {
					return err
				}
				if filepath.IsAbs(link) || filepath.VolumeName(link) != "" {
					return fmt.Errorf("增量包不支持绝对符号链接: %s", link)
				}
				e.Link = filepath.ToSlash(link)
			case d.Type().IsRegular():
				info, err := d.Info()
				if err != nil {
					return err
				}
				raw, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				sum := sha256.Sum256(raw)
				e.Mode = uint32(info.Mode().Perm())
				e.Size = int64(len(raw))
				e.SHA256 = hex.EncodeToString(sum[:])

				prevMeta, hasSamePath := oldFiles[e.Path]
				switch {
				case e.Path != stapledTicket && hasSamePath && prevMeta.sum == e.SHA256:
					e.From = e.Path
				case e.Path != stapledTicket && bySum[e.SHA256] != "":
					e.From = bySum[e.SHA256]
				default:
					blob := deflateBytes(raw)
					if e.Path != stapledTicket && hasSamePath && prevMeta.size > 0 && prevMeta.size <= maxDeltaDiff && len(raw) <= maxDeltaDiff {
						prevBytes, err := os.ReadFile(filepath.Join(oldDir, filepath.FromSlash(e.Path)))
						if err != nil {
							return err
						}
						if patch := bsdiffDiff(prevBytes, raw); len(patch) < len(blob) {
							e.From = e.Path
							blob = patch
						}
					}
					e.Data = int64(len(blob))
					blobs = append(blobs, blob)
				}
			default:
				return fmt.Errorf("增量包仅支持普通文件、目录和符号链接: %s", p)
			}
			index.Entries = append(index.Entries, e)
			return nil
		})
		if err != nil {
			return err
		}
	}

	rawIndex, err := json.Marshal(index)
	if err != nil {
		return err
	}
	compressedIndex := deflateBytes(rawIndex)
	header := binary.AppendUvarint([]byte(deltaMagic), uint64(len(compressedIndex)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(compressedIndex); err != nil {
		return err
	}
	for _, blob := range blobs {
		if _, err := w.Write(blob); err != nil {
			return err
		}
	}
	return nil
}

func deltaRelPath(dir, p string) string {
	rel, _ := filepath.Rel(dir, p)
	return filepath.ToSlash(rel)
}

// ApplyDelta reconstructs newDir (which must not exist yet) from oldDir and
// the delta payload read from r (of size bytes), verifying every entry's size
// and SHA-256 digest.
func ApplyDelta(r io.ReaderAt, size int64, from, to, oldDir, newDir string) error {
	if size <= int64(len(deltaMagic)) || size > maxAssetSize {
		return errors.New("不是有效的增量更新包")
	}
	var head [len(deltaMagic) + binary.MaxVarintLen64]byte
	n, _ := r.ReadAt(head[:min(size, int64(len(head)))], 0)
	if !bytes.HasPrefix(head[:n], []byte(deltaMagic)) {
		return errors.New("不是有效的增量更新包")
	}
	indexLen, k := binary.Uvarint(head[len(deltaMagic):n])
	start := int64(len(deltaMagic)) + int64(k)
	if k <= 0 || indexLen > uint64(size-start) {
		return errDamagedDelta
	}

	indexReader := flate.NewReader(io.NewSectionReader(r, start, int64(indexLen)))
	defer indexReader.Close()
	rawIndex, err := io.ReadAll(io.LimitReader(indexReader, maxDeltaIndex+1))
	if err != nil || len(rawIndex) > maxDeltaIndex {
		return errDamagedDelta
	}
	var index deltaIndex
	if err := json.Unmarshal(rawIndex, &index); err != nil {
		return errDamagedDelta
	}
	if normalizeVersion(index.From) != normalizeVersion(from) || normalizeVersion(index.Version) != normalizeVersion(to) {
		return fmt.Errorf("增量包适用于 %s -> %s，不能用于 %s -> %s", index.From, index.Version, from, to)
	}

	offset := start + int64(indexLen)
	seen := make(map[string]bool, len(index.Entries))
	var totalExtracted int64
	for _, e := range index.Entries {
		if !fs.ValidPath(e.Path) || e.Path == "." || seen[e.Path] ||
			e.Size < 0 || e.Size > maxSingleFile || e.Data < 0 || e.Data > size-offset ||
			(e.Dir && e.Link != "") ||
			(!e.isFile() && (e.Size != 0 || e.Data != 0 || e.From != "")) ||
			(e.From != "" && !fs.ValidPath(e.From)) ||
			(e.isFile() && e.From == "" && e.Data == 0) {
			return errDamagedDelta
		}
		if e.Link != "" {
			if path.IsAbs(e.Link) || filepath.VolumeName(e.Link) != "" {
				return fmt.Errorf("增量包包含越界符号链接: %s", e.Path)
			}
			target := path.Join(path.Dir(e.Path), e.Link)
			if !fs.ValidPath(target) {
				return fmt.Errorf("增量包包含越界符号链接: %s", e.Path)
			}
		}
		if e.isFile() {
			totalExtracted += e.Size
			if totalExtracted > maxExtractSize {
				return errors.New("增量包解压后超过大小上限")
			}
		}
		seen[e.Path] = true
		offset += e.Data
	}
	if offset != size {
		return errDamagedDelta
	}

	if err := os.Mkdir(newDir, 0o755); err != nil {
		return err
	}
	oldRoot, err := os.OpenRoot(oldDir)
	if err != nil {
		return err
	}
	defer oldRoot.Close()
	newRoot, err := os.OpenRoot(newDir)
	if err != nil {
		return err
	}
	defer newRoot.Close()

	offset = start + int64(indexLen)
	for _, e := range index.Entries {
		name := filepath.FromSlash(e.Path)
		if parent := filepath.Dir(name); parent != "." {
			if err := newRoot.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		switch {
		case e.Dir:
			err = newRoot.MkdirAll(name, 0o755)
		case e.Link != "":
			err = newRoot.Symlink(filepath.FromSlash(e.Link), name)
		default:
			err = applyDeltaFile(e, io.NewSectionReader(r, offset, e.Data), oldRoot, newRoot)
			offset += e.Data
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func applyDeltaFile(e deltaEntry, data *io.SectionReader, oldRoot, newRoot *os.Root) error {
	rel := filepath.FromSlash(e.Path)
	perm := fs.FileMode(e.Mode).Perm()
	if perm == 0 {
		perm = 0o644
	}
	outFile, err := newRoot.OpenFile(rel, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm|0o200)
	if err != nil {
		return err
	}
	defer outFile.Close()

	h := sha256.New()
	limited := &deltaLimitWriter{w: io.MultiWriter(outFile, h), left: e.Size}
	if e.From == "" {
		reader := flate.NewReader(data)
		_, err = io.Copy(limited, reader)
		if closeErr := reader.Close(); err == nil {
			err = closeErr
		}
	} else {
		var src *os.File
		src, err = oldRoot.Open(filepath.FromSlash(e.From))
		if err != nil {
			return err
		}
		defer src.Close()
		var info fs.FileInfo
		info, err = src.Stat()
		switch {
		case err != nil:
		case !info.Mode().IsRegular():
			err = fmt.Errorf("本地基准文件不是普通文件: %s", e.From)
		case e.Data == 0:
			_, err = io.Copy(limited, src)
		default:
			err = bsdiffPatch(limited, e.Size, src, info.Size(), data, e.Data)
		}
	}
	if err == nil {
		err = outFile.Close()
	}
	if errors.Is(err, errDamagedPatch) || errors.Is(err, errDeltaTooLong) ||
		(err == nil && (limited.left != 0 || hex.EncodeToString(h.Sum(nil)) != e.SHA256)) {
		return fmt.Errorf("增量更新重建文件校验失败: %s", e.Path)
	}
	if err != nil {
		return err
	}
	return newRoot.Chmod(rel, perm)
}

var errDeltaTooLong = errors.New("输出超过声明大小")

type deltaLimitWriter struct {
	w    io.Writer
	left int64
}

func (l *deltaLimitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		return 0, errDeltaTooLong
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}
