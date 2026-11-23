package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/muq9738/Vantry/pkg/api"
)

// errAssetUnauthorized marks a download the GitHub token could not open, so
// the caller can retry without the token.
var errAssetUnauthorized = errors.New("更新下载未获授权")

// stageAsset downloads (preferring a self-verifying delta when available,
// falling back automatically to the full archive) and verifies a separate
// bundle. Even a writable target remains untouched while the application or
// Core may still be running.
func (m *Manager) stageAsset(ctx context.Context, asset, deltaAsset githubAsset, info api.AppUpdateInfo, progress func(int64, int64)) (StagedUpdate, error) {
	target := m.targetAppPath()
	// Prefer the same volume for the later offline rename. A non-writable
	// Applications directory still permits download to temporary storage.
	stageBesideTarget := m.goos() == "darwin" && target != "" && parentWritable(target)
	parent := os.TempDir()
	if stageBesideTarget {
		parent = filepath.Dir(target)
	}
	root, err := os.MkdirTemp(parent, ".vantry-update-")
	if err != nil {
		return StagedUpdate{}, fmt.Errorf("无法暂存更新: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(root)
		}
	}()

	payload := filepath.Join(root, "payload")
	var bundle string
	usedDelta := false
	usedAsset := asset

	if info.DeltaAvailable && deltaAsset.Name != "" && isExistingDir(target) {
		deltaBundle, deltaErr := m.tryStageDelta(ctx, root, payload, target, deltaAsset, info, progress)
		if deltaErr == nil {
			bundle = deltaBundle
			usedDelta = true
			usedAsset = deltaAsset
		} else {
			_ = os.RemoveAll(payload)
			if ctx.Err() != nil || asset.Name == "" {
				return StagedUpdate{}, deltaErr
			}
		}
	}

	if bundle == "" {
		if asset.Name == "" {
			return StagedUpdate{}, fmt.Errorf("发布 %s 没有适用于 %s-%s 的安装包", info.LatestVersion, m.goos(), m.goarch())
		}
		archive := filepath.Join(root, "download"+assetSuffix(asset.Name))
		if err := m.downloadAsset(ctx, asset, archive, progress); err != nil {
			return StagedUpdate{}, err
		}
		if err := os.MkdirAll(payload, 0o755); err != nil {
			return StagedUpdate{}, fmt.Errorf("无法创建解压目录: %w", err)
		}
		if err := m.extract(ctx, archive, asset.Name, payload, root); err != nil {
			return StagedUpdate{}, err
		}
		extracted, err := findStagedPayload(payload, m.goos())
		if err != nil {
			return StagedUpdate{}, err
		}
		if m.goos() == "darwin" {
			if err := m.verifyBundle(ctx, extracted); err != nil {
				return StagedUpdate{}, err
			}
		}
		bundle = extracted
		usedAsset = asset
	}

	staged := StagedUpdate{
		Version:    info.LatestVersion,
		StagedPath: bundle,
		TargetPath: target,
		UsedDelta:  usedDelta,
		AssetName:  usedAsset.Name,
		Size:       usedAsset.Size,
	}
	if m.goos() == "darwin" && target != "" {
		installer, err := prepareInstaller(root, bundle, target, m.opts.DataDir)
		if err != nil {
			return StagedUpdate{}, err
		}
		staged.InstallerPath = installer
	}
	staged.CanAutoRestart = m.canAutoRestart(staged)

	keep = true
	m.mu.Lock()
	prevRoot := m.stagedRoot
	copyStaged := staged
	m.staged = &copyStaged
	m.stagedRoot = root
	m.mergeStagedLocked(&m.info)
	m.mu.Unlock()

	if prevRoot != "" && prevRoot != root &&
		strings.HasPrefix(filepath.Base(prevRoot), ".vantry-update-") &&
		!within(prevRoot, staged.StagedPath) &&
		(target == "" || !within(prevRoot, target)) {
		_ = os.RemoveAll(prevRoot)
	}
	return staged, nil
}

