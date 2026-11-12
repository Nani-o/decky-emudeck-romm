package rpcs3

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/adapters/system"
	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

var (
	ErrRPCS3Running       = errors.New("RPCS3 must be closed before installing a PS3 package")
	ErrTitleAlreadyExists = errors.New("RPCS3 already has this title; refusing to overwrite it")
	ErrLicenseExists      = errors.New("RPCS3 already has this license; refusing to overwrite it")
	ErrInstallUnverified  = errors.New("RPCS3 install did not produce a verified title; staging was kept for review")
)

// Runner is injectable so tests never launch the real RPCS3 process.
type Runner interface {
	Run(context.Context, string, ...string) error
}

// ProcessLister reads the local process table. An error blocks installation.
type ProcessLister func() ([]domain.Process, error)

type Installer struct {
	config             Config
	initErr            error
	runner             Runner
	processes          ProcessLister
	validateExecutable func(string) error
	now                func() time.Time
	mu                 sync.Mutex
	removeTitle        func(string, installedInventory) error
	archiveRename      func(string, string) error
	extraRunner        func(string) Runner
	overlayWrite       func(string, string) error
}

// New constructs the production adapter. It uses the installed AppImage, the
// user's existing RPCS3 config and the real /proc process list.
func New(config Config) *Installer {
	normalized, err := config.normalized()
	if err != nil {
		return &Installer{initErr: err}
	}
	return &Installer{
		config: normalized,
		runner: commandRunner{home: normalized.HomeDir, xdgConfigHome: configXDGHome(normalized.ConfigDir)},
		processes: func() ([]domain.Process, error) {
			return system.Processes(normalized.ProcRoot)
		},
		validateExecutable: validateAppImage,
		now:                time.Now,
	}
}

