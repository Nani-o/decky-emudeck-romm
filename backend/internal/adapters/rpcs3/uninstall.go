package rpcs3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type removalReceipt struct {
	Version         int    `json:"version"`
	ROMID           int    `json:"rom_id"`
	TitleID         string `json:"title_id"`
	OwnershipSHA256 string `json:"ownership_sha256"`
	Phase           string `json:"phase"`
}

func ownershipHash(intent installIntent) string {
	b, _ := json.Marshal(intent)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (i *Installer) removalPath(intent installIntent) string {
	return filepath.Join(i.config.IntentDir, intent.TitleID+".removal.json")
}
func (i *Installer) readRemoval(intent installIntent) (removalReceipt, bool, error) {
	path := i.removalPath(intent)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return removalReceipt{}, false, nil
	}
	if err != nil {
		return removalReceipt{}, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return removalReceipt{}, false, ErrIntentNeedsReview
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return removalReceipt{}, false, err
	}
	var r removalReceipt
	if json.Unmarshal(data, &r) != nil || r.Version != 1 || r.ROMID != intent.ROMID || r.TitleID != intent.TitleID || r.OwnershipSHA256 != ownershipHash(intent) || (r.Phase != "removing" && r.Phase != "removed") {
		return removalReceipt{}, false, ErrIntentNeedsReview
	}
	return r, true, nil
}
func (i *Installer) writeRemoval(intent installIntent, phase string) error {
	r := removalReceipt{1, intent.ROMID, intent.TitleID, ownershipHash(intent), phase}
	data, _ := json.Marshal(r)
	f, err := os.CreateTemp(i.config.IntentDir, ".removal-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
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
	if err = os.Rename(name, i.removalPath(intent)); err != nil {
		return err
	}
	return syncDirectory(i.config.IntentDir)
}
func (i *Installer) ownedIntent(romID int) (installIntent, bool, error) {
	entries, err := os.ReadDir(i.config.IntentDir)
	if errors.Is(err, os.ErrNotExist) {
		return installIntent{}, false, nil
	}
	if err != nil {
		return installIntent{}, false, err
	}
	var selected installIntent
	found := false
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 14 || !strings.HasSuffix(name, ".json") {
			continue
		}
		intent, _, err := readIntent(i.config.IntentDir, strings.TrimSuffix(name, ".json"))
		if err != nil {
			return installIntent{}, false, err
		}
		if intent.ROMID != romID {
			continue
		}
		if found {
			return installIntent{}, false, ErrIntentNeedsReview
		}
		selected = intent
		found = true
	}
	return selected, found, nil
}
func (i *Installer) removalCheck(ctx context.Context, romID int) (installIntent, bool, bool, error) {
	if i.initErr != nil {
		return installIntent{}, false, false, i.initErr
	}
	if ctx == nil || romID <= 0 {
		return installIntent{}, false, false, fmt.Errorf("invalid PS3 removal request")
	}
	if err := ctx.Err(); err != nil {
		return installIntent{}, false, false, err
	}
	intent, found, err := i.ownedIntent(romID)
	if err != nil || !found {
		return intent, found, false, err
	}
	if intent.Version != 2 || intent.Phase != intentVerified || intent.Inventory == nil || !intent.LicenseOwned {
		return intent, true, false, ErrIntentNeedsReview
	}
	profile, err := i.config.profile()
	if err != nil {
		return intent, true, false, err
	}
	if profile.DevHDD0 != intent.DevHDD0 || profile.UserID != intent.RPCS3User {
		return intent, true, false, ErrIntentNeedsReview
	}
	if err = i.ensureRPCS3Closed(); err != nil {
		return intent, true, false, err
	}
	receipt, resuming, err := i.readRemoval(intent)
	if err != nil {
		return intent, true, false, err
	}
	root, _, exists, err := pathUnderRoot(intent.DevHDD0, "game", intent.TitleID)
	if err != nil {
		return intent, true, false, err
	}
	if receipt.Phase == "removed" && exists {
		return intent, true, false, ErrIntentNeedsReview
	}
	if err = checkInstalledInventory(root, *intent.Inventory, resuming); err != nil {
		return intent, true, false, err
	}
	license, info, exists, err := pathUnderRoot(intent.DevHDD0, "home", intent.RPCS3User, "exdata", intent.ContentID+".rap")
	if err != nil {
		return intent, true, false, err
	}
	if !exists {
		if !resuming {
			return intent, true, false, ErrIntentNeedsReview
		}
	} else {
		if receipt.Phase == "removed" || !info.Mode().IsRegular() || info.Size() != 16 {
			return intent, true, false, ErrIntentNeedsReview
		}
		sum, err := sha256File(license, 16)
		if err != nil || sum != intent.RAPSHA256 {
			return intent, true, false, ErrIntentNeedsReview
		}
	}
	return intent, true, resuming, nil
}

