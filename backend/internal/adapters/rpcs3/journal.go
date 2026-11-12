package rpcs3

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

const maxIntentSize = 8 << 20

const (
	intentInstalling  = "installing"
	intentNeedsReview = "needs_review"
	intentVerified    = "verified"
)

var ErrIntentNeedsReview = errors.New("an earlier PS3 install needs review; automatic retry is refused")

type installIntent struct {
	Version       int                 `json:"version"`
	BaseKind      string              `json:"base_kind,omitempty"`
	Extras        []extraReceipt      `json:"extras,omitempty"`
	ExtraLicenses []extraLicense      `json:"extra_licenses,omitempty"`
	DevHDD0       string              `json:"dev_hdd0,omitempty"`
	LicenseOwned  bool                `json:"license_owned,omitempty"`
	EntryCount    uint32              `json:"entry_count,omitempty"`
	Inventory     *installedInventory `json:"inventory,omitempty"`
	ROMID         int                 `json:"rom_id"`
	ContentID     string              `json:"content_id"`
	TitleID       string              `json:"title_id"`
	RPCS3User     string              `json:"rpcs3_user_id"`
	PackageSHA256 string              `json:"package_sha256"`
	RAPSHA256     string              `json:"rap_sha256"`
	Phase         string              `json:"phase"`
	Reason        string              `json:"reason,omitempty"`
	StartedAt     time.Time           `json:"started_at"`
	VerifiedAt    time.Time           `json:"verified_at,omitempty"`
}

func intentFile(directory, titleID string) string {
	return filepath.Join(directory, titleID+".json")
}

func readIntent(directory, titleID string) (installIntent, bool, error) {
	path := intentFile(directory, titleID)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return installIntent{}, false, nil
	}
	if err != nil {
		return installIntent{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || info.Size() > maxIntentSize {
		return installIntent{}, false, fmt.Errorf("PS3 install intent is not a bounded regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return installIntent{}, false, err
	}
	var intent installIntent
	if err := json.Unmarshal(data, &intent); err != nil {
		return installIntent{}, false, fmt.Errorf("read PS3 install intent: %w", err)
	}
	_, identityErr := domain.ParsePS3Identity(intent.ContentID, intent.TitleID)
	if (intent.Version != 1 && intent.Version != 2 && intent.Version != 3) || !validOwnedIntent(intent) || intent.TitleID != titleID || !validIntentPhase(intent.Phase) ||
		intent.ROMID <= 0 || identityErr != nil || len(intent.RPCS3User) != 8 || !asciiNumeric(intent.RPCS3User) ||
		!validSHA256(intent.PackageSHA256) || (!validSHA256(intent.RAPSHA256) && !(intent.Version == 3 && intent.BaseKind == "disc" && intent.RAPSHA256 == "")) || len(intent.Reason) > 2048 ||
		intent.StartedAt.IsZero() || !validIntentTimestamps(intent) {
		return installIntent{}, false, fmt.Errorf("PS3 install intent is incomplete or unsupported")
	}
	return intent, true, nil
}

func createIntent(directory string, intent installIntent) error {
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxIntentSize {
		return fmt.Errorf("PS3 install intent exceeds the size limit")
	}
	path := intentFile(directory, intent.TitleID)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrIntentNeedsReview
		}
		return fmt.Errorf("create durable PS3 install intent: %w", err)
	}
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("persist PS3 install intent: %w", err)
	}
	return syncDirectory(directory)
}

func updateIntent(directory string, intent installIntent) error {
	if err := ensurePrivateDirectory(directory); err != nil {
		return err
	}
	path := intentFile(directory, intent.TitleID)
	current, found, err := readIntent(directory, intent.TitleID)
	if err != nil {
		return err
	}
	if !found || current.Phase != intentInstalling || current.Version != intent.Version ||
		current.ROMID != intent.ROMID || current.ContentID != intent.ContentID || current.TitleID != intent.TitleID ||
		current.RPCS3User != intent.RPCS3User || current.PackageSHA256 != intent.PackageSHA256 ||
		current.RAPSHA256 != intent.RAPSHA256 || current.DevHDD0 != intent.DevHDD0 || current.LicenseOwned != intent.LicenseOwned || current.EntryCount != intent.EntryCount || !current.StartedAt.Equal(intent.StartedAt) {
		return ErrIntentNeedsReview
	}
	data, err := json.MarshalIndent(intent, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxIntentSize {
		return fmt.Errorf("PS3 install intent exceeds the size limit")
	}
	tmp, err := os.CreateTemp(directory, ".ps3-intent-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("update PS3 install intent: %w", err)
	}
	return syncDirectory(directory)
}

func ensurePrivateDirectory(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	remainder := strings.TrimPrefix(absolute, volume+string(filepath.Separator))
	if remainder == "" {
		return fmt.Errorf("PS3 intent directory cannot be a filesystem root")
	}
	current := volume + string(filepath.Separator)
	components := strings.Split(remainder, string(filepath.Separator))
	for _, component := range components {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("PS3 intent directory contains a non-directory or symbolic link")
		}
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("PS3 intent directory contains a non-directory or symbolic link")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("PS3 intent directory must be private to its owner")
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }()
	return dir.Sync()
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func validIntentPhase(value string) bool {
	switch value {
	case intentInstalling, intentNeedsReview, intentVerified:
		return true
	default:
		return false
	}
}

func validIntentTimestamps(intent installIntent) bool {
	switch intent.Phase {
	case intentInstalling:
		return intent.Reason == "" && intent.VerifiedAt.IsZero()
	case intentNeedsReview:
		return intent.Reason != "" && intent.VerifiedAt.IsZero()
	case intentVerified:
		return intent.Reason == "" && !intent.VerifiedAt.IsZero()
	default:
		return false
	}
}

func validOwnedIntent(intent installIntent) bool {
	if intent.Version == 3 {
		return validExtendedIntent(intent)
	}
	if intent.Version == 1 {
		return intent.DevHDD0 == "" && !intent.LicenseOwned && intent.EntryCount == 0 && intent.Inventory == nil
	}
	if !filepath.IsAbs(intent.DevHDD0) || filepath.Clean(intent.DevHDD0) != intent.DevHDD0 || intent.DevHDD0 == string(filepath.Separator) || strings.ContainsAny(intent.DevHDD0, "\x00\r\n") || !intent.LicenseOwned || intent.EntryCount == 0 {
		return false
	}
	if intent.Inventory == nil {
		return intent.Phase != intentVerified
	}
	return validateInstalledInventory(*intent.Inventory) == nil && len(intent.Inventory.Files)+len(intent.Inventory.Directories) == int(intent.EntryCount)
}
