package rpcs3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

// InstallExtras stages native package extraction away from the active NAND.
// Cancellation is honored up to the durable commit; the bounded final placement
// then completes or leaves a resumable receipt with the original bytes intact.
func (i *Installer) InstallExtras(ctx context.Context, req domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var result domain.PS3ExtrasResult
	if i.initErr != nil {
		return result, i.initErr
	}
	if ctx == nil || req.ROMID <= 0 || !validTitle(req.TitleID) || len(req.Packages) == 0 || len(req.Packages) > 128 {
		return result, fmt.Errorf("invalid PS3 attachments request")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := i.validateExecutable(i.config.AppImagePath); err != nil {
		return result, err
	}
	profile, err := i.config.profile()
	if err != nil {
		return result, err
	}
	physical, err := filepath.EvalSymlinks(profile.DevHDD0)
	if err != nil || physical != profile.DevHDD0 {
		return result, ErrIntentNeedsReview
	}
	if err = i.ensureRPCS3Closed(); err != nil {
		return result, err
	}
	if err = ensurePrivateDirectory(i.config.IntentDir); err != nil {
		return result, err
	}
	bundles := make([]ExtraBundle, 0, len(req.Packages))
	ids := map[int]bool{}
	for _, input := range req.Packages {
		if input.SourceFileID <= 0 || ids[input.SourceFileID] {
			return result, fmt.Errorf("PS3 attachment source IDs must be distinct")
		}
		ids[input.SourceFileID] = true
		b, err := InspectExtraBundle(input.Directory, input.Kind, req.TitleID)
		if err != nil {
			return result, err
		}
		b.SourceFileID = input.SourceFileID
		bundles = append(bundles, b)
	}
	if err = sortExtraBundles(bundles); err != nil {
		return result, err
	}
	pending, found, err := i.readOverlay(req.TitleID)
	if err != nil {
		return result, err
	}
	if found {
		if pending.After.ROMID != req.ROMID || pending.After.DevHDD0 != profile.DevHDD0 || pending.After.RPCS3User != profile.UserID || !sameExtraManifest(pending.After.Extras, bundles) || (pending.After.BaseKind == "disc") != req.DiscBase {
			return result, ErrIntentNeedsReview
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if err = i.applyOverlay(pending); err != nil {
			return result, fmt.Errorf("PS3 installation is incomplete; private source and before-images retained for a safe retry: %w", err)
		}
		return extrasResult(req.TitleID, bundles, i.now()), nil
	}
	if err = i.finishExtraRestart(req, profile); err != nil {
		return result, err
	}
	before, oldSFO, err := i.extrasBase(req, profile)
	if err != nil {
		return result, err
	}
	known := map[string]bool{}
	if before != nil {
		for _, x := range before.Extras {
			known[x.PackageSHA256] = true
		}
		if err = checkOldExtraManifest(before.Extras, bundles); err != nil {
			return result, err
		}
	}
	after := installIntent{}
	if before != nil {
		after = *before
		after.ExtraLicenses = append([]extraLicense(nil), before.ExtraLicenses...)
	} else {
		after = installIntent{ROMID: req.ROMID, TitleID: req.TitleID, ContentID: bundles[0].Identity.ContentID, PackageSHA256: bundles[0].PackageSHA256, RPCS3User: profile.UserID, DevHDD0: profile.DevHDD0, StartedAt: i.now().UTC(), BaseKind: "disc"}
	}
	after.Version = 3
	if after.BaseKind == "" {
		after.BaseKind = "psn"
	}
	after.Phase = intentVerified
	after.Reason = ""
	after.VerifiedAt = i.now().UTC()
	after.Extras = extraReceipts(bundles)
	currentVersion := ""
	if len(oldSFO) > 0 {
		tmpFields, err := inspectSFOData(oldSFO)
		if err != nil {
			return result, err
		}
		currentVersion = tmpFields["APP_VER"]
	}
	hasNewPackage := false
	for _, b := range bundles {
		if !known[b.PackageSHA256] {
			hasNewPackage = true
		}
	}
	type dlcFileOwner struct{ contentID, sha256 string }
	dlcFiles := map[string]dlcFileOwner{}
	var work string
	defer func() {
		if work == "" {
			return
		}
		// Once an overlay receipt exists its before-images are needed for replay.
		if _, err := os.Lstat(i.overlayPath(req.TitleID)); err == nil {
			return
		}
		if filepath.Dir(work) == i.config.IntentDir && strings.HasPrefix(filepath.Base(work), ".ps3-extras-"+req.TitleID+"-") {
			if info, err := os.Lstat(work); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				_ = os.RemoveAll(work)
			}
		}
	}()
	var changes []nativeChange
	payloads := map[string]string{}
	licenseSet := map[string]extraLicense{}
	for _, l := range intentLicenses(after) {
		licenseSet[l.ContentID] = l
	}
	for _, b := range bundles {
		if known[b.PackageSHA256] && !(hasNewPackage && b.Kind == domain.PS3PackageDLC) {
			continue
		}
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if b.Kind == domain.PS3PackageUpdate && currentVersion != "" {
			old, err := versionNumber(currentVersion)
			new, err2 := versionNumber(b.AppVersion)
			if err != nil || err2 != nil || new <= old {
				return result, fmt.Errorf("PS3 update %s would repeat or downgrade installed version %s without matching ownership", b.AppVersion, currentVersion)
			}
		}
		if work == "" {
			work, err = os.MkdirTemp(i.config.IntentDir, ".ps3-extras-"+req.TitleID+"-")
			if err != nil {
				return result, err
			}
			if err = syncDirectory(i.config.IntentDir); err != nil {
				return result, err
			}
		}
		stage, err := i.stageExtra(ctx, work, profile, b, oldSFO)
		if err != nil {
			return result, fmt.Errorf("native PS3 %s staging failed (source=%d, APP_VER=%s); downloaded package sources retained for retry: %w", b.Kind, b.SourceFileID, b.AppVersion, err)
		}
		for _, f := range stage.inventory.Files {
			if b.Kind == domain.PS3PackageDLC {
				if owner, found := dlcFiles[f.Path]; found && owner.contentID != b.Identity.ContentID && owner.sha256 != f.SHA256 {
					return result, fmt.Errorf("different PS3 DLCs provide conflicting bytes at %s; installation order is ambiguous", f.Path)
				}
				dlcFiles[f.Path] = dlcFileOwner{contentID: b.Identity.ContentID, sha256: f.SHA256}
			}
			relative := path.Join("game", req.TitleID, f.Path)
			payloads[relative] = filepath.Join(stage.root, filepath.FromSlash(f.Path))
		}
		if b.Kind == domain.PS3PackageUpdate {
			oldSFO, err = os.ReadFile(filepath.Join(stage.root, "PARAM.SFO"))
			if err != nil {
				return result, err
			}
			currentVersion = b.AppVersion
		}
		if b.RAPPath != "" {
			if old, exists := licenseSet[b.Identity.ContentID]; exists {
				if old.SHA256 != b.RAPSHA256 {
					return result, fmt.Errorf("PS3 attachment license differs from the recorded license")
				}
			} else {
				p, info, exists, err := pathUnderRoot(profile.DevHDD0, "home", profile.UserID, "exdata", b.Identity.ContentID+".rap")
				if err != nil {
					return result, err
				}
				owned := !exists
				if exists && (!info.Mode().IsRegular() || info.Size() != 16 || !imageMatches(p, installedFile{Size: 16, SHA256: b.RAPSHA256})) {
					return result, fmt.Errorf("existing PS3 license differs from the supplied attachment")
				}
				l := extraLicense{ContentID: b.Identity.ContentID, SHA256: b.RAPSHA256, Owned: owned}
				licenseSet[l.ContentID] = l
				after.ExtraLicenses = append(after.ExtraLicenses, l)
				if owned {
					payloads[path.Join("home", profile.UserID, "exdata", b.Identity.ContentID+".rap")] = b.RAPPath
				}
			}
		}
		known[b.PackageSHA256] = true
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if before != nil {
		if err = checkInstalledInventory(extraTitleRoot(profile.DevHDD0, req.TitleID), *before.Inventory, false); err != nil {
			return result, err
		}
		if err = i.verifyIntentLicenses(*before, false, false); err != nil {
			return result, err
		}
	}
	if len(payloads) == 0 {
		if before == nil {
			return result, ErrIntentNeedsReview
		}
		after.Inventory = before.Inventory
		after.EntryCount = before.EntryCount
		if !validOwnedIntent(after) {
			return result, ErrIntentNeedsReview
		}
		if ownershipHash(*before) != ownershipHash(after) {
			if err = savePrivateJSON(intentFile(i.config.IntentDir, req.TitleID), after, false); err != nil {
				return result, err
			}
		}
		if work != "" {
			_ = os.RemoveAll(work)
		}
		return extrasResult(req.TitleID, bundles, i.now()), nil
	}
	keys := make([]string, 0, len(payloads))
	for k := range payloads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, relative := range keys {
		source := payloads[relative]
		info, err := validRegular(source, maxInstalledBytes)
		if err != nil {
			return result, err
		}
		sha, err := sha256File(source, info.Size())
		if err != nil {
			return result, err
		}
		image := installedFile{Path: relative, Size: info.Size(), SHA256: sha}
		c := nativeChange{Path: relative, After: image}
		target, old, exists, err := nativePath(profile.DevHDD0, relative)
		if err != nil {
			return result, err
		}
		if exists {
			if !old.Mode().IsRegular() {
				return result, ErrIntentNeedsReview
			}
			oldsha, err := sha256File(target, old.Size())
			if err != nil {
				return result, err
			}
			previous := installedFile{Path: relative, Size: old.Size(), SHA256: oldsha}
			c.Before = &previous
			if oldsha == sha && old.Size() == info.Size() {
				continue
			}
			if err = copyPrivate(filepath.Join(work, "before", filepath.FromSlash(relative)), target, previous, work); err != nil {
				return result, err
			}
		}
		if err = copyPrivate(filepath.Join(work, "payload", filepath.FromSlash(relative)), source, image, work); err != nil {
			return result, err
		}
		changes = append(changes, c)
	}
	inventory, err := buildOverlayInventory(before, changes, req.TitleID)
	if err != nil {
		return result, err
	}
	after.Inventory = &inventory
	after.EntryCount = uint32(len(inventory.Files) + len(inventory.Directories))
	if !validOwnedIntent(after) {
		return result, ErrIntentNeedsReview
	}
	if len(changes) == 0 {
		if err = savePrivateJSON(intentFile(i.config.IntentDir, req.TitleID), after, false); err != nil {
			return result, err
		}
		if work != "" {
			_ = os.RemoveAll(work)
		}
		return extrasResult(req.TitleID, bundles, i.now()), nil
	}
	receipt := overlayReceipt{Version: 1, WorkDir: work, Before: before, After: after, Changes: changes}
	if err = i.validateOverlay(receipt, req.TitleID); err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if err = savePrivateJSON(i.overlayPath(req.TitleID), receipt, true); err != nil {
		return result, err
	}
	// The commit is deliberately not cancelable: a cancellation after this point
	// must not expose a half-installed title as a successful cancellation.
	if err = i.applyOverlay(receipt); err != nil {
		return result, fmt.Errorf("PS3 content commit is incomplete; retry the same download to resume safely (original bytes retained at %s): %w", work, err)
	}
	return extrasResult(req.TitleID, bundles, i.now()), nil
}

func (i *Installer) extrasBase(req domain.PS3ExtrasRequest, profile rpcs3Profile) (*installIntent, []byte, error) {
	in, found, err := readIntent(i.config.IntentDir, req.TitleID)
	if err != nil {
		return nil, nil, err
	}
	if found {
		if in.ROMID != req.ROMID || (in.Version != 2 && in.Version != 3) || in.Phase != intentVerified || in.Inventory == nil || in.DevHDD0 != profile.DevHDD0 || in.RPCS3User != profile.UserID || (in.BaseKind == "disc") != req.DiscBase {
			return nil, nil, ErrIntentNeedsReview
		}
		if receipt, removed, err := i.readRemoval(in); err != nil {
			return nil, nil, err
		} else if removed {
			if receipt.Phase != "removed" || !req.DiscBase {
				return nil, nil, ErrIntentNeedsReview
			}
			if _, err = i.restartAfterRemoved(in, syntheticBundle(in), profile); err != nil {
				return nil, nil, err
			}
			raw, err := verifiedBaseSFO(req.BaseSFOPath, req.TitleID)
			return nil, raw, err
		}
		if err = checkInstalledInventory(extraTitleRoot(profile.DevHDD0, req.TitleID), *in.Inventory, false); err != nil {
			return nil, nil, err
		}
		if err = i.verifyIntentLicenses(in, false, false); err != nil {
			return nil, nil, err
		}
		sfo := filepath.Join(extraTitleRoot(profile.DevHDD0, req.TitleID), "PARAM.SFO")
		if req.DiscBase {
			if _, err = os.Lstat(sfo); errors.Is(err, os.ErrNotExist) {
				sfo = req.BaseSFOPath
			}
		}
		raw, err := verifiedBaseSFO(sfo, req.TitleID)
		return &in, raw, err
	}
	if !req.DiscBase {
		return nil, nil, fmt.Errorf("PS3 base package is not owned by this plugin; import it first")
	}
	_, _, exists, err := pathUnderRoot(profile.DevHDD0, "game", req.TitleID)
	if err != nil {
		return nil, nil, err
	}
	if exists {
		return nil, nil, fmt.Errorf("PS3 native title data already exists without plugin ownership; refusing to overwrite it")
	}
	raw, err := verifiedBaseSFO(req.BaseSFOPath, req.TitleID)
	return nil, raw, err
}
func verifiedBaseSFO(p, title string) ([]byte, error) {
	if !filepath.IsAbs(p) || filepath.Base(p) != "PARAM.SFO" {
		return nil, fmt.Errorf("independent base PARAM.SFO is required")
	}
	fields, err := inspectSFOStrings(p)
	if err != nil || fields["TITLE_ID"] != title {
		return nil, ErrIntentNeedsReview
	}
	raw, err := os.ReadFile(p)
	return raw, err
}
func inspectSFOData(data []byte) (map[string]string, error) {
	// The shared parser stays file based to retain its regular-file/size checks.
	f, err := os.CreateTemp("", "romm-ps3-sfo-")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return inspectSFOStrings(name)
}
func extraReceipts(bs []ExtraBundle) []extraReceipt {
	out := make([]extraReceipt, 0, len(bs))
	for _, b := range bs {
		out = append(out, extraReceipt{SourceFileID: b.SourceFileID, Kind: b.Kind, ContentID: b.Identity.ContentID, PackageSHA256: b.PackageSHA256, RAPSHA256: b.RAPSHA256, AppVersion: b.AppVersion})
	}
	return out
}
func sameExtraManifest(records []extraReceipt, bs []ExtraBundle) bool {
	if len(records) != len(bs) {
		return false
	}
	want := extraReceipts(bs)
	for j := range records {
		if records[j] != want[j] {
			return false
		}
	}
	return true
}
func checkOldExtraManifest(old []extraReceipt, bs []ExtraBundle) error {
	byID := map[int]extraReceipt{}
	for _, r := range extraReceipts(bs) {
		byID[r.SourceFileID] = r
	}
	for _, r := range old {
		if got, ok := byID[r.SourceFileID]; !ok || got != r {
			return fmt.Errorf("an installed PS3 attachment disappeared or changed; uninstall and reinstall the game instead of silently retaining old content")
		}
	}
	return nil
}
func extrasResult(title string, bs []ExtraBundle, now time.Time) domain.PS3ExtrasResult {
	out := domain.PS3ExtrasResult{TitleID: title, VerifiedAt: now.UTC()}
	for _, b := range bs {
		out.AppliedSourceFileIDs = append(out.AppliedSourceFileIDs, b.SourceFileID)
	}
	return out
}
func copyPrivate(destination, source string, image installedFile, work string) error {
	if !imageMatches(source, image) {
		return ErrIntentNeedsReview
	}
	if err := durablePrivateParents(work, filepath.Dir(destination)); err != nil {
		return err
	}
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	src, err := os.Open(source)
	if err != nil {
		file.Close()
		return err
	}
	n, err := io.Copy(file, io.LimitReader(src, image.Size+1))
	src.Close()
	if err == nil && n != image.Size {
		err = ErrIntentNeedsReview
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = syncDirectory(filepath.Dir(destination))
	}
	return err
}

func durablePrivateParents(work, dir string) error {
	relative, err := filepath.Rel(work, dir)
	if err != nil || !filepath.IsLocal(relative) {
		return ErrIntentNeedsReview
	}
	if err = requireRealDirectory(work); err != nil {
		return err
	}
	if err = syncDirectory(filepath.Dir(work)); err != nil {
		return err
	}
	current := work
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			current = filepath.Join(current, component)
			info, err := os.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				if err = os.Mkdir(current, 0700); err != nil {
					return err
				}
				if err = syncDirectory(filepath.Dir(current)); err != nil {
					return err
				}
			} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return ErrIntentNeedsReview
			}
		}
	}
	return nil
}