// CheckRemoval verifies ownership without changing native files. A missing receipt
// means an ordinary disc game, which has no native installation to remove.
func (i *Installer) CheckRemoval(ctx context.Context, romID int) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, found, _, err := i.removalCheck(ctx, romID)
	return found, err
}

// Remove deletes only the recorded native title and its newly installed RAP.
// Save data, trophies, firmware and every other title are outside this operation.
func (i *Installer) Remove(ctx context.Context, romID int) (bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	intent, found, resuming, err := i.removalCheck(ctx, romID)
	if err != nil || !found {
		return found, err
	}
	if !resuming {
		if err = i.writeRemoval(intent, "removing"); err != nil {
			return true, err
		}
	}
	if err = ctx.Err(); err != nil {
		return true, err
	}
	root := filepath.Join(intent.DevHDD0, "game", intent.TitleID)
	remove := i.removeTitle
	if remove == nil {
		remove = removeInstalledInventory
	}
	if err = remove(root, *intent.Inventory); err != nil {
		return true, err
	}
	if err = ctx.Err(); err != nil {
		return true, err
	}
	license, info, exists, err := pathUnderRoot(intent.DevHDD0, "home", intent.RPCS3User, "exdata", intent.ContentID+".rap")
	if err != nil {
		return true, err
	}
	if exists {
		if !info.Mode().IsRegular() {
			return true, ErrIntentNeedsReview
		}
		sum, err := sha256File(license, 16)
		if err != nil || sum != intent.RAPSHA256 {
			return true, ErrIntentNeedsReview
		}
		if err = os.Remove(license); err != nil {
			return true, err
		}
		if err = syncDirectory(filepath.Dir(license)); err != nil {
			return true, err
		}
	}
	return true, i.writeRemoval(intent, "removed")
}

func (i *Installer) restartAfterRemoved(intent installIntent, bundle Bundle, profile rpcs3Profile) (bool, error) {
	receipt, found, err := i.readRemoval(intent)
	if err != nil || !found {
		return false, err
	}
	if receipt.Phase != "removed" {
		return false, ErrIntentNeedsReview
	}
	if intent.Version != 2 || intent.DevHDD0 != profile.DevHDD0 || !sameIntentBundle(intent, intent.ROMID, bundle, profile.UserID) {
		return false, ErrIntentNeedsReview
	}
	for _, parts := range [][]string{{"game", intent.TitleID}, {"home", intent.RPCS3User, "exdata", intent.ContentID + ".rap"}} {
		_, _, exists, err := pathUnderRoot(intent.DevHDD0, parts...)
		if err != nil {
			return false, err
		}
		if exists {
			return false, ErrIntentNeedsReview
		}
	}
	marker := restartReceipt{Intent: intent, Removal: receipt}
	data, err := json.Marshal(marker)
	if err != nil {
		return false, err
	}
	file, err := os.CreateTemp(i.config.IntentDir, ".ps3-restart-*")
	if err != nil {
		return false, err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return false, err
	}
	if _, err = os.Lstat(i.restartPath(intent.TitleID)); !errors.Is(err, os.ErrNotExist) {
		return false, ErrIntentNeedsReview
	}
	if err = os.Rename(tmp, i.restartPath(intent.TitleID)); err != nil {
		return false, err
	}
	if err = syncDirectory(i.config.IntentDir); err != nil {
		return false, err
	}
	if err = i.completePendingRestart(intent.ROMID, bundle, profile); err != nil {
		return false, err
	}
	return true, nil
}