func (m *Manager) tryStageDelta(ctx context.Context, root, payload, target string, deltaAsset githubAsset, info api.AppUpdateInfo, progress func(int64, int64)) (string, error) {
	deltaArchive := filepath.Join(root, "update.delta")
	if err := m.downloadAsset(ctx, deltaAsset, deltaArchive, progress); err != nil {
		return "", err
	}
	defer os.Remove(deltaArchive)

	if err := os.MkdirAll(payload, 0o755); err != nil {
		return "", fmt.Errorf("无法创建增量解压目录: %w", err)
	}
	deltaDest := filepath.Join(payload, filepath.Base(target))
	file, err := os.Open(deltaArchive)
	if err != nil {
		return "", fmt.Errorf("打开增量更新包失败: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("检查增量更新包失败: %w", err)
	}
	if err := ApplyDelta(file, stat.Size(), m.currentVersion(), info.LatestVersion, target, deltaDest); err != nil {
		return "", err
	}
	if m.goos() == "darwin" {
		if err := m.verifyBundle(ctx, deltaDest); err != nil {
			return "", err
		}
	}
	return deltaDest, nil
}

func findStagedPayload(payload, goos string) (string, error) {
	if goos == "darwin" {
		return findAppBundle(payload)
	}
	if bundle, err := findAppBundle(payload); err == nil {
		return bundle, nil
	}
	entries, err := os.ReadDir(payload)
	if err != nil {
		return "", fmt.Errorf("读取更新包内容失败: %w", err)
	}
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(payload, entries[0].Name()), nil
	}
	if len(entries) > 0 {
		return payload, nil
	}
	return "", errors.New("更新包内容为空")
}

func isExistingDir(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func pathExists(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (m *Manager) canAutoRestart(staged StagedUpdate) bool {
	return m.goos() == "darwin" && staged.TargetPath != "" && staged.StagedPath != "" && parentWritable(staged.TargetPath)
}

func (m *Manager) downloadAsset(ctx context.Context, asset githubAsset, dest string, progress func(int64, int64)) error {
	if token := m.githubToken(ctx); token != "" {
		err := m.fetchAsset(ctx, asset, dest, token, progress)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errAssetUnauthorized) {
			return err
		}
		m.forgetToken()
	}
	return m.fetchAsset(ctx, asset, dest, "", progress)
}

// fetchAsset downloads the asset and checks that the bytes received match
// the size GitHub reports and the Content-Length the server sends.
func (m *Manager) fetchAsset(ctx context.Context, asset githubAsset, dest, token string, progress func(int64, int64)) error {
	endpoint := assetDownloadURL(asset, token)
	if endpoint == "" {
		return errors.New("发布资产缺少下载地址")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("构造下载请求失败: %w", err)
	}
	request.Header.Set("User-Agent", m.userAgent())
	request.Header.Set("Accept", "application/octet-stream")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := m.client.Do(request)
	if err != nil {
		return fmt.Errorf("下载更新失败: %w", err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("%w: %s", errAssetUnauthorized, response.Status)
	default:
		io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("下载更新失败: %s", response.Status)
	}

	total := asset.Size
	if response.ContentLength > 0 {
		if total > 0 && response.ContentLength != total {
			return fmt.Errorf("更新包大小不一致: 发布声明 %d 字节，服务器返回 %d 字节", total, response.ContentLength)
		}
		total = response.ContentLength
	}
	if total > maxAssetSize {
		return fmt.Errorf("更新包超过大小上限: %d 字节", total)
	}
	file, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("无法写入更新包: %w", err)
	}
	reader := &progressReader{
		reader:   io.LimitReader(response.Body, maxAssetSize+1),
		total:    total,
		progress: progress,
	}
	written, copyErr := io.Copy(file, reader)
	closeErr := file.Close()
	if copyErr != nil {
		return fmt.Errorf("下载更新包失败: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("写入更新包失败: %w", closeErr)
	}
	if written > maxAssetSize {
		return errors.New("更新包超过大小上限")
	}
	if total > 0 && written != total {
		return fmt.Errorf("更新包不完整: 收到 %d 字节，期望 %d 字节", written, total)
	}
	progress(written, written)
	return nil
}

func assetDownloadURL(asset githubAsset, token string) string {
	// The API asset endpoint needs the Authorization header for private
	// repositories; the browser URL is public.
	if token != "" && asset.URL != "" {
		return asset.URL
	}
	if asset.BrowserDownloadURL != "" {
		return asset.BrowserDownloadURL
	}
	return asset.URL
}

type progressReader struct {
	reader   io.Reader
	read     int64
	total    int64
	progress func(int64, int64)
}

func (r *progressReader) Read(buffer []byte) (int, error) {
	n, err := r.reader.Read(buffer)
	r.read += int64(n)
	if r.progress != nil {
		r.progress(r.read, r.total)
	}
	return n, err
}

func assetSuffix(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"):
		return ".tar.gz"
	case strings.HasSuffix(lower, ".tgz"):
		return ".tgz"
	case strings.HasSuffix(lower, ".zip"):
		return ".zip"
	case strings.HasSuffix(lower, ".dmg"):
		return ".dmg"
	default:
		return ".bin"
	}
}

func (m *Manager) extract(ctx context.Context, archive, name, dest, root string) error {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz"):
		return extractTarGz(archive, dest)
	case strings.HasSuffix(lower, ".zip"):
		return extractZip(archive, dest)
	case strings.HasSuffix(lower, ".dmg"):
		return m.extractDMG(ctx, archive, dest, root)
	default:
		return fmt.Errorf("不支持的更新包格式: %s", filepath.Base(name))
	}
}

