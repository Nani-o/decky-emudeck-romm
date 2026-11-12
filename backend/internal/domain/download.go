package domain

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

// GameState is where a game stands between the Server and this Deck.
type GameState string

const (
	StateOnRomM      GameState = "on_romm"
	StateQueued      GameState = "queued"
	StateDownloading GameState = "downloading"
	StateInstalled   GameState = "installed"
	// StateDeleting is an installed game whose files are being checked and deleted.
	StateDeleting GameState = "deleting"
	// StateFailed is a download that stopped on an error. It stays in the queue, keeping what it
	// downloaded, until it is retried or dismissed.
	StateFailed GameState = "failed"
	// StateOnDeck is a game with a User-managed file where its download would go. The plugin never
	// downloads over it; Adopt or Link to RomM take it in instead.
	StateOnDeck GameState = "on_deck"
	// StateUpdate is an installed game whose files on the Server changed since the plugin
	// downloaded it.
	StateUpdate GameState = "update_available"
)

// Installed says whether a game in state s is on this Deck, the plugin's: installed, with an
// update, or being deleted until its files are gone.
func (s GameState) Installed() bool {
	return s == StateInstalled || s == StateUpdate || s == StateDeleting
}

// QueuedDownload is a download in the queue as state.db keeps it, so the queue outlives the
// backend.
type QueuedDownload struct {
	ROMID        int
	PlatformID   int
	PlatformName string
	Name         string
	TotalBytes   int64
	// Problem is why the download failed; nil while it is queued or running.
	Problem *Error
	// Partial is the file whose bytes are in staging, so a later attempt resumes them only while
	// the Server still has that same file. Its FileID is zero before the first attempt.
	Partial FileSnapshot
	// Update is set on the download of an update of a Plugin-owned game, which replaces its files.
	Update bool
}

// ROMFile is one file of a ROM on the Server.
type ROMFile struct {
	ID   int
	Name string
	// Path is where the file sits inside its game, with / separators: its name for a game that is
	// one file, or its path under the game's folder. It doesn't depend on how the Server's library
	// is laid out.
	Path      string
	SizeBytes int64
	// Category is the kind of file: "game" for the game itself, or manual, dlc, update and so on.
	// Nested files outside a category folder have none.
	Category string
	// TitleID and TitleVersion are RomM package metadata; TitleVersion is the raw CNMT integer.
	TitleID      string
	TitleVersion string
	LastModified time.Time
	CRC          string
	MD5          string
	SHA1         string
}

// ROMWithFiles is a ROM on the Server and its files.
type ROMWithFiles struct {
	ROMID int
	Files []ROMFile
}

// Hashed says whether the Server has any hash for f, without which f can't be matched.
func (f ROMFile) Hashed() bool { return f.SHA1 != "" || f.MD5 != "" || f.CRC != "" }

// IsGame says whether the file is part of the game itself, rather than a manual, DLC, soundtrack
// or the like.
func (f ROMFile) IsGame() bool {
	category := strings.ToLower(strings.TrimSpace(f.Category))
	return category == "" || category == "game"
}

// ContentFiles returns Switch files the emulator reads from a game folder: its game, updates and
// DLC. Other RomM categories remain outside the installed content manifest.
func ContentFiles(files []ROMFile) []ROMFile {
	selected, err := SelectContentFiles(files)
	if err != nil {
		// Comparison-only callers keep ambiguous metadata visible; downloading uses
		// SelectContentFiles directly and refuses it before changing anything.
		return allContentFiles(files)
	}
	return selected
}

func allContentFiles(files []ROMFile) []ROMFile {
	var out []ROMFile
	for _, f := range files {
		category := strings.ToLower(strings.TrimSpace(f.Category))
		switch category {
		case "", "game":
			f.Category = ""
			out = append(out, f)
		case "update", "dlc":
			f.Category = category
			out = append(out, f)
		}
	}
	return out
}

// GameFiles returns the files that make up the game itself.
func GameFiles(files []ROMFile) []ROMFile {
	var out []ROMFile
	for _, f := range files {
		if f.IsGame() {
			out = append(out, f)
		}
	}
	return out
}

// FileSnapshot is a game file as the Server described it when the plugin downloaded it, which
// later fetches compare against to find updates.
type FileSnapshot struct {
	FileID int
	// Category is empty for the base game, or update/dlc for Switch content. Empty is also how
	// snapshots written before category tracking identify base files.
	Category     string
	SizeBytes    int64
	LastModified time.Time
	// HashKind and Hash are the strongest hash the Server had for the file: sha1, md5 or crc. Both
	// are empty when it had none.
	HashKind string
	Hash     string
}

