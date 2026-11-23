package attachments

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/muq9738/Vantry/pkg/api"
)

func TestResolveRejectsSymlinksAtEveryStoredLevel(t *testing.T) {
	for _, component := range []string{"store", "item", "metadata", "content", "file"} {
		t.Run(component, func(t *testing.T) {
			root := t.TempDir()
			store := Store{Directory: filepath.Join(root, "attachments")}
			item, err := store.Import("note.txt", []byte("copy"))
			if err != nil {
				t.Fatal(err)
			}
			path := map[string]string{
				"store":    store.Directory,
				"item":     filepath.Join(store.Directory, item.ID),
				"metadata": filepath.Join(store.Directory, item.ID, "metadata.json"),
				"content":  filepath.Join(store.Directory, item.ID, "content"),
				"file":     filepath.Join(store.Directory, item.ID, "content", item.Name),
			}[component]
			moved := filepath.Join(root, "elsewhere")
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, path); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Resolve(item); err == nil {
				t.Fatal("symlink accepted: " + component)
			}
		})
	}
}

func TestResolveRejectsInvalidStoredSizeAndMetadata(t *testing.T) {
	for _, test := range []struct {
		name string
		size int64
		raw  string
	}{
		{name: "empty", size: 0},
		{name: "oversized", size: MaxSize + 1},
		{name: "large-metadata", size: 4, raw: strings.Repeat(" ", 1<<20)},
		{name: "malformed-metadata", size: 4, raw: "{"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := Store{Directory: filepath.Join(t.TempDir(), "store")}
			item, err := store.Import("note.txt", []byte("copy"))
			if err != nil {
				t.Fatal(err)
			}
			item.Size = test.size
			path := filepath.Join(store.Directory, item.ID)
			if err := os.Truncate(filepath.Join(path, "content", item.Name), item.Size); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(item)
			if test.raw != "" {
				raw = append([]byte(test.raw), raw...)
			}
			if err := os.WriteFile(filepath.Join(path, "metadata.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Resolve(item); err == nil {
				t.Fatal("invalid stored attachment accepted")
			}
		})
	}
}

func TestImportRejectsUnsafeNamesAndNonPrivateStore(t *testing.T) {
	store := Store{Directory: t.TempDir()}
	if err := os.Chmod(store.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"", ".", "..", "/", "line\n.txt", "nul\x00.txt", "bad\\name.txt", "tab\t.txt", string([]byte{0xff})} {
		if _, err := store.Import(name, []byte("copy")); err == nil {
			t.Errorf("unsafe name accepted: %q", name)
		}
	}
	if err := os.Chmod(store.Directory, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Import("note.txt", []byte("copy")); err == nil {
		t.Fatal("public store accepted")
	}
}

func TestResolveRejectsInvalidIDsAndChangedConsumerFields(t *testing.T) {
	store := Store{Directory: filepath.Join(t.TempDir(), "store")}
	item, err := store.Import("note.txt", []byte("copy"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "file_", "file_../other", "file_a/b", "file_a\\b", "file_a\x00", "file_a\n", "file_" + strings.Repeat("x", 200)} {
		changed := item
		changed.ID = id
		if _, err := store.Resolve(changed); err == nil {
			t.Errorf("invalid ID accepted: %q", id)
		}
	}
	for _, change := range []func(*api.Attachment){
		func(a *api.Attachment) { a.Name = "other.txt" },
		func(a *api.Attachment) { a.Size++ },
		func(a *api.Attachment) { a.MIME = "image/png" },
	} {
		changed := item
		change(&changed)
		if _, err := store.Resolve(changed); err == nil {
			t.Errorf("changed fields accepted: %+v", changed)
		}
	}
}

func TestResolveRejectsMissingMetadataAndSpecialFiles(t *testing.T) {
	for _, component := range []string{"metadata.json", "content/note.txt"} {
		t.Run(component, func(t *testing.T) {
			store := Store{Directory: filepath.Join(t.TempDir(), "store")}
			item, err := store.Import("note.txt", []byte("copy"))
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.Directory, item.ID, component)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Resolve(item); err == nil {
				t.Fatal("missing file accepted")
			}
			if err := syscall.Mkfifo(path, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Resolve(item); err == nil {
				t.Fatal("FIFO accepted")
			}
		})
	}
}

func TestImportSizeBoundaryAndIndependentIDs(t *testing.T) {
	t.Chdir(t.TempDir())
	store := Store{Directory: "store"}
	for _, size := range []int{0, MaxSize + 1} {
		if _, err := store.Import("note.txt", make([]byte, size)); err == nil {
			t.Errorf("invalid upload size accepted: %d", size)
		}
	}
	item, err := store.Import("note.txt", make([]byte, MaxSize))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Resolve(item)
	if err != nil || !filepath.IsAbs(path) {
		t.Fatalf("relative store did not resolve an absolute path: %q, %v", path, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != MaxSize {
		t.Fatalf("size boundary: %v, %v", info, err)
	}
	second, err := store.Import("note.txt", make([]byte, MaxSize))
	if err != nil || second.ID == item.ID {
		t.Fatalf("two uploads share an ID: %+v, %v", second, err)
	}
}

func TestCopySurvivesOriginalChangesAndRejectsMissingContent(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original.txt")
	if err := os.WriteFile(original, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Directory: filepath.Join(root, "store")}
	item, err := store.Import(filepath.Base(original), data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(original, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	path, err := store.Resolve(item)
	if err != nil || !filepath.IsAbs(path) {
		t.Fatalf("resolve copy: %s, %v", path, err)
	}
	copied, err := os.ReadFile(path)
	if err != nil || string(copied) != "original" {
		t.Fatalf("copy followed original: %q, %v", copied, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := store.Validate([]api.Attachment{item}); err == nil {
		t.Fatal("missing file validated")
	}
}