func extractTarGz(path, dest string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("打开更新包失败: %w", err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("更新包不是有效的 .tar.gz: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var total int64
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取更新包失败: %w", err)
		}
		target, err := safeJoin(dest, header.Name)
		if err != nil {
			return err
		}
		mode := fs.FileMode(header.Mode).Perm()
		if mode == 0 {
			mode = 0o644
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("创建更新目录失败: %w", err)
			}
		case tar.TypeReg, tar.TypeRegA:
			if header.Size < 0 || header.Size > maxSingleFile {
				return fmt.Errorf("更新包文件过大: %s", header.Name)
			}
			total += header.Size
			if total > maxExtractSize {
				return errors.New("更新包解压后超过大小上限")
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("创建更新目录失败: %w", err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return fmt.Errorf("写入更新文件失败: %w", err)
			}
			_, copyErr := io.CopyN(out, reader, header.Size)
			closeErr := out.Close()
			if copyErr != nil {
				return fmt.Errorf("写入更新文件失败: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("写入更新文件失败: %w", closeErr)
			}
		case tar.TypeSymlink:
			if err := writeSymlink(dest, target, header.Linkname); err != nil {
				return err
			}
		case tar.TypeLink:
			linkTarget, err := safeJoin(dest, header.Linkname)
			if err != nil {
				return err
			}
			if err := os.Link(linkTarget, target); err != nil {
				return fmt.Errorf("创建更新硬链接失败: %w", err)
			}
		}
	}
}

func extractZip(path, dest string) error {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return fmt.Errorf("更新包不是有效的 .zip: %w", err)
	}
	defer reader.Close()
	var total int64
	for _, entry := range reader.File {
		target, err := safeJoin(dest, entry.Name)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("创建更新目录失败: %w", err)
			}
		case mode&os.ModeSymlink != 0:
			source, err := entry.Open()
			if err != nil {
				return fmt.Errorf("读取更新包失败: %w", err)
			}
			link, readErr := io.ReadAll(io.LimitReader(source, 4096))
			source.Close()
			if readErr != nil {
				return fmt.Errorf("读取更新包失败: %w", readErr)
			}
			if err := writeSymlink(dest, target, string(link)); err != nil {
				return err
			}
		case mode.IsRegular():
			if entry.UncompressedSize64 > maxSingleFile {
				return fmt.Errorf("更新包文件过大: %s", entry.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("创建更新目录失败: %w", err)
			}
			source, err := entry.Open()
			if err != nil {
				return fmt.Errorf("读取更新包失败: %w", err)
			}
			permission := mode.Perm()
			if permission == 0 {
				permission = 0o644
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, permission)
			if err != nil {
				source.Close()
				return fmt.Errorf("写入更新文件失败: %w", err)
			}
			written, copyErr := io.Copy(out, io.LimitReader(source, maxSingleFile+1))
			closeErr := out.Close()
			source.Close()
			if copyErr != nil {
				return fmt.Errorf("写入更新文件失败: %w", copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("写入更新文件失败: %w", closeErr)
			}
			if written > maxSingleFile {
				return fmt.Errorf("更新包文件过大: %s", entry.Name)
			}
			total += written
			if total > maxExtractSize {
				return errors.New("更新包解压后超过大小上限")
			}
		}
	}
	return nil
}

func (m *Manager) extractDMG(ctx context.Context, archive, dest, root string) error {
	if m.goos() != "darwin" {
		return errors.New("只有 macOS 可以安装 .dmg 更新")
	}
	mount := filepath.Join(root, "mount")
	if err := os.MkdirAll(mount, 0o755); err != nil {
		return fmt.Errorf("无法创建挂载点: %w", err)
	}
	if err := m.runExternal(ctx, "hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", mount, archive); err != nil {
		return fmt.Errorf("挂载磁盘镜像失败: %w", err)
	}
	defer func() {
		detachCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = m.runExternal(detachCtx, "hdiutil", "detach", mount)
	}()
	entries, err := os.ReadDir(mount)
	if err != nil {
		return fmt.Errorf("读取磁盘镜像失败: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasSuffix(strings.ToLower(entry.Name()), ".app") {
			return copyTree(filepath.Join(mount, entry.Name()), filepath.Join(dest, entry.Name()))
		}
	}
	return errors.New("磁盘镜像中没有 .app 应用")
}