// SnapshotOf takes the snapshot of f.
func SnapshotOf(f ROMFile) FileSnapshot {
	category := strings.ToLower(strings.TrimSpace(f.Category))
	if category == "game" {
		category = ""
	}
	s := FileSnapshot{FileID: f.ID, Category: category, SizeBytes: f.SizeBytes, LastModified: f.LastModified}
	switch {
	case f.SHA1 != "":
		s.HashKind, s.Hash = "sha1", f.SHA1
	case f.MD5 != "":
		s.HashKind, s.Hash = "md5", f.MD5
	case f.CRC != "":
		s.HashKind, s.Hash = "crc", f.CRC
	}
	return s
}

// UpdateAvailable says whether the Server's files of a game differ from the snapshot taken when the
// plugin downloaded it: a game file added, removed or re-hashed. Where the snapshot has no hash, a
// changed size or modification time counts instead. Manuals, DLC and the like never count, and the
// files on this Deck aren't looked at: extracting or converting a game changes their bytes.
func UpdateAvailable(snapshot []FileSnapshot, files []ROMFile) bool {
	game := GameFiles(files)
	return filesChanged(snapshot, game, false)
}

// ContentUpdateAvailable compares the base game, updates and DLC recorded for Switch. It doesn't
// count manuals or unrelated RomM categories.
func ContentUpdateAvailable(snapshot []FileSnapshot, files []ROMFile) bool {
	return filesChanged(snapshot, ContentFiles(files), true)
}

func filesChanged(snapshot []FileSnapshot, current []ROMFile, compareCategory bool) bool {
	if !compareCategory {
		snapshot = BaseSnapshots(snapshot)
	}
	if len(current) != len(snapshot) {
		return true
	}
	byID := make(map[int]FileSnapshot, len(snapshot))
	for _, s := range snapshot {
		byID[s.FileID] = s
	}
	for _, f := range current {
		s, ok := byID[f.ID]
		if !ok || compareCategory && s.Category != SnapshotOf(f).Category {
			return true
		}
		if now := f.hash(s.HashKind); s.Hash != "" && now != "" {
			if !strings.EqualFold(strings.TrimLeft(now, "0"), strings.TrimLeft(s.Hash, "0")) {
				return true
			}
			continue
		}
		if f.SizeBytes != s.SizeBytes || !f.LastModified.Equal(s.LastModified) {
			return true
		}
	}
	return false
}

// BaseSnapshots returns only the snapshots that identify the base game. This keeps Find, Adopt and
// server identity checks independent from optional Switch updates and DLC.
func BaseSnapshots(snapshot []FileSnapshot) []FileSnapshot {
	var out []FileSnapshot
	for _, s := range snapshot {
		if s.Category == "" || s.Category == "game" {
			s.Category = ""
			out = append(out, s)
		}
	}
	return out
}

// ContentSnapshotsOf takes the snapshot of each Switch game, update and DLC file.
func ContentSnapshotsOf(files []ROMFile) []FileSnapshot {
	current := ContentFiles(files)
	out := make([]FileSnapshot, 0, len(current))
	for _, f := range current {
		out = append(out, SnapshotOf(f))
	}
	return out
}

// ChangedContent returns the current Switch files that differ from the recorded manifest and the
// old file IDs that need replacement or removal. Files with unchanged metadata and hashes are
// omitted so an update that only adds DLC doesn't fetch the base game again.
func ChangedContent(snapshot []FileSnapshot, files []ROMFile) (download []ROMFile, oldIDs map[int]bool) {
	current := ContentFiles(files)
	now := make(map[int]FileSnapshot, len(current))
	for _, f := range current {
		now[f.ID] = SnapshotOf(f)
	}
	oldIDs = map[int]bool{}
	oldByID := make(map[int]FileSnapshot, len(snapshot))
	for _, s := range snapshot {
		oldByID[s.FileID] = s
		latest, ok := now[s.FileID]
		if !ok || latest.Category != s.Category || snapshotChanged(s, latest) {
			oldIDs[s.FileID] = true
		}
	}
	for _, f := range current {
		old, ok := oldByID[f.ID]
		if !ok || old.Category != SnapshotOf(f).Category || snapshotChanged(old, SnapshotOf(f)) {
			download = append(download, f)
		}
	}
	// A changed volume requires its entire selected RAR cohort. Extracted files
	// belong to the first volume, so its owned outputs must also be replaced even
	// when only a later volume changed on the server.
	changedIDs := map[int]bool{}
	for _, f := range download {
		changedIDs[f.ID] = true
	}
	changedGroups := map[string]bool{}
	for _, f := range current {
		if f.Category != "update" {
			continue
		}
		key, _, part := switchUpdateCohort(f)
		if part > 0 && changedIDs[f.ID] {
			changedGroups[key] = true
		}
	}
	if len(changedGroups) > 0 {
		download = nil
		for _, f := range current {
			key, _, part := switchUpdateCohort(f)
			complete := f.Category == "update" && part > 0 && changedGroups[key]
			if changedIDs[f.ID] || complete {
				download = append(download, f)
			}
			if complete {
				if _, exists := oldByID[f.ID]; exists {
					oldIDs[f.ID] = true
				}
			}
		}
	}
	return download, oldIDs
}