type stagedExtra struct {
	root      string
	inventory installedInventory
}

func (i *Installer) stageExtra(ctx context.Context, work string, profile rpcs3Profile, b ExtraBundle, baseSFO []byte) (out stagedExtra, stageErr error) {
	var logPath string
	defer func() {
		if stageErr != nil {
			i.recordStageDiagnostic(b, logPath, stageErr)
		}
	}()
	stage, err := os.MkdirTemp(work, "native-")
	if err != nil {
		return out, err
	}
	configDir := filepath.Join(stage, "config", "rpcs3")
	cache := filepath.Join(stage, "cache")
	nand := filepath.Join(stage, "nand")
	for _, p := range []string{filepath.Join(configDir, "GuiConfigs"), filepath.Join(cache, "rpcs3"), filepath.Join(nand, "home", profile.UserID, "exdata"), filepath.Join(nand, "game")} {
		if err = os.MkdirAll(p, 0700); err != nil {
			return out, err
		}
	}
	vfs := "$(EmulatorDir): " + strconv.Quote(configDir+string(filepath.Separator)) + "\n/dev_hdd0/: " + strconv.Quote(nand+string(filepath.Separator)) + "\n/dev_flash/: " + strconv.Quote(filepath.Join(stage, "flash")+string(filepath.Separator)) + "\n/dev_hdd1/: " + strconv.Quote(filepath.Join(stage, "hdd1")+string(filepath.Separator)) + "\n"
	if err = os.WriteFile(filepath.Join(configDir, "vfs.yml"), []byte(vfs), 0600); err != nil {
		return out, err
	}
	if err = os.WriteFile(filepath.Join(configDir, "GuiConfigs", "persistent_settings.dat"), []byte("[Users]\nactive_user="+profile.UserID+"\n"), 0600); err != nil {
		return out, err
	}
	if b.Kind == domain.PS3PackageUpdate && len(baseSFO) > 0 {
		if err = os.MkdirAll(extraTitleRoot(nand, b.Identity.TitleID), 0700); err != nil {
			return out, err
		}
		if err = os.WriteFile(filepath.Join(extraTitleRoot(nand, b.Identity.TitleID), "PARAM.SFO"), baseSFO, 0600); err != nil {
			return out, err
		}
	}
	runner := Runner(commandRunner{home: i.config.HomeDir, xdgConfigHome: filepath.Dir(configDir), xdgCacheHome: cache, xdgDataHome: filepath.Join(stage, "data"), xdgStateHome: filepath.Join(stage, "state")})
	if i.extraRunner != nil {
		runner = i.extraRunner(configDir)
	}
	logPath = filepath.Join(cache, "rpcs3", "RPCS3.log")
	before, err := snapshotNativeLog(logPath)
	if err != nil {
		return out, err
	}
	started := time.Now()
	runCtx, cancel := context.WithTimeout(ctx, i.config.Timeout)
	runErr := runner.Run(runCtx, i.config.AppImagePath, "--headless", "--installpkg", b.PackagePath)
	contextErr := runCtx.Err()
	cancel()
	var teardown *headlessTeardownError
	known := errors.As(runErr, &teardown) && ctx.Err() == nil && contextErr == nil
	if ctx.Err() != nil || contextErr != nil || (runErr != nil && !known) {
		return out, errors.Join(runErr, ctx.Err(), contextErr)
	}
	current, err := InspectExtraBundle(b.Directory, b.Kind, b.Identity.TitleID)
	if err != nil || !sameBundle(b.Bundle, current.Bundle) || current.AppVersion != b.AppVersion || current.InstallDirectory != b.InstallDirectory {
		return out, fmt.Errorf("attachment source metadata changed after native extraction: %w", errors.Join(ErrInstallUnverified, err))
	}
	// A native completion record is required even for exit zero. It proves that
	// all native entries were processed before any overlay can reach the real NAND.
	if err = freshNativeInstallSuccess(logPath, before, started, b.Bundle); err != nil {
		// RPCS3 takes the logged title_id from PARAM.SFO, not the PKG header.
		// Legitimate data-only DLC without SFO logs an empty title_id. Exact fresh
		// package path, validated header, isolated destination and full inventory
		// still bind that record to this one expected title.
		_, sfoErr := os.Lstat(filepath.Join(extraTitleRoot(nand, b.Identity.TitleID), "PARAM.SFO"))
		if b.Kind != domain.PS3PackageDLC || !errors.Is(sfoErr, os.ErrNotExist) {
			return out, err
		}
		logBundle := b.Bundle
		logBundle.Identity.TitleID = ""
		if err = freshNativeInstallSuccess(logPath, before, started, logBundle); err != nil {
			return out, err
		}
	}
	installed := extraInstallRoot(nand, b)
	if err = verifyPackageEntryCount(installed, b.EntryCount); err != nil {
		return out, err
	}
	if err = verifyPrivateNativeNamespace(nand, b.Identity.TitleID); err != nil {
		return out, err
	}
	out.root = extraTitleRoot(nand, b.Identity.TitleID)
	out.inventory, err = captureInstalledInventory(out.root)
	if err != nil {
		return out, err
	}
	if b.Kind == domain.PS3PackageUpdate {
		fields, err := inspectSFOStrings(filepath.Join(out.root, "PARAM.SFO"))
		if err != nil || fields["TITLE_ID"] != b.Identity.TitleID || fields["CATEGORY"] != "GD" {
			return out, fmt.Errorf("native update PARAM.SFO rejected: TITLE_ID=%q CATEGORY=%q APP_VER=%q: %w", fields["TITLE_ID"], fields["CATEGORY"], fields["APP_VER"], errors.Join(ErrInstallUnverified, err))
		}
		v, e := versionNumber(fields["APP_VER"])
		expected, _ := versionNumber(b.AppVersion)
		if e != nil || v != expected {
			return out, fmt.Errorf("native PS3 update PARAM.SFO version differs from its package metadata")
		}
	} else {
		// A DLC is data, not a replacement executable or root game identity.
		for _, f := range out.inventory.Files {
			if f.Path == "PARAM.SFO" || strings.EqualFold(f.Path, "USRDIR/EBOOT.BIN") {
				return out, fmt.Errorf("PS3 DLC tries to replace the base game's identity or executable")
			}
		}
	}
	return out, nil
}

