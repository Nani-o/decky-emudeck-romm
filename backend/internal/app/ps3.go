package app

import (
	"context"
	"crypto/sha1" //nolint:gosec // native ownership uses SHA-1 for downloaded files
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

// PS3PackageInstaller installs and independently verifies one supported native
// base-game package. It never exposes NAND paths as owned ROM files.
type PS3PackageInstaller interface {
	Install(ctx context.Context, romID int, stagingDir string) (domain.PS3InstallResult, error)
}

// PS3PackageRemover owns native title removal separately from ROM files.
type PS3PackageRemover interface {
	CheckRemoval(context.Context, int) (bool, error)
	Remove(context.Context, int) (bool, error)
}

func nativePS3Shortcut(rec domain.OwnedROM) bool {
	for _, f := range rec.Files {
		if strings.EqualFold(filepath.Ext(f.Path), ".desktop") {
			for dir := filepath.Dir(f.Path); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
				if filepath.Base(dir) == "ps3" {
					return true
				}
			}
		}
	}
	return false
}

// InstallPS3PackagesWith enables the native package path without changing disc
// directory installation or save synchronization.
func (d *Downloads) InstallPS3PackagesWith(installer PS3PackageInstaller) { d.ps3 = installer }

// stagePS3Package leaves ordinary disc files unchanged. PKG bundles instead
// produce one owned shortcut, only after the native installer verified its
// title and license. Inputs never appear as playable entries in roms/.
func (d *Downloads) stagePS3Package(ctx context.Context, j *job, rom domain.ROM, staged []stagedFile, staging, dir string, old []domain.PlacedFile) ([]stagedFile, string, error) {
	if isPS3Disc(staged) {
		return staged, "", nil
	}
	pkg := false
	rap := false
	for _, f := range staged {
		pkg = pkg || strings.EqualFold(filepath.Ext(f.From), ".pkg")
		rap = rap || strings.EqualFold(filepath.Ext(f.From), ".rap")
	}
	if !pkg && !rap {
		return staged, "", nil
	}
	if d.ps3 == nil {
		return nil, "", &domain.Error{Kind: domain.KindCantDownload, Message: "Native RPCS3 package installation is unavailable."}
	}
	if j.update {
		return nil, "", &domain.Error{Kind: domain.KindCantDownload, Message: "This installed PS3 PKG must be updated through RPCS3; native package updates and DLC are not yet supported."}
	}
	name := rom.FileName
	if name == "" {
		name = rom.Name
	}
	name = filepath.Base(name)
	if domain.IsArchive(name) || strings.EqualFold(filepath.Ext(name), ".pkg") {
		name = strings.TrimSuffix(name, filepath.Ext(name))
	}
	name += ".desktop"
	if !filepath.IsLocal(name) || filepath.Base(name) != name || !domain.SafeESDELaunchPath(name) {
		return nil, "", &domain.Error{Kind: domain.KindCantDownload, Message: "This PS3 shortcut name cannot be safely launched by ES-DE."}
	}
	if err := checkTarget(filepath.Join(dir, name), old); err != nil {
		return nil, "", err
	}
	inputs := make([]stagedFile, 0, 2)
	for _, f := range staged {
		switch strings.ToLower(filepath.Ext(f.From)) {
		case ".pkg", ".rap":
			inputs = append(inputs, f)
		case ".txt", ".md", ".nfo", ".url", ".pdf": // documentation is not an installer input
		default:
			if strings.EqualFold(filepath.Base(f.From), "README") || strings.EqualFold(filepath.Base(f.From), "LICENSE") {
				continue
			}
			return nil, "", &domain.Error{Kind: domain.KindCantDownload, Message: "The PS3 download mixes a PKG with unsupported game files."}
		}
	}
	if len(inputs) != 2 {
		return nil, "", &domain.Error{Kind: domain.KindCantDownload, Message: "Native PS3 installation needs exactly one base-game PKG and its RAP."}
	}
	private := filepath.Join(staging, ".ps3-package")
	if err := privatePS3Inputs(private, inputs); err != nil {
		return nil, "", err
	}
	d.setPhase(j, "install")
	// A cancelled native install can have changed NAND. Its source and journal
	// must survive rather than looking like a cancelled ordinary transfer.
	j.retainStaging = true
	result, err := d.ps3.Install(ctx, j.ROMID, private)
	if err != nil {
		return nil, "", fmt.Errorf("RPCS3 package installation failed; private staging was retained for review: %w", err)
	}
	identity, err := domain.ParsePS3ContentID(result.Identity.ContentID)
	if err != nil || identity != result.Identity || result.ROMID != j.ROMID || result.VerifiedAt.IsZero() || result.DesktopEntry == "" || len(result.RPCS3User) != 8 || strings.Trim(result.RPCS3User, "0123456789") != "" {
		return nil, "", &domain.Error{Kind: domain.KindCantDownload, Message: "RPCS3 did not return a verified title and launcher; staging was retained."}
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	path := filepath.Join(staging, madeDir, name)
	if err := writePS3Launcher(path, []byte(result.DesktopEntry)); err != nil {
		return nil, "", err
	}
	sum := sha1.Sum([]byte(result.DesktopEntry))
	sourceID := 0
	for _, f := range inputs {
		if strings.EqualFold(filepath.Ext(f.From), ".pkg") {
			sourceID = f.SourceFileID
		}
	}
	return []stagedFile{{From: path, Rel: name, SHA1: hex.EncodeToString(sum[:]), SourceFileID: sourceID}}, name, nil
}

func privatePS3Inputs(directory string, inputs []stagedFile) error {
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe PS3 package staging directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	expected := map[string]bool{}
	for _, f := range inputs {
		name := filepath.Base(f.From)
		if expected[name] {
			return fmt.Errorf("duplicate PS3 package input")
		}
		expected[name] = true
	}
	for _, e := range entries {
		if !expected[e.Name()] {
			return fmt.Errorf("unexpected earlier PS3 staging input; review is required")
		}
	}
	for _, f := range inputs {
		target := filepath.Join(directory, filepath.Base(f.From))
		info, err := os.Lstat(target)
		if err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("unsafe earlier PS3 staging input")
			}
			sum, err := fileSHA1(target)
			if err != nil {
				return err
			}
			if sum != f.SHA1 {
				return fmt.Errorf("earlier PS3 staging differs; review is required")
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := moveFile(f.From, target); err != nil {
			return err
		}
		if err := os.Chmod(target, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// An interrupted placement can reuse exactly the generated launcher. Never
// overwrite a redirected or modified file even inside private staging.
func writePS3Launcher(path string, data []byte) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe PS3 launcher staging directory")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsafe earlier PS3 launcher")
		}
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(old) != string(data) {
			return fmt.Errorf("earlier PS3 launcher differs; review is required")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := out.Write(data)
	if writeErr == nil {
		writeErr = out.Sync()
	}
	closeErr := out.Close()
	return errors.Join(writeErr, closeErr)
}

func isPS3Disc(files []stagedFile) bool {
	paths := map[string]bool{}
	for _, f := range files {
		paths[strings.ToUpper(filepath.ToSlash(f.Rel))] = true
	}
	const boot = "PS3_GAME/USRDIR/EBOOT.BIN"
	for path := range paths {
		if !strings.HasSuffix(path, boot) {
			continue
		}
		root := strings.TrimSuffix(path, boot)
		if root != "" && !strings.HasSuffix(root, "/") {
			continue
		}
		if paths[root+"PS3_GAME/PARAM.SFO"] {
			return true
		}
	}
	return false
}
