package domain

import (
	"fmt"
	"path/filepath"
	"strings"
)

// SelectPS3ContentFiles preserves every explicitly attached update and DLC.
// PS3 patches can form a chain, so Switch's latest-only selection is unsuitable.
// Embedded files of a disc remain ordinary game files, regardless of extension.
func SelectPS3ContentFiles(files []ROMFile) ([]ROMFile, error) {
	selected := allContentFiles(files)
	ids := map[int]bool{}
	for i, f := range selected {
		if f.Path == "" {
			f.Path = f.Name
		}
		if f.ID <= 0 || ids[f.ID] || !filepath.IsLocal(filepath.FromSlash(f.Path)) || !filepath.IsLocal(f.Name) || f.SizeBytes < 0 {
			return nil, &Error{Kind: KindCantDownload, Message: "RomM lists an invalid or duplicate PS3 content file."}
		}
		ids[f.ID] = true
		if !f.IsGame() {
			switch strings.ToLower(filepath.Ext(f.Name)) {
			case ".pkg", ".rap", ".zip", ".7z":
			default:
				return nil, &Error{Kind: KindCantDownload, Message: "PS3 updates and DLC must be PKG/RAP files or ZIP/7z package archives."}
			}
		}
		selected[i] = f
	}
	if len(GameFiles(selected)) == 0 {
		return nil, &Error{Kind: KindCantDownload, Message: "RomM lists PS3 attachments without a base game."}
	}
	return selected, nil
}

func PS3HasAttachments(files []ROMFile) bool {
	for _, f := range allContentFiles(files) {
		if !f.IsGame() {
			return true
		}
	}
	return false
}

func PS3HasAttachmentSnapshots(snapshot []FileSnapshot) bool {
	for _, f := range snapshot {
		if f.Category == "update" || f.Category == "dlc" {
			return true
		}
	}
	return false
}

// PS3TransferFiles is shared by preview, queue and execution. Existing bases
// stay on disk; all current attachments are fetched together so native package
// metadata, licenses and patch dependencies can be verified as one manifest.
func PS3TransferFiles(files []ROMFile, old *OwnedROM) ([]ROMFile, error) {
	selected, err := SelectPS3ContentFiles(files)
	if err != nil || old == nil {
		return selected, err
	}
	if UpdateAvailable(BaseSnapshots(old.Snapshot), files) {
		return nil, &Error{Kind: KindCantDownload, Message: "The PS3 base game changed. Remove and download it again before installing its packages."}
	}
	current := map[int]FileSnapshot{}
	for _, f := range selected {
		current[f.ID] = SnapshotOf(f)
	}
	for _, previous := range old.Snapshot {
		if previous.Category != "update" && previous.Category != "dlc" {
			continue
		}
		now, ok := current[previous.FileID]
		if !ok || now.Category != previous.Category || snapshotChanged(previous, now) {
			return nil, &Error{Kind: KindCantDownload, Message: fmt.Sprintf("An installed PS3 package (%d) was replaced or removed on RomM. Remove and download the game again to rebuild its native installation.", previous.FileID)}
		}
	}
	var transfer []ROMFile
	for _, f := range selected {
		if !f.IsGame() {
			transfer = append(transfer, f)
		}
	}
	return transfer, nil
}

func ps3ContentSnapshots(files []ROMFile) []FileSnapshot {
	selected := allContentFiles(files)
	out := make([]FileSnapshot, 0, len(selected))
	for _, f := range selected {
		out = append(out, SnapshotOf(f))
	}
	return out
}