// RPCS3 Emu::Init creates these two empty initialization directories even for
// --installpkg. Only their exact empty shapes are permitted in a fresh private
// NAND; they are never copied into the real title or treated as game content.
func verifyPrivateNativeNamespace(nand, title string) error {
	game := filepath.Join(nand, "game")
	entries, err := os.ReadDir(game)
	if err != nil {
		return err
	}
	found := false
	for _, entry := range entries {
		p := filepath.Join(game, entry.Name())
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("invalid native game namespace entry %s", entry.Name())
		}
		if entry.Name() == title {
			found = true
			continue
		}
		children, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		if entry.Name() == "＄locks" && len(children) == 0 {
			continue
		}
		if entry.Name() == "TEST12345" && len(children) == 1 && children[0].Name() == "USRDIR" {
			sub := filepath.Join(p, "USRDIR")
			inf, err := os.Lstat(sub)
			if err != nil || !inf.IsDir() || inf.Mode()&os.ModeSymlink != 0 {
				return ErrInstallUnverified
			}
			inside, err := os.ReadDir(sub)
			if err == nil && len(inside) == 0 {
				continue
			}
		}
		return fmt.Errorf("unexpected or nonempty native initialization entry %s beside title %s", entry.Name(), title)
	}
	if !found {
		return fmt.Errorf("native expected title %s is missing", title)
	}
	return nil
}