// A durable restart receipt makes the two archival renames independently
// resumable. It never authorizes overwriting a newly introduced native title.
type restartReceipt struct {
	Intent  installIntent  `json:"intent"`
	Removal removalReceipt `json:"removal"`
}

func (i *Installer) restartPath(title string) string {
	return filepath.Join(i.config.IntentDir, title+".restart.json")
}
func (i *Installer) completePendingRestart(romID int, bundle Bundle, profile rpcs3Profile) error {
	path := i.restartPath(bundle.Identity.TitleID)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxIntentSize+8192 {
		return ErrIntentNeedsReview
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var marker restartReceipt
	if json.Unmarshal(data, &marker) != nil {
		return ErrIntentNeedsReview
	}
	intent := marker.Intent
	if intent.Version != 2 || intent.Phase != intentVerified || !validOwnedIntent(intent) || !validIntentTimestamps(intent) || intent.DevHDD0 != profile.DevHDD0 || !sameIntentBundle(intent, romID, bundle, profile.UserID) || marker.Removal.Version != 1 || marker.Removal.ROMID != romID || marker.Removal.TitleID != intent.TitleID || marker.Removal.Phase != "removed" || marker.Removal.OwnershipSHA256 != ownershipHash(intent) {
		return ErrIntentNeedsReview
	}
	for _, parts := range [][]string{{"game", intent.TitleID}, {"home", intent.RPCS3User, "exdata", intent.ContentID + ".rap"}} {
		_, _, exists, err := pathUnderRoot(intent.DevHDD0, parts...)
		if err != nil {
			return err
		}
		if exists {
			return ErrIntentNeedsReview
		}
	}
	history := filepath.Join(i.config.IntentDir, "history")
	if err = ensurePrivateDirectory(history); err != nil {
		return err
	}
	prefix := filepath.Join(history, intent.TitleID+"-"+ownershipHash(intent))
	expectedIntent, _ := json.Marshal(intent)
	expectedRemoval, _ := json.Marshal(marker.Removal)
	for _, entry := range []struct {
		source, target string
		expected       []byte
	}{{i.removalPath(intent), prefix + ".removal.json", expectedRemoval}, {intentFile(i.config.IntentDir, intent.TitleID), prefix + ".json", expectedIntent}} {
		sourceFound, err := matchingRestartFile(entry.source, entry.expected)
		if err != nil {
			return err
		}
		targetFound, err := matchingRestartFile(entry.target, entry.expected)
		if err != nil {
			return err
		}
		if !sourceFound && !targetFound {
			return ErrIntentNeedsReview
		}
		if sourceFound {
			if targetFound {
				if err = os.Remove(entry.source); err != nil {
					return err
				}
			} else {
				rename := i.archiveRename
				if rename == nil {
					rename = os.Rename
				}
				if err = rename(entry.source, entry.target); err != nil {
					return err
				}
			}
		}
		if err = syncDirectory(history); err != nil {
			return err
		}
		if err = syncDirectory(i.config.IntentDir); err != nil {
			return err
		}
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(i.config.IntentDir)
}
func matchingRestartFile(path string, expected []byte) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxIntentSize {
		return false, ErrIntentNeedsReview
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var got, want any
	if json.Unmarshal(data, &got) != nil || json.Unmarshal(expected, &want) != nil {
		return false, ErrIntentNeedsReview
	}
	canonicalGot, _ := json.Marshal(got)
	canonicalWant, _ := json.Marshal(want)
	if string(canonicalGot) != string(canonicalWant) {
		return false, ErrIntentNeedsReview
	}
	return true, nil
}
