package rpcs3

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type nativeChange struct {
	Path   string         `json:"path"`
	Before *installedFile `json:"before,omitempty"`
	After  installedFile  `json:"after"`
}
type overlayReceipt struct {
	Version int            `json:"version"`
	WorkDir string         `json:"work_dir"`
	Before  *installIntent `json:"before,omitempty"`
	After   installIntent  `json:"after"`
	Changes []nativeChange `json:"changes"`
}

func (i *Installer) overlayPath(title string) string {
	return filepath.Join(i.config.IntentDir, title+".overlay.json")
}
func (i *Installer) readOverlay(title string) (overlayReceipt, bool, error) {
	var r overlayReceipt
	p := i.overlayPath(title)
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxIntentSize*3 {
		return r, true, ErrIntentNeedsReview
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return r, true, err
	}
	if json.Unmarshal(data, &r) != nil || i.validateOverlay(r, title) != nil {
		return r, true, ErrIntentNeedsReview
	}
	return r, true, nil
}
func (i *Installer) validateOverlay(r overlayReceipt, title string) error {
	if r.Version != 1 || !validTitle(title) || r.After.TitleID != title || r.After.Version != 3 || !validOwnedIntent(r.After) || !validIntentTimestamps(r.After) || r.After.ROMID <= 0 || !validSHA256(r.After.PackageSHA256) || len(r.Changes) == 0 || len(r.Changes) > maxInstalledEntries {
		return ErrIntentNeedsReview
	}
	encoded, err := json.MarshalIndent(r.After, "", "  ")
	if err != nil || len(encoded) > maxIntentSize {
		return fmt.Errorf("PS3 resulting ownership receipt exceeds its read limit")
	}
	if !filepath.IsAbs(r.WorkDir) || filepath.Dir(r.WorkDir) != i.config.IntentDir || !strings.HasPrefix(filepath.Base(r.WorkDir), ".ps3-extras-"+title+"-") {
		return ErrIntentNeedsReview
	}
	if r.Before != nil {
		b := *r.Before
		if !validOwnedIntent(b) || !validIntentTimestamps(b) || b.ROMID != r.After.ROMID || b.TitleID != title || b.DevHDD0 != r.After.DevHDD0 || b.RPCS3User != r.After.RPCS3User {
			return ErrIntentNeedsReview
		}
	}
	allowedLicenses := map[string]bool{}
	for _, l := range intentLicenses(r.After) {
		allowedLicenses[path.Join("home", r.After.RPCS3User, "exdata", l.ContentID+".rap")] = l.Owned
	}
	seen := map[string]bool{}
	for _, c := range r.Changes {
		if !inventoryRelative(c.Path) || seen[c.Path] || c.After.Path != c.Path || !validImage(c.After) {
			return ErrIntentNeedsReview
		}
		seen[c.Path] = true
		if !strings.HasPrefix(c.Path, "game/"+title+"/") && !allowedLicenses[c.Path] {
			return ErrIntentNeedsReview
		}
		if c.Before != nil && (c.Before.Path != c.Path || !validImage(*c.Before)) {
			return ErrIntentNeedsReview
		}
	}
	return nil
}
func validImage(f installedFile) bool {
	return inventoryRelative(f.Path) && f.Size >= 0 && f.Size <= maxInstalledBytes && validSHA256(f.SHA256)
}
func imageMatches(p string, f installedFile) bool {
	sum, err := sha256File(p, f.Size)
	return err == nil && sum == f.SHA256
}
func (i *Installer) overlayMatchesCurrent(r overlayReceipt) error {
	current, found, err := readIntent(i.config.IntentDir, r.After.TitleID)
	if err != nil {
		return err
	}
	if found && ownershipHash(current) == ownershipHash(r.After) {
		return nil
	}
	if r.Before == nil {
		if found {
			return ErrIntentNeedsReview
		}
	} else if !found || ownershipHash(current) != ownershipHash(*r.Before) {
		return ErrIntentNeedsReview
	}
	return nil
}
func (i *Installer) checkHybrid(r overlayReceipt) error {
	root := extraTitleRoot(r.After.DevHDD0, r.After.TitleID)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if r.Before == nil {
			return nil
		}
		return ErrIntentNeedsReview
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrIntentNeedsReview
	}
	// A crash after mkdir but before the first file may leave a fresh disc
	// overlay empty. Only directories declared by the durable plan are allowed.
	actual, err := captureInstalledInventory(root)
	if err != nil {
		if r.Before != nil {
			return err
		}
		allowed := map[string]bool{}
		for _, d := range r.After.Inventory.Directories {
			allowed[d] = true
		}
		emptyErr := filepath.WalkDir(root, func(p string, e os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if p == root {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil || !e.IsDir() || e.Type()&os.ModeSymlink != 0 || !allowed[filepath.ToSlash(rel)] {
				return ErrIntentNeedsReview
			}
			return nil
		})
		if emptyErr != nil {
			return err
		}
		return nil
	}
	beforeFiles := map[string]installedFile{}
	beforeDirs := map[string]bool{}
	if r.Before != nil {
		for _, f := range r.Before.Inventory.Files {
			beforeFiles[f.Path] = f
		}
		for _, d := range r.Before.Inventory.Directories {
			beforeDirs[d] = true
		}
	}
	afterFiles := map[string]installedFile{}
	afterDirs := map[string]bool{}
	for _, f := range r.After.Inventory.Files {
		afterFiles[f.Path] = f
	}
	for _, d := range r.After.Inventory.Directories {
		afterDirs[d] = true
	}
	found := map[string]bool{}
	for _, f := range actual.Files {
		found[f.Path] = true
		a, ok := afterFiles[f.Path]
		if !ok {
			return ErrIntentNeedsReview
		}
		if a.Size == f.Size && a.SHA256 == f.SHA256 {
			continue
		}
		b, ok := beforeFiles[f.Path]
		if !ok || b.Size != f.Size || b.SHA256 != f.SHA256 {
			return ErrIntentNeedsReview
		}
	}
	for name := range beforeFiles {
		if !found[name] {
			return ErrIntentNeedsReview
		}
	}
	actualDirs := map[string]bool{}
	for _, d := range actual.Directories {
		if !afterDirs[d] {
			return ErrIntentNeedsReview
		}
		actualDirs[d] = true
	}
	for name := range beforeDirs {
		if !actualDirs[name] {
			return ErrIntentNeedsReview
		}
	}
	return nil
}
func (i *Installer) applyOverlay(r overlayReceipt) error {
	if err := i.validateOverlay(r, r.After.TitleID); err != nil {
		return err
	}
	profile, err := i.config.profile()
	if err != nil || profile.DevHDD0 != r.After.DevHDD0 || profile.UserID != r.After.RPCS3User {
		return ErrIntentNeedsReview
	}
	if err = i.ensureRPCS3Closed(); err != nil {
		return err
	}
	if err = i.overlayMatchesCurrent(r); err != nil {
		return err
	}
	current, found, err := readIntent(i.config.IntentDir, r.After.TitleID)
	if err != nil {
		return err
	}
	if found && ownershipHash(current) == ownershipHash(r.After) {
		if err = checkInstalledInventory(extraTitleRoot(profile.DevHDD0, r.After.TitleID), *r.After.Inventory, false); err != nil {
			return err
		}
		if err = i.verifyIntentLicenses(r.After, false, false); err != nil {
			return err
		}
		return i.finishOverlay(r)
	}
	physical, err := filepath.EvalSymlinks(r.WorkDir)
	if err != nil || physical != r.WorkDir {
		return ErrIntentNeedsReview
	}
	if err = i.checkHybrid(r); err != nil {
		return err
	}
	// Verify every retained before-image and every incoming byte before the first
	// native mutation. Backups are never replayed over a foreign modification.
	for _, c := range r.Changes {
		if !imageMatches(filepath.Join(r.WorkDir, "payload", filepath.FromSlash(c.Path)), c.After) {
			return ErrIntentNeedsReview
		}
		if c.Before != nil && !imageMatches(filepath.Join(r.WorkDir, "before", filepath.FromSlash(c.Path)), *c.Before) {
			return ErrIntentNeedsReview
		}
		p, info, present, err := nativePath(profile.DevHDD0, c.Path)
		if err != nil {
			return err
		}
		if present {
			if !info.Mode().IsRegular() || (!imageMatches(p, c.After) && (c.Before == nil || !imageMatches(p, *c.Before))) {
				return ErrIntentNeedsReview
			}
		} else if c.Before != nil {
			return ErrIntentNeedsReview
		}
	}
	root, err := os.OpenRoot(profile.DevHDD0)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, c := range r.Changes {
		p, _, present, err := nativePath(profile.DevHDD0, c.Path)
		if err != nil {
			return err
		}
		if present && imageMatches(p, c.After) {
			continue
		}
		if i.overlayWrite != nil {
			if err = i.overlayWrite(filepath.Join(r.WorkDir, "payload", filepath.FromSlash(c.Path)), p); err != nil {
				return err
			}
		}
		if err = copyIntoRoot(root, c.Path, filepath.Join(r.WorkDir, "payload", filepath.FromSlash(c.Path)), c.After, overlayTempPath(r, c.Path)); err != nil {
			return err
		}
	}
	if err = checkInstalledInventory(extraTitleRoot(profile.DevHDD0, r.After.TitleID), *r.After.Inventory, false); err != nil {
		return err
	}
	if err = i.verifyIntentLicenses(r.After, false, false); err != nil {
		return err
	}
	if err = i.overlayMatchesCurrent(r); err != nil {
		return err
	}
	if err = savePrivateJSON(intentFile(i.config.IntentDir, r.After.TitleID), r.After, false); err != nil {
		return err
	}
	return i.finishOverlay(r)
}
func (i *Installer) finishOverlay(r overlayReceipt) error {
	if err := cleanOverlayTemps(r); err != nil {
		return err
	}
	// Native ownership is durable before any backup or operation receipt leaves.
	// Removing only this private work tree cannot reach game data or saves.
	if err := os.Remove(i.overlayPath(r.After.TitleID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncDirectory(i.config.IntentDir); err != nil {
		return err
	}
	if filepath.Dir(r.WorkDir) != i.config.IntentDir || !strings.HasPrefix(filepath.Base(r.WorkDir), ".ps3-extras-"+r.After.TitleID+"-") {
		return ErrIntentNeedsReview
	}
	if info, err := os.Lstat(r.WorkDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrIntentNeedsReview
		}
		return os.RemoveAll(r.WorkDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func nativePath(nand, relative string) (string, os.FileInfo, bool, error) {
	if !inventoryRelative(relative) {
		return "", nil, false, ErrIntentNeedsReview
	}
	return pathUnderRoot(nand, strings.Split(relative, "/")...)
}
func copyIntoRoot(root *os.Root, relative, source string, image installedFile, tmp string) error {
	if !inventoryRelative(relative) || !imageMatches(source, image) {
		return ErrIntentNeedsReview
	}
	parent := path.Dir(relative)
	parts := strings.Split(parent, "/")
	current := ""
	for _, part := range parts {
		if current == "" {
			current = part
		} else {
			current += "/" + part
		}
		info, err := root.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err = root.Mkdir(current, 0755); err != nil {
				return err
			}
			if err = syncNativeDir(root, path.Dir(current)); err != nil {
				return err
			}
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrIntentNeedsReview
		}
	}
	if !inventoryRelative(tmp) || !strings.HasPrefix(tmp, ".romm-ps3-overlays/") {
		return ErrIntentNeedsReview
	}
	tempParent := path.Dir(tmp)
	for _, d := range []string{".romm-ps3-overlays", tempParent} {
		info, err := root.Lstat(d)
		if errors.Is(err, os.ErrNotExist) {
			if err = root.Mkdir(d, 0700); err != nil {
				return err
			}
			if err = syncNativeDir(root, path.Dir(d)); err != nil {
				return err
			}
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return ErrIntentNeedsReview
		}
	}
	if info, err := root.Lstat(tmp); err == nil {
		if !info.Mode().IsRegular() || info.Size() > image.Size {
			return ErrIntentNeedsReview
		}
		if err = root.Remove(tmp); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dst, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	defer root.Remove(tmp)
	src, err := os.Open(source)
	if err != nil {
		dst.Close()
		return err
	}
	n, copyErr := io.Copy(dst, io.LimitReader(src, image.Size+1))
	src.Close()
	if copyErr == nil && n != image.Size {
		copyErr = ErrIntentNeedsReview
	}
	if copyErr == nil {
		copyErr = dst.Sync()
	}
	closeErr := dst.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	if !imageMatches(source, image) {
		return ErrIntentNeedsReview
	}
	if err = root.Rename(tmp, relative); err != nil {
		return err
	}
	for _, directory := range []string{parent, tempParent} {
		dir, err := root.Open(directory)
		if err != nil {
			return err
		}
		err = dir.Sync()
		dir.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func buildOverlayInventory(before *installIntent, changes []nativeChange, title string) (installedInventory, error) {
	files := map[string]installedFile{}
	dirs := map[string]bool{}
	if before != nil {
		for _, f := range before.Inventory.Files {
			files[f.Path] = f
		}
		for _, d := range before.Inventory.Directories {
			dirs[d] = true
		}
	}
	prefix := "game/" + title + "/"
	for _, c := range changes {
		if !strings.HasPrefix(c.Path, prefix) {
			continue
		}
		name := strings.TrimPrefix(c.Path, prefix)
		if dirs[name] {
			return installedInventory{}, fmt.Errorf("PS3 attachment replaces a directory with a file")
		}
		for _, d := range parentDirectories(name) {
			if _, found := files[d]; found {
				return installedInventory{}, fmt.Errorf("PS3 attachment replaces a file with a directory")
			}
			dirs[d] = true
		}
		f := c.After
		f.Path = name
		files[name] = f
	}
	inv := installedInventory{}
	for _, f := range files {
		inv.Files = append(inv.Files, f)
	}
	for d := range dirs {
		inv.Directories = append(inv.Directories, d)
	}
	sort.Slice(inv.Files, func(a, b int) bool { return inv.Files[a].Path < inv.Files[b].Path })
	sort.Strings(inv.Directories)
	return inv, validateInstalledInventory(inv)
}

// Temp destinations are derived from a previously persisted operation receipt.
// They live outside game/TITLEID so an interrupted write cannot pollute the
// installed inventory. Only these exact private operation files are removable.
func overlayTempPath(r overlayReceipt, relative string) string {
	h := sha256.Sum256([]byte(relative))
	return path.Join(".romm-ps3-overlays", filepath.Base(r.WorkDir), hex.EncodeToString(h[:])+".tmp")
}
func cleanOverlayTemps(r overlayReceipt) error {
	root, err := os.OpenRoot(r.After.DevHDD0)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, d := range []string{".romm-ps3-overlays", path.Join(".romm-ps3-overlays", filepath.Base(r.WorkDir))} {
		info, err := root.Lstat(d)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return ErrIntentNeedsReview
		}
	}
	for _, c := range r.Changes {
		p := overlayTempPath(r, c.Path)
		info, err := root.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > c.After.Size {
			return ErrIntentNeedsReview
		}
		if err = root.Remove(p); err != nil {
			return err
		}
	}
	dir := path.Join(".romm-ps3-overlays", filepath.Base(r.WorkDir))
	if err = root.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func syncNativeDir(root *os.Root, name string) error {
	directory, err := root.Open(name)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