func snapshotChanged(old, latest FileSnapshot) bool {
	if old.Hash != "" && latest.Hash != "" && old.HashKind == latest.HashKind {
		return !strings.EqualFold(strings.TrimLeft(old.Hash, "0"), strings.TrimLeft(latest.Hash, "0"))
	}
	return old.SizeBytes != latest.SizeBytes || !old.LastModified.Equal(latest.LastModified)
}

// hash returns f's hash of kind, empty when the Server has none.
func (f ROMFile) hash(kind string) string {
	switch kind {
	case "sha1":
		return f.SHA1
	case "md5":
		return f.MD5
	case "crc":
		return f.CRC
	}
	return ""
}

// PlacedFile is a file the plugin put on disk, as it was right after placing it.
type PlacedFile struct {
	Path      string
	SizeBytes int64
	ModTime   time.Time
	// SourceFileID ties a placed file to the RomM file it came from, so updates can replace only
	// changed content while preserving unchanged files. Zero is legacy or generated output.
	SourceFileID int
	// SHA1 is of the bytes on disk. RomM hashes a zipped ROM's contents rather than the zip, so it
	// can differ from the Server's hash of the same file.
	SHA1 string
}

// OwnedROM is the record that makes a ROM Plugin-owned: what the plugin placed, and what the
// Server had when it did.
type OwnedROM struct {
	ROMID       int
	PlatformID  int
	Files       []PlacedFile
	Snapshot    []FileSnapshot
	InstalledAt time.Time
	// Ignored is the Server's game files when the user chose Ignore this update, nil when they
	// never did. That update isn't offered again; one changing the files further is.
	Ignored []FileSnapshot
	// Media is the art the plugin placed in ES-DE's media folder. ES-DE's scraper may replace it, so
	// unlike Files a changed one never hands the game back to the user: it's just left alone.
	Media []PlacedFile
}

// SnapshotsOf takes the snapshot of each of files' game files.
func SnapshotsOf(files []ROMFile) []FileSnapshot {
	game := GameFiles(files)
	out := make([]FileSnapshot, 0, len(game))
	for _, f := range game {
		out = append(out, SnapshotOf(f))
	}
	return out
}

// SnapshotsBySystem selects the update manifest installed for a system.
func SnapshotsBySystem(system System, files []ROMFile) []FileSnapshot {
	if system.Folder == "switch" {
		return ContentSnapshotsOf(files)
	}
	if system.Folder == "ps3" {
		return ps3ContentSnapshots(files)
	}
	return SnapshotsOf(files)
}

// UpdateAvailableBySystem compares the content a system installs. Switch tracks updates and DLC;
// other systems retain the base-game-only contract.
func UpdateAvailableBySystem(system System, snapshot []FileSnapshot, files []ROMFile) bool {
	if system.Folder == "switch" {
		return ContentUpdateAvailable(snapshot, files)
	}
	if system.Folder == "ps3" {
		return filesChanged(snapshot, allContentFiles(files), true)
	}
	return UpdateAvailable(snapshot, files)
}

// HasUpdate says whether the Server's files are an update of rec the user hasn't ignored.
func (rec OwnedROM) HasUpdate(files []ROMFile) bool {
	if !UpdateAvailable(rec.Snapshot, files) {
		return false
	}
	return rec.Ignored == nil || UpdateAvailable(rec.Ignored, files)
}

// HasUpdateFor uses Switch's content manifest where the system supports updates and DLC.
func (rec OwnedROM) HasUpdateFor(system System, files []ROMFile) bool {
	if !UpdateAvailableBySystem(system, rec.Snapshot, files) {
		return false
	}
	return rec.Ignored == nil || UpdateAvailableBySystem(system, rec.Ignored, files)
}

var sizeUnits = []string{"B", "KB", "MB", "GB", "TB"}

// FormatSize writes a size in bytes as a short label, e.g. "1.4 GB", the way the frontend does.
func FormatSize(bytes int64) string {
	value, unit := float64(bytes), 0
	for value >= 1024 && unit < len(sizeUnits)-1 {
		value /= 1024
		unit++
	}
	if value < 10 && unit > 0 {
		return fmt.Sprintf("%.1f %s", value, sizeUnits[unit])
	}
	return fmt.Sprintf("%.0f %s", math.Round(value), sizeUnits[unit])
}

// SystemForSlug returns the EmuDeck System a RomM platform's games go in.
func SystemForSlug(slug string) (System, bool) {
	for _, s := range Systems {
		if slices.Contains(s.Slugs, slug) {
			return s, true
		}
	}
	return System{}, false
}
