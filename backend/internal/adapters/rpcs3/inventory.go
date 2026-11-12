package rpcs3

import (
	"crypto/sha256"
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
	"strings"
	"unicode/utf8"
)

const (
	maxInstalledEntries  = 1_000_000
	maxInstalledBytes    = int64(64 << 30)
	maxInstalledManifest = 8 << 20
)

type installedFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type installedInventory struct {
	Files       []installedFile `json:"files"`
	Directories []string        `json:"directories"`
}

// validateInstalledInventory validates the journal before any filesystem access.
// The title root is implicit; every other parent must be an inventoried directory.
func validateInstalledInventory(inv installedInventory) error {
	if len(inv.Files) == 0 || len(inv.Files) > maxInstalledEntries || len(inv.Directories) > maxInstalledEntries-len(inv.Files) {
		return fmt.Errorf("invalid installed title inventory size")
	}
	all := make(map[string]bool, len(inv.Files)+len(inv.Directories))
	dirs := make(map[string]bool, len(inv.Directories))
	var total int64
	for _, d := range inv.Directories {
		if !inventoryRelative(d) || all[d] {
			return fmt.Errorf("invalid or duplicate installed directory")
		}
		all[d] = true
		dirs[d] = true
	}
	for _, f := range inv.Files {
		decoded, err := hex.DecodeString(f.SHA256)
		if !inventoryRelative(f.Path) || all[f.Path] || f.Size < 0 || f.Size > maxInstalledBytes-total || err != nil || len(decoded) != sha256.Size || f.SHA256 != strings.ToLower(f.SHA256) {
			return fmt.Errorf("invalid installed file inventory")
		}
		all[f.Path] = true
		total += f.Size
	}
	for name := range all {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if !dirs[parent] {
				return fmt.Errorf("installed entry has an undeclared or non-directory parent")
			}
		}
	}
	raw, err := json.Marshal(inv)
	if err != nil || len(raw) > maxInstalledManifest {
		return fmt.Errorf("installed inventory exceeds journal limit")
	}
	return nil
}

