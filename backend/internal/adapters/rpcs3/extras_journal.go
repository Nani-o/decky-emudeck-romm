package rpcs3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

type extraReceipt struct {
	SourceFileID  int                   `json:"source_file_id"`
	Kind          domain.PS3PackageKind `json:"kind"`
	ContentID     string                `json:"content_id"`
	PackageSHA256 string                `json:"package_sha256"`
	RAPSHA256     string                `json:"rap_sha256,omitempty"`
	AppVersion    string                `json:"app_version,omitempty"`
}
type extraLicense struct {
	ContentID string `json:"content_id"`
	SHA256    string `json:"sha256"`
	Owned     bool   `json:"owned"`
}

func validExtendedIntent(in installIntent) bool {
	if (in.BaseKind != "psn" && in.BaseKind != "disc") || !filepath.IsAbs(in.DevHDD0) || filepath.Clean(in.DevHDD0) != in.DevHDD0 || in.DevHDD0 == string(filepath.Separator) || strings.ContainsAny(in.DevHDD0, "\x00\r\n") || in.EntryCount == 0 || in.Inventory == nil || in.Phase != intentVerified || len(in.Extras) == 0 || len(in.Extras) > 128 || len(in.ExtraLicenses) > 128 {
		return false
	}
	if in.BaseKind == "psn" && (!in.LicenseOwned || !validSHA256(in.RAPSHA256)) {
		return false
	}
	if in.BaseKind == "disc" && (in.LicenseOwned || in.RAPSHA256 != "") {
		return false
	}
	if validateInstalledInventory(*in.Inventory) != nil || len(in.Inventory.Files)+len(in.Inventory.Directories) != int(in.EntryCount) {
		return false
	}
	ids := map[int]bool{}
	for _, x := range in.Extras {
		id, err := domain.ParsePS3ContentID(x.ContentID)
		if err != nil || id.TitleID != in.TitleID || x.SourceFileID <= 0 || ids[x.SourceFileID] || !validSHA256(x.PackageSHA256) || (x.RAPSHA256 != "" && !validSHA256(x.RAPSHA256)) || (x.Kind != domain.PS3PackageUpdate && x.Kind != domain.PS3PackageDLC) {
			return false
		}
		ids[x.SourceFileID] = true
		if x.Kind == domain.PS3PackageUpdate {
			if _, err = versionNumber(x.AppVersion); err != nil {
				return false
			}
		}
	}
	licenses := map[string]bool{}
	if in.LicenseOwned {
		licenses[in.ContentID] = true
	}
	for _, l := range in.ExtraLicenses {
		id, err := domain.ParsePS3ContentID(l.ContentID)
		if err != nil || id.TitleID != in.TitleID || !validSHA256(l.SHA256) || licenses[l.ContentID] {
			return false
		}
		licenses[l.ContentID] = true
	}
	for _, x := range in.Extras {
		if x.RAPSHA256 == "" {
			continue
		}
		matched := false
		for _, l := range intentLicenses(in) {
			if l.ContentID == x.ContentID && l.SHA256 == x.RAPSHA256 {
				matched = true
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
func intentLicenses(in installIntent) []extraLicense {
	out := append([]extraLicense(nil), in.ExtraLicenses...)
	if in.LicenseOwned {
		out = append([]extraLicense{{ContentID: in.ContentID, SHA256: in.RAPSHA256, Owned: true}}, out...)
	}
	return out
}
func (i *Installer) verifyIntentLicenses(in installIntent, allowMissing, ownedOnly bool) error {
	for _, l := range intentLicenses(in) {
		if ownedOnly && !l.Owned {
			continue
		}
		p, info, found, err := pathUnderRoot(in.DevHDD0, "home", in.RPCS3User, "exdata", l.ContentID+".rap")
		if err != nil {
			return err
		}
		if !found {
			if allowMissing {
				continue
			}
			return ErrIntentNeedsReview
		}
		if !info.Mode().IsRegular() || info.Size() != 16 {
			return ErrIntentNeedsReview
		}
		sha, err := sha256File(p, 16)
		if err != nil || sha != l.SHA256 {
			return ErrIntentNeedsReview
		}
	}
	return nil
}
func (i *Installer) refusePendingOverlay(title string) error {
	_, err := os.Lstat(i.overlayPath(title))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("PS3 content installation is incomplete; retry its update before launching another installation or uninstalling")
}
func (i *Installer) BaseIdentity(ctx context.Context, romID int) (domain.PS3Identity, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.initErr != nil {
		return domain.PS3Identity{}, i.initErr
	}
	if ctx == nil || romID <= 0 {
		return domain.PS3Identity{}, ErrIntentNeedsReview
	}
	if err := ctx.Err(); err != nil {
		return domain.PS3Identity{}, err
	}
	in, found, err := i.ownedIntent(romID)
	if err != nil {
		return domain.PS3Identity{}, err
	}
	if !found || (in.Version != 2 && in.Version != 3) || in.Phase != intentVerified || in.BaseKind == "disc" || in.Inventory == nil {
		return domain.PS3Identity{}, ErrIntentNeedsReview
	}
	profile, err := i.config.profile()
	if err != nil || profile.DevHDD0 != in.DevHDD0 || profile.UserID != in.RPCS3User {
		return domain.PS3Identity{}, ErrIntentNeedsReview
	}
	pending, present, pendingErr := i.readOverlay(in.TitleID)
	if pendingErr != nil {
		return domain.PS3Identity{}, pendingErr
	}
	if present {
		base, err := i.validatePendingBase(pending, romID, profile)
		if err != nil {
			return domain.PS3Identity{}, err
		}
		return domain.PS3Identity{ContentID: base.ContentID, TitleID: base.TitleID}, nil
	}
	if err = checkInstalledInventory(extraTitleRoot(in.DevHDD0, in.TitleID), *in.Inventory, false); err != nil {
		return domain.PS3Identity{}, err
	}
	if err = i.verifyIntentLicenses(in, false, false); err != nil {
		return domain.PS3Identity{}, err
	}
	title, err := inspectParamsfoTitleID(filepath.Join(extraTitleRoot(in.DevHDD0, in.TitleID), "PARAM.SFO"))
	if err != nil || title != in.TitleID {
		return domain.PS3Identity{}, ErrIntentNeedsReview
	}
	return domain.PS3Identity{ContentID: in.ContentID, TitleID: in.TitleID}, nil
}
func savePrivateJSON(p string, v any, exclusive bool) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	limit := maxIntentSize
	if strings.HasSuffix(p, ".overlay.json") {
		limit = maxIntentSize * 3
	}
	if len(data) > limit {
		return fmt.Errorf("PS3 operation journal exceeds its read-compatible bound")
	}
	dir := filepath.Dir(p)
	if err = ensurePrivateDirectory(dir); err != nil {
		return err
	}
	if exclusive {
		if _, err = os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return ErrIntentNeedsReview
		}
	}
	f, err := os.CreateTemp(dir, ".ps3-write-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if exclusive {
		if err = os.Link(tmp, p); err != nil {
			return err
		}
	} else if err = os.Rename(tmp, p); err != nil {
		return err
	}
	return syncDirectory(dir)
}

func removedNativePaths(in installIntent) [][]string {
	out := [][]string{{"game", in.TitleID}}
	for _, l := range intentLicenses(in) {
		if l.Owned {
			out = append(out, []string{"home", in.RPCS3User, "exdata", l.ContentID + ".rap"})
		}
	}
	return out
}
func (i *Installer) refusePendingForROM(romID int) error {
	entries, err := os.ReadDir(i.config.IntentDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".overlay.json") {
			continue
		}
		title := strings.TrimSuffix(entry.Name(), ".overlay.json")
		r, found, err := i.readOverlay(title)
		if err != nil {
			return err
		}
		if found && r.After.ROMID == romID {
			return fmt.Errorf("PS3 content installation is incomplete; retry it before uninstalling")
		}
	}
	return nil
}
func syntheticBundle(in installIntent) Bundle {
	return Bundle{Identity: domain.PS3Identity{ContentID: in.ContentID, TitleID: in.TitleID}, PackageSHA256: in.PackageSHA256, RAPSHA256: in.RAPSHA256, EntryCount: in.EntryCount}
}
func (i *Installer) finishExtraRestart(req domain.PS3ExtrasRequest, profile rpcs3Profile) error {
	data, err := os.ReadFile(i.restartPath(req.TitleID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > maxIntentSize+8192 {
		return ErrIntentNeedsReview
	}
	var r restartReceipt
	if json.Unmarshal(data, &r) != nil || r.Intent.ROMID != req.ROMID || r.Intent.TitleID != req.TitleID || (r.Intent.BaseKind == "disc") != req.DiscBase {
		return ErrIntentNeedsReview
	}
	return i.completePendingRestart(req.ROMID, syntheticBundle(r.Intent), profile)
}

// PreparedBase only resolves an already installed base while an explicitly
// requested extras transaction is pending. It never commits or reports a ready
// game: the caller must invoke InstallExtras before saving download ownership.
// The generic Install path intentionally continues to refuse a hybrid title.
func (i *Installer) PreparedBase(ctx context.Context, romID int, sourceDir string) (domain.PS3InstallResult, bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	var out domain.PS3InstallResult
	if i.initErr != nil {
		return out, false, i.initErr
	}
	if ctx == nil || romID <= 0 {
		return out, false, ErrIntentNeedsReview
	}
	if err := ctx.Err(); err != nil {
		return out, false, err
	}
	bundle, err := InspectBundle(sourceDir)
	if err != nil {
		return out, false, err
	}
	pending, found, err := i.readOverlay(bundle.Identity.TitleID)
	if err != nil || !found {
		return out, false, err
	}
	profile, err := i.config.profile()
	if err != nil {
		return out, false, err
	}
	if err = i.ensureRPCS3Closed(); err != nil {
		return out, false, err
	}
	base, err := i.validatePendingBase(pending, romID, profile)
	if err != nil {
		return out, false, err
	}
	if !sameIntentBundle(base, romID, bundle, profile.UserID) {
		return out, false, ErrIntentNeedsReview
	}
	launcher, err := desktopEntry(i.config.AppImagePath, filepath.Join(base.DevHDD0, "game", base.TitleID, "USRDIR", "EBOOT.BIN"), base.TitleID)
	if err != nil {
		return out, false, err
	}
	out = domain.PS3InstallResult{ROMID: romID, Identity: bundle.Identity, RPCS3User: profile.UserID, VerifiedAt: base.VerifiedAt, DesktopEntry: launcher}
	return out, true, nil
}
func (i *Installer) validatePendingBase(pending overlayReceipt, romID int, profile rpcs3Profile) (installIntent, error) {
	if pending.Before == nil || pending.Before.BaseKind == "disc" || pending.Before.ROMID != romID || pending.After.ROMID != romID || pending.After.DevHDD0 != profile.DevHDD0 || pending.After.RPCS3User != profile.UserID {
		return installIntent{}, ErrIntentNeedsReview
	}
	if err := i.overlayMatchesCurrent(pending); err != nil {
		return installIntent{}, err
	}
	if err := i.checkHybrid(pending); err != nil {
		return installIntent{}, err
	}
	if err := i.verifyIntentLicenses(*pending.Before, false, false); err != nil {
		return installIntent{}, err
	}
	return *pending.Before, nil
}