func copyTree(source, dest string) error {
	var total int64
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return fmt.Errorf("创建应用目录失败: %w", err)
			}
			return nil
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("读取应用符号链接失败: %w", err)
			}
			if filepath.IsAbs(link) || filepath.VolumeName(link) != "" {
				return fmt.Errorf("应用包含绝对符号链接: %s", link)
			}
			resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(link))
			if !within(dest, resolved) {
				return fmt.Errorf("应用符号链接越界: %s", link)
			}
			return os.Symlink(filepath.FromSlash(link), target)
		case info.Mode().IsRegular():
			total += info.Size()
			if total > maxExtractSize {
				return errors.New("应用内容超过大小上限")
			}
			return copyFile(path, target, info.Mode().Perm())
		default:
			return nil
		}
	})
}

func copyFile(source, dest string, permission fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("创建应用目录失败: %w", err)
	}
	in, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("读取应用文件失败: %w", err)
	}
	defer in.Close()
	if permission == 0 {
		permission = 0o644
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, permission)
	if err != nil {
		return fmt.Errorf("写入应用文件失败: %w", err)
	}
	_, copyErr := io.Copy(out, io.LimitReader(in, maxSingleFile+1))
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("写入应用文件失败: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("写入应用文件失败: %w", closeErr)
	}
	return nil
}

// findAppBundle returns the shallowest .app directory of the extracted
// payload.
func findAppBundle(root string) (string, error) {
	found := ""
	bestDepth := -1
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		depth := 0
		if relative != "." {
			depth = len(strings.Split(relative, string(filepath.Separator)))
		}
		if strings.HasSuffix(strings.ToLower(entry.Name()), ".app") {
			if bestDepth < 0 || depth < bestDepth {
				bestDepth = depth
				found = path
			}
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("读取更新包内容失败: %w", err)
	}
	if found == "" {
		return "", errors.New("更新包中没有 .app 应用")
	}
	return found, nil
}

// safeJoin resolves an archive entry under root, rejecting absolute names
// and ".." traversal.
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", errors.New("更新包包含空路径")
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("更新包包含非法路径: %q", name)
	}
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" ||
		clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("更新包路径越界: %s", name)
	}
	if clean == "." {
		return root, nil
	}
	target := filepath.Join(root, clean)
	if !within(root, target) {
		return "", fmt.Errorf("更新包路径越界: %s", name)
	}
	return target, nil
}

func within(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}

func writeSymlink(root, target, linkName string) error {
	if linkName == "" {
		return errors.New("更新包包含空符号链接")
	}
	if filepath.IsAbs(linkName) || filepath.VolumeName(linkName) != "" {
		return fmt.Errorf("更新包包含绝对符号链接: %s", linkName)
	}
	resolved := filepath.Join(filepath.Dir(target), filepath.FromSlash(linkName))
	if !within(root, resolved) {
		return fmt.Errorf("更新包符号链接越界: %s", linkName)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("创建更新目录失败: %w", err)
	}
	return os.Symlink(filepath.FromSlash(linkName), target)
}

// verifyBundle requires Apple's code signature to be valid for the staged
// bundle. macOS refuses to run a modified or unsigned copy.
func (m *Manager) verifyBundle(ctx context.Context, bundle string) error {
	if err := m.runExternal(ctx, "codesign", "--verify", "--deep", "--strict", bundle); err != nil {
		return fmt.Errorf("应用签名校验失败: %w", err)
	}
	return nil
}

func (m *Manager) runExternal(ctx context.Context, name string, args ...string) error {
	if m.opts.Runner != nil {
		return m.opts.Runner(ctx, name, args...)
	}
	command := exec.CommandContext(ctx, name, args...)
	var stderr cappedError
	command.Stderr = &stderr
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("%s: %w (%.200s)", name, err, detail)
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

type cappedError struct {
	builder strings.Builder
}

func (c *cappedError) Write(p []byte) (int, error) {
	if room := 4096 - c.builder.Len(); room > 0 {
		if len(p) > room {
			c.builder.Write(p[:room])
		} else {
			c.builder.Write(p)
		}
	}
	return len(p), nil
}

func (c *cappedError) String() string { return c.builder.String() }

func parentWritable(target string) bool {
	probe, err := os.MkdirTemp(filepath.Dir(target), ".vantry-write-")
	if err != nil {
		return false
	}
	os.Remove(probe)
	return true
}

// swapApp replaces target with staged, keeping the previous bundle until
// the new one is in place so a failure can restore it.
func swapApp(staged, target string) error {
	parent := filepath.Dir(target)
	backup, err := os.MkdirTemp(parent, "."+filepath.Base(target)+".backup-")
	if err != nil {
		return fmt.Errorf("无法备份现有应用: %w", err)
	}
	if err := os.Remove(backup); err != nil {
		return fmt.Errorf("无法准备备份目录: %w", err)
	}
	moved := false
	if _, err := os.Lstat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("无法备份现有应用: %w", err)
		}
		moved = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("无法检查现有应用: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		if moved {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("无法替换应用: %w", err)
	}
	if moved {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func isAuthStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound
}