// Install installs one verified base-game package through the qualified RPCS3
// AppImage command and verifies the resulting files. It never deletes staging,
// RPCS3 firmware, configuration, or user save data.
func (i *Installer) Install(ctx context.Context, romID int, stagingDir string) (domain.PS3InstallResult, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.initErr != nil {
		return domain.PS3InstallResult{}, i.initErr
	}
	if ctx == nil || romID <= 0 {
		return domain.PS3InstallResult{}, fmt.Errorf("invalid PS3 install request")
	}
	bundle, err := InspectBundle(stagingDir)
	if err != nil {
		return domain.PS3InstallResult{}, err
	}
	if err := i.validateExecutable(i.config.AppImagePath); err != nil {
		return domain.PS3InstallResult{}, err
	}
	profile, err := i.config.profile()
	if err != nil {
		return domain.PS3InstallResult{}, err
	}
	physicalRoot, physicalErr := filepath.EvalSymlinks(profile.DevHDD0)
	if physicalErr != nil || physicalRoot != profile.DevHDD0 {
		return domain.PS3InstallResult{}, fmt.Errorf("RPCS3 storage contains a redirect")
	}
	launcher, err := desktopEntry(i.config.AppImagePath, filepath.Join(profile.DevHDD0, "game", bundle.Identity.TitleID, "USRDIR", "EBOOT.BIN"), bundle.Identity.TitleID)
	if err != nil {
		return domain.PS3InstallResult{}, err
	}
	if err := i.ensureRPCS3Closed(); err != nil {
		return domain.PS3InstallResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return domain.PS3InstallResult{}, err
	}

	if err := i.completePendingRestart(romID, bundle, profile); err != nil {
		return domain.PS3InstallResult{}, err
	}
	intent, found, err := readIntent(i.config.IntentDir, bundle.Identity.TitleID)
	if err != nil {
		return domain.PS3InstallResult{}, fmt.Errorf("read PS3 install intent: %w", err)
	}
	if found {
		restarted, restartErr := i.restartAfterRemoved(intent, bundle, profile)
		if restartErr != nil {
			return domain.PS3InstallResult{}, restartErr
		}
		if restarted {
			found = false
		}
	}
	if found {
		if err := i.refusePendingOverlay(intent.TitleID); err != nil {
			return domain.PS3InstallResult{}, err
		}
		if (intent.Version != 2 && intent.Version != 3) || intent.BaseKind == "disc" || intent.Inventory == nil || intent.DevHDD0 != profile.DevHDD0 {
			return domain.PS3InstallResult{}, ErrIntentNeedsReview
		}
		if intent.Phase == intentVerified && sameIntentBundle(intent, romID, bundle, profile.UserID) {
			verificationBundle := bundle
			verificationBundle.EntryCount = intent.EntryCount
			if err := i.verifyInstalled(profile, verificationBundle); err == nil {
				if err := checkInstalledInventory(filepath.Join(profile.DevHDD0, "game", intent.TitleID), *intent.Inventory, false); err != nil {
					return domain.PS3InstallResult{}, ErrIntentNeedsReview
				}
				if err := i.verifyIntentLicenses(intent, false, false); err != nil {
					return domain.PS3InstallResult{}, err
				}
				return i.result(romID, bundle.Identity, profile.UserID, launcher), nil
			}
		}
		return domain.PS3InstallResult{}, ErrIntentNeedsReview
	}

	_, _, titleExists, err := pathUnderRoot(profile.DevHDD0, "game", bundle.Identity.TitleID)
	if err != nil {
		return domain.PS3InstallResult{}, fmt.Errorf("inspect RPCS3 title target: %w", err)
	}
	if titleExists {
		return domain.PS3InstallResult{}, ErrTitleAlreadyExists
	}
	_, exdataInfo, exdataExists, err := pathUnderRoot(profile.DevHDD0, "home", profile.UserID, "exdata")
	if err != nil {
		return domain.PS3InstallResult{}, fmt.Errorf("inspect RPCS3 active user exdata: %w", err)
	}
	if exdataExists && (exdataInfo == nil || !exdataInfo.IsDir()) {
		return domain.PS3InstallResult{}, fmt.Errorf("RPCS3 active user exdata path is not a directory")
	}
	if exdataExists {
		_, _, licenseExists, err := pathUnderRoot(profile.DevHDD0, "home", profile.UserID, "exdata", bundle.Identity.ContentID+".rap")
		if err != nil {
			return domain.PS3InstallResult{}, fmt.Errorf("inspect RPCS3 license target: %w", err)
		}
		if licenseExists {
			return domain.PS3InstallResult{}, ErrLicenseExists
		}
	}

	started := i.now().UTC()
	intent = installIntent{
		Version:       2,
		DevHDD0:       profile.DevHDD0,
		LicenseOwned:  true,
		EntryCount:    bundle.EntryCount,
		ROMID:         romID,
		ContentID:     bundle.Identity.ContentID,
		TitleID:       bundle.Identity.TitleID,
		RPCS3User:     profile.UserID,
		PackageSHA256: bundle.PackageSHA256,
		RAPSHA256:     bundle.RAPSHA256,
		Phase:         intentInstalling,
		StartedAt:     started,
	}
	if err := createIntent(i.config.IntentDir, intent); err != nil {
		return domain.PS3InstallResult{}, err
	}

	logBefore, logSnapshotErr := snapshotNativeLog(filepath.Join(i.config.ConfigDir, "RPCS3.log"))
	runStartedAt := time.Now()
	installCtx, cancel := context.WithTimeout(ctx, i.config.Timeout)
	err = i.runner.Run(installCtx, i.config.AppImagePath, "--headless", "--installpkg", bundle.Directory)
	installContextErr := installCtx.Err()
	cancel()
	var teardown *headlessTeardownError
	knownTeardown := errors.As(err, &teardown) && ctx.Err() == nil && installContextErr == nil
	if ctx.Err() != nil || installContextErr != nil || err != nil && !knownTeardown {
		reason := "rpcs3_command_failed"
		if ctx.Err() != nil || installContextErr != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = "process_interrupted"
		}
		cause := errors.Join(err, ctx.Err(), installContextErr)
		if journalErr := i.needsReview(intent, reason); journalErr != nil {
			return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, fmt.Errorf("RPCS3 command failed: %w", cause), journalErr)
		}
		return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err, ctx.Err(), installContextErr)
	}

	currentProfile, profileErr := i.config.profile()
	if profileErr != nil || currentProfile != profile {
		if err := i.needsReview(intent, "rpcs3_config_changed_during_install"); err != nil {
			return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err)
		}
		return domain.PS3InstallResult{}, ErrInstallUnverified
	}
	currentBundle, bundleErr := InspectBundle(bundle.Directory)
	if bundleErr != nil || !sameBundle(bundle, currentBundle) {
		if err := i.needsReview(intent, "staging_changed_during_install"); err != nil {
			return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err)
		}
		return domain.PS3InstallResult{}, ErrInstallUnverified
	}
	if err := i.verifyInstalled(profile, bundle); err != nil {
		if journalErr := i.needsReview(intent, "installed_files_failed_verification"); journalErr != nil {
			return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, journalErr)
		}
		return domain.PS3InstallResult{}, ErrInstallUnverified
	}

	if knownTeardown {
		if logSnapshotErr != nil || freshNativeInstallSuccess(filepath.Join(i.config.ConfigDir, "RPCS3.log"), logBefore, runStartedAt, bundle) != nil {
			if err := i.needsReview(intent, "native_completion_log_unverified"); err != nil {
				return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err)
			}
			return domain.PS3InstallResult{}, ErrInstallUnverified
		}
	}
	if ctx.Err() != nil {
		if err := i.needsReview(intent, "process_interrupted"); err != nil {
			return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err)
		}
		return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, ctx.Err())
	}

	inventory, inventoryErr := captureInstalledInventory(filepath.Join(profile.DevHDD0, "game", bundle.Identity.TitleID))
	if inventoryErr != nil || len(inventory.Files)+len(inventory.Directories) != int(bundle.EntryCount) {
		if err := i.needsReview(intent, "installed_ownership_unverified"); err != nil {
			return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err)
		}
		return domain.PS3InstallResult{}, ErrInstallUnverified
	}
	if ctx.Err() != nil {
		if err := i.needsReview(intent, "process_interrupted"); err != nil {
			return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err)
		}
		return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, ctx.Err())
	}
	intent.Inventory = &inventory
	intent.Phase = intentVerified
	intent.VerifiedAt = i.now().UTC()
	if err := updateIntent(i.config.IntentDir, intent); err != nil {
		return domain.PS3InstallResult{}, errors.Join(ErrInstallUnverified, err)
	}
	return i.result(romID, bundle.Identity, profile.UserID, launcher), nil
}