func inventoryRelative(name string) bool {
	if name == "" || name == "." || len(name) > 4096 || !utf8.ValidString(name) || strings.Contains(name, "\\") || !fs.ValidPath(name) || path.Clean(name) != name {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func inventoryRootName(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || filepath.Base(filepath.Dir(root)) != "game" {
		return fmt.Errorf("installed inventory root is not a native title directory")
	}
	for _, part := range strings.Split(root, string(filepath.Separator)) {
		if part == ".dotfiles" {
			return fmt.Errorf("native title is inside protected dotfiles")
		}
	}
	title := filepath.Base(root)
	if len(title) != 9 {
		return fmt.Errorf("invalid native title directory")
	}
	for i, c := range []byte(title) {
		if i < 4 && (c < 'A' || c > 'Z') || i >= 4 && (c < '0' || c > '9') {
			return fmt.Errorf("invalid native title directory")
		}
	}
	return nil
}

// openInventoryRoot rejects redirects before opening and verifies the opened inode.
// os.Root then prevents descendant paths from escaping that pinned directory.
func openInventoryRoot(root string, allowMissing bool) (*os.Root, error) {
	if err := inventoryRootName(root); err != nil {
		return nil, err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) && allowMissing {
		parent, resolveErr := filepath.EvalSymlinks(filepath.Dir(root))
		if resolveErr != nil || parent != filepath.Dir(root) {
			return nil, fmt.Errorf("native title parent is unavailable or redirected")
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("native title is not a physical directory")
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root {
		return nil, fmt.Errorf("native title path is redirected")
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, err
	}
	after, err := opened.Stat(".")
	current, currentErr := os.Lstat(root)
	if err != nil || currentErr != nil || !os.SameFile(info, after) || !os.SameFile(info, current) || current.Mode()&fs.ModeSymlink != 0 {
		opened.Close()
		return nil, fmt.Errorf("native title changed while opening")
	}
	return opened, nil
}

func hashInstalledFile(root *os.Root, name string, expected fs.FileInfo) (installedFile, error) {
	if !expected.Mode().IsRegular() || expected.Size() < 0 || expected.Size() > maxInstalledBytes {
		return installedFile{}, fmt.Errorf("installed title contains a special or oversized file")
	}
	file, err := root.Open(name)
	if err != nil {
		return installedFile{}, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !os.SameFile(expected, before) || !before.Mode().IsRegular() || before.Size() != expected.Size() || !before.ModTime().Equal(expected.ModTime()) {
		return installedFile{}, fmt.Errorf("installed file changed while opening")
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(file, expected.Size()+1))
	after, statErr := file.Stat()
	current, pathErr := root.Lstat(name)
	if err != nil || statErr != nil || pathErr != nil || n != expected.Size() || !os.SameFile(before, after) || !os.SameFile(before, current) || !current.Mode().IsRegular() || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || current.Size() != before.Size() || !current.ModTime().Equal(before.ModTime()) {
		return installedFile{}, fmt.Errorf("installed file changed while hashing")
	}
	return installedFile{Path: name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func scanInstalledInventory(root *os.Root) (installedInventory, error) {
	inv := installedInventory{}
	count := 0
	manifestBytes := len(`{"files":[],"directories":[]}`)
	addManifestEntry := func(value any) error {
		raw, err := json.Marshal(value)
		manifestBytes += len(raw) + 1
		if err != nil || manifestBytes > maxInstalledManifest {
			return fmt.Errorf("installed inventory exceeds journal limit")
		}
		return nil
	}
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		count++
		if count > maxInstalledEntries || !inventoryRelative(name) {
			return fmt.Errorf("invalid or excessive installed title entries")
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("installed title contains a symbolic link")
		}
		if info.IsDir() {
			if err := addManifestEntry(name); err != nil {
				return err
			}
			inv.Directories = append(inv.Directories, name)
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxInstalledBytes-total {
			return fmt.Errorf("installed title contains a special file or exceeds its size limit")
		}
		value, err := hashInstalledFile(root, name, info)
		if err != nil {
			return err
		}
		if err := addManifestEntry(value); err != nil {
			return err
		}
		total += value.Size
		inv.Files = append(inv.Files, value)
		return nil
	})
	if err != nil {
		return installedInventory{}, err
	}
	slices.Sort(inv.Directories)
	slices.SortFunc(inv.Files, func(a, b installedFile) int { return strings.Compare(a.Path, b.Path) })
	return inv, nil
}

func captureInstalledInventory(root string) (installedInventory, error) {
	opened, err := openInventoryRoot(root, false)
	if err != nil {
		return installedInventory{}, err
	}
	defer opened.Close()
	inv, err := scanInstalledInventory(opened)
	if err != nil {
		return installedInventory{}, err
	}
	if err = validateInstalledInventory(inv); err != nil {
		return installedInventory{}, err
	}
	return inv, nil
}

func checkInventoryAt(root *os.Root, expected installedInventory, allowMissing bool) error {
	actual, err := scanInstalledInventory(root)
	if err != nil {
		return err
	}
	files := make(map[string]installedFile, len(expected.Files))
	dirs := make(map[string]bool, len(expected.Directories))
	for _, f := range expected.Files {
		files[f.Path] = f
	}
	for _, d := range expected.Directories {
		dirs[d] = true
	}
	for _, f := range actual.Files {
		if old, ok := files[f.Path]; !ok || old != f {
			return fmt.Errorf("installed title has an unexpected or modified file")
		}
		delete(files, f.Path)
	}
	for _, d := range actual.Directories {
		if !dirs[d] {
			return fmt.Errorf("installed title has an unexpected directory")
		}
		delete(dirs, d)
	}
	if !allowMissing && (len(files) != 0 || len(dirs) != 0) {
		return fmt.Errorf("installed title is missing recorded entries")
	}
	return nil
}

func checkInstalledInventory(root string, inv installedInventory, allowMissing bool) error {
	if err := validateInstalledInventory(inv); err != nil {
		return err
	}
	opened, err := openInventoryRoot(root, allowMissing)
	if err != nil {
		return err
	}
	if opened == nil {
		return nil
	}
	defer opened.Close()
	return checkInventoryAt(opened, inv, allowMissing)
}

// removeInstalledInventory requires the caller's durable removing journal. It
// allows recorded entries already removed by an interrupted attempt, but no
// changed/extra entries. Files are unlinked individually and directories only
// with rmdir semantics: a newly added user file is never recursively deleted.
func removeInstalledInventory(root string, inv installedInventory) error {
	return removeInstalledInventoryWith(root, inv, func(parent *os.Root, name string) error { return parent.Remove(name) })
}

func removeInstalledInventoryWith(root string, inv installedInventory, remove func(*os.Root, string) error) error {
	if err := validateInstalledInventory(inv); err != nil {
		return err
	}
	opened, err := openInventoryRoot(root, true)
	if err != nil {
		return err
	}
	if opened == nil {
		// A previous attempt may have removed the title but failed before its
		// parent was flushed. Flush game before allowing a durable receipt.
		parent, err := os.OpenRoot(filepath.Dir(root))
		if err != nil {
			return err
		}
		defer parent.Close()
		return syncInventoryDirectory(parent)
	}
	defer opened.Close()
	if err := checkInventoryAt(opened, inv, true); err != nil {
		return err
	}
	files := slices.Clone(inv.Files)
	slices.SortFunc(files, func(a, b installedFile) int { return strings.Compare(b.Path, a.Path) })
	for _, f := range files {
		if err := verifyInventoryRoot(root, opened); err != nil {
			return err
		}
		parent, base, err := inventoryEntryParent(opened, f.Path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := parent.Lstat(base)
		if errors.Is(err, fs.ErrNotExist) {
			parent.Close()
			continue
		}
		if err != nil {
			parent.Close()
			return err
		}
		value, err := hashInstalledFile(parent, base, info)
		if err != nil || value.Size != f.Size || value.SHA256 != f.SHA256 {
			parent.Close()
			return fmt.Errorf("installed file changed before removal")
		}
		err = removeInventoryEntry(parent, base, remove)
		parent.Close()
		if err != nil {
			return err
		}
	}
	dirs := slices.Clone(inv.Directories)
	slices.SortFunc(dirs, func(a, b string) int {
		if d := strings.Count(b, "/") - strings.Count(a, "/"); d != 0 {
			return d
		}
		return strings.Compare(b, a)
	})
	for _, d := range dirs {
		if err := verifyInventoryRoot(root, opened); err != nil {
			return err
		}
		parent, base, err := inventoryEntryParent(opened, d)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := parent.Lstat(base)
		if errors.Is(err, fs.ErrNotExist) {
			parent.Close()
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			parent.Close()
			return fmt.Errorf("installed directory changed before removal")
		}
		err = removeInventoryEntry(parent, base, remove)
		parent.Close()
		if err != nil {
			return err
		}
	}
	if err := verifyInventoryRoot(root, opened); err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Dir(root))
	if err != nil {
		return err
	}
	defer parent.Close()
	current, err := parent.Lstat(filepath.Base(root))
	pinned, pinnedErr := opened.Stat(".")
	if errors.Is(err, fs.ErrNotExist) {
		return syncInventoryDirectory(parent)
	}
	if err != nil || pinnedErr != nil || current.Mode()&fs.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(current, pinned) {
		return fmt.Errorf("native title changed before removing its directory")
	}
	return removeInventoryEntry(parent, filepath.Base(root), remove)
}

// Open the immediate parent separately, so a concurrent rename cannot redirect
// a leaf unlink through a newly introduced symlink elsewhere in the title.
func inventoryEntryParent(root *os.Root, name string) (*os.Root, string, error) {
	parentName := path.Dir(name)
	current := ""
	if parentName != "." {
		for _, part := range strings.Split(parentName, "/") {
			current = path.Join(current, part)
			info, err := root.Lstat(current)
			if err != nil {
				return nil, "", err
			}
			if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
				return nil, "", fmt.Errorf("native title parent was redirected")
			}
		}
	}
	before, err := root.Lstat(parentName)
	if err != nil {
		return nil, "", err
	}
	parent, err := root.OpenRoot(parentName)
	if err != nil {
		return nil, "", err
	}
	pinned, err := parent.Stat(".")
	if err != nil || !os.SameFile(before, pinned) {
		parent.Close()
		return nil, "", fmt.Errorf("native title parent changed while opening")
	}
	return parent, path.Base(name), nil
}

// Require the original physical path to still designate the pinned title before
// each mutation; a renamed/replaced title must be reviewed by the caller.
func verifyInventoryRoot(root string, pinned *os.Root) error {
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root {
		return fmt.Errorf("native title path changed before removal")
	}
	current, err := os.Lstat(root)
	original, pinnedErr := pinned.Stat(".")
	if err != nil || pinnedErr != nil || !current.IsDir() || current.Mode()&fs.ModeSymlink != 0 || !os.SameFile(current, original) {
		return fmt.Errorf("native title changed before removal")
	}
	return nil
}

// Flush every successful parent-directory mutation before the orchestration can
// mark the removal receipt durable. No caller may report removed on sync error.
func removeInventoryEntry(parent *os.Root, name string, remove func(*os.Root, string) error) error {
	if err := remove(parent, name); err != nil {
		return err
	}
	return syncInventoryDirectory(parent)
}

func syncInventoryDirectory(root *os.Root) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