func (i *Installer) ensureRPCS3Closed() error {
	if i.processes == nil {
		return fmt.Errorf("RPCS3 process check is unavailable")
	}
	processes, err := i.processes()
	if err != nil {
		return fmt.Errorf("list processes before PS3 install: %w", err)
	}
	for _, process := range processes {
		if processIsRPCS3(process) {
			return ErrRPCS3Running
		}
	}
	return nil
}

func (i *Installer) needsReview(intent installIntent, reason string) error {
	intent.Phase = intentNeedsReview
	intent.Reason = reason
	return updateIntent(i.config.IntentDir, intent)
}

func (i *Installer) verifyInstalled(profile rpcs3Profile, bundle Bundle) error {
	_, titleInfo, found, err := pathUnderRoot(profile.DevHDD0, "game", bundle.Identity.TitleID)
	if err != nil || !found || titleInfo == nil || !titleInfo.IsDir() {
		return fmt.Errorf("installed title directory is missing or invalid")
	}
	sfoPath, sfoInfo, found, err := pathUnderRoot(profile.DevHDD0, "game", bundle.Identity.TitleID, "PARAM.SFO")
	if err != nil || !found || sfoInfo == nil || !sfoInfo.Mode().IsRegular() {
		return fmt.Errorf("installed PARAM.SFO is missing or invalid")
	}
	titleID, err := inspectParamsfoTitleID(sfoPath)
	if err != nil || titleID != bundle.Identity.TitleID {
		return fmt.Errorf("installed PARAM.SFO title ID does not match the PKG")
	}
	_, ebootInfo, found, err := pathUnderRoot(profile.DevHDD0, "game", bundle.Identity.TitleID, "USRDIR", "EBOOT.BIN")
	if err != nil || !found || ebootInfo == nil || !ebootInfo.Mode().IsRegular() || ebootInfo.Size() == 0 {
		return fmt.Errorf("installed EBOOT.BIN is missing or invalid")
	}
	if err := verifyPackageEntryCount(filepath.Join(profile.DevHDD0, "game", bundle.Identity.TitleID), bundle.EntryCount); err != nil {
		return err
	}
	licensePath, licenseInfo, found, err := pathUnderRoot(profile.DevHDD0, "home", profile.UserID, "exdata", bundle.Identity.ContentID+".rap")
	if err != nil || !found || licenseInfo == nil || !licenseInfo.Mode().IsRegular() || licenseInfo.Size() != 16 {
		return fmt.Errorf("installed RAP license is missing or invalid")
	}
	licenseHash, err := sha256File(licensePath, 16)
	if err != nil || licenseHash != bundle.RAPSHA256 {
		return fmt.Errorf("installed RAP license does not match the staged license")
	}
	return nil
}

func (i *Installer) result(romID int, identity domain.PS3Identity, userID, launcher string) domain.PS3InstallResult {
	return domain.PS3InstallResult{ROMID: romID, Identity: identity, RPCS3User: userID, VerifiedAt: i.now().UTC(), DesktopEntry: launcher}
}

func sameIntentBundle(intent installIntent, romID int, bundle Bundle, userID string) bool {
	return intent.ROMID == romID && intent.ContentID == bundle.Identity.ContentID && intent.TitleID == bundle.Identity.TitleID &&
		intent.RPCS3User == userID && intent.PackageSHA256 == bundle.PackageSHA256 && intent.RAPSHA256 == bundle.RAPSHA256
}

func sameBundle(a, b Bundle) bool {
	return a.Identity == b.Identity && a.PackageSHA256 == b.PackageSHA256 && a.RAPSHA256 == b.RAPSHA256 && a.PackageSize == b.PackageSize && a.EntryCount == b.EntryCount
}

func pathUnderRoot(root string, components ...string) (string, fs.FileInfo, bool, error) {
	if err := requireRealDirectory(root); err != nil {
		return "", nil, false, err
	}
	current := root
	var info fs.FileInfo
	for index, component := range components {
		if component == "" || !filepath.IsLocal(component) || strings.ContainsAny(component, `/\`) {
			return "", nil, false, fmt.Errorf("unsafe RPCS3 path component")
		}
		current = filepath.Join(current, component)
		foundInfo, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return current, nil, false, nil
		}
		if err != nil {
			return "", nil, false, err
		}
		info = foundInfo
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", nil, false, fmt.Errorf("RPCS3 target path contains a symbolic link")
		}
		if index < len(components)-1 && !info.IsDir() {
			return "", nil, false, fmt.Errorf("RPCS3 target parent is not a directory")
		}
	}
	return current, info, true, nil
}

type commandRunner struct {
	home          string
	xdgConfigHome string
	xdgCacheHome  string
	xdgDataHome   string
	xdgStateHome  string
}

func (r commandRunner) Run(ctx context.Context, executable string, args ...string) error {
	command := exec.CommandContext(ctx, executable, args...)
	values := map[string]string{"HOME": r.home, "XDG_CONFIG_HOME": r.xdgConfigHome, "XDG_CACHE_HOME": r.xdgConfigHome}
	if r.xdgCacheHome != "" {
		values["XDG_CACHE_HOME"] = r.xdgCacheHome
	}
	if r.xdgDataHome != "" {
		values["XDG_DATA_HOME"] = r.xdgDataHome
	}
	if r.xdgStateHome != "" {
		values["XDG_STATE_HOME"] = r.xdgStateHome
	}
	command.Env = overrideEnv(os.Environ(), values)
	command.Stdout = io.Discard
	stderr := &boundedTail{limit: 16 << 10}
	command.Stderr = stderr
	err := command.Run()
	if knownHeadlessTeardown(err, ctx.Err(), string(stderr.data)) {
		return &headlessTeardownError{}
	}
	return err
}

func overrideEnv(environment []string, values map[string]string) []string {
	out := make([]string, 0, len(environment)+len(values))
	for _, item := range environment {
		key, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, replace := values[key]; !replace {
			out = append(out, item)
		}
	}
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	return out
}

func validateAppImage(path string) error {
	if err := validateAppImageFile(path); err != nil {
		return err
	}
	// RPCS3 also ships Type-2 AppImages with uruntime/DWARFS. Executing
	// the installed emulator does not require interpreting its embedded FS.
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	var header [64]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return fmt.Errorf("invalid RPCS3 ELF header: %w", err)
	}
	if string(header[:4]) != "\x7fELF" || header[4] != 2 || header[5] != 1 || header[6] != 1 ||
		string(header[8:11]) != "AI\x02" || binary.LittleEndian.Uint16(header[18:20]) != 62 ||
		binary.LittleEndian.Uint32(header[20:24]) != 1 || binary.LittleEndian.Uint16(header[52:54]) != 64 {
		return fmt.Errorf("RPCS3 must be an ELF64 x86_64 Type-2 AppImage")
	}
	kind := binary.LittleEndian.Uint16(header[16:18])
	if kind != 2 && kind != 3 {
		return fmt.Errorf("RPCS3 ELF is not executable")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := uint64(info.Size())
	programOffset, programSize, programCount := binary.LittleEndian.Uint64(header[32:40]), uint64(binary.LittleEndian.Uint16(header[54:56])), uint64(binary.LittleEndian.Uint16(header[56:58]))
	if programOffset < 64 || programSize != 56 || programCount == 0 || programOffset > size || programSize*programCount > size-programOffset {
		return fmt.Errorf("RPCS3 ELF program table is invalid")
	}
	sectionOffset, sectionSize, sectionCount := binary.LittleEndian.Uint64(header[40:48]), uint64(binary.LittleEndian.Uint16(header[58:60])), uint64(binary.LittleEndian.Uint16(header[60:62]))
	if sectionCount > 0 && (sectionOffset < 64 || sectionSize != 64 || sectionOffset > size || sectionSize*sectionCount > size-sectionOffset) {
		return fmt.Errorf("RPCS3 ELF section table is invalid")
	}
	return nil
}

func validateAppImageFile(path string) error {
	if filepath.Base(path) != "rpcs3.AppImage" {
		return fmt.Errorf("RPCS3 executable must be the installed rpcs3.AppImage")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("RPCS3 AppImage is unavailable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("RPCS3 AppImage is not an executable regular file")
	}
	return nil
}

var _ Runner = commandRunner{}
