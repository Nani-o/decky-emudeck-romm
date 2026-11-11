package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

func TestReplacedFileOnRomMShowsUpdate(t *testing.T) {
	f := newDownloadFixture(t, 2)
	ctx := context.Background()
	f.installedChrono(t)
	f.server.mu.Lock()
	f.server.files[1][0].LastModified = time.Unix(9000, 0)
	f.server.files[1][0].SizeBytes++
	f.server.mu.Unlock()

	if err := f.downloads.CheckUpdates(ctx, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if states, _ := f.downloads.States(ctx); states[1] != domain.StateUpdate {
		t.Fatalf("states %v", states)
	}
	if c := f.ownershipChange(t); c.ROMID != 1 || c.Kind != OwnershipUpdate || c.State != domain.StateUpdate {
		t.Fatalf("change %+v", c)
	}
	// Checking again changes nothing and says nothing.
	if err := f.downloads.CheckUpdates(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-f.ownership:
		t.Fatalf("change %+v", c)
	default:
	}
}

func TestMetadataEditShowsNoUpdate(t *testing.T) {
	f := newDownloadFixture(t, 2)
	ctx := context.Background()
	f.installedChrono(t)
	// A rename on RomM touches the ROM, not its files.
	f.catalog.mu.Lock()
	r := f.catalog.roms[1]
	r.Name = "Chrono Trigger (Renamed)"
	f.catalog.roms[1] = r
	f.catalog.mu.Unlock()

	if err := f.downloads.CheckUpdates(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	if states, _ := f.downloads.States(ctx); states[1] != domain.StateInstalled {
		t.Fatalf("states %v", states)
	}
}

func TestSwitchAddedContentDownloadsOnlyNewFilesAndHidesThemFromESDE(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base bytes"})
	base := f.installed(t, 13)
	if len(base.Snapshot) != 1 || base.Snapshot[0].Category != "" || len(base.Files) != 1 {
		t.Fatalf("base manifest %+v", base)
	}
	root := filepath.Join(f.root, "roms", "switch", "Mario Kart 8 Deluxe.nsp")
	basePath := filepath.Join(root, "Mario Kart 8 Deluxe.nsp")
	if got := readFile(t, basePath); got != "base bytes" {
		t.Fatalf("base bytes %q", got)
	}
	for len(f.server.started) > 0 {
		<-f.server.started
	}

	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base bytes"},
		gameFile{"update/4.0.1.nsp", "update", "update bytes"},
		gameFile{"dlc/Wave 1.nsp", "dlc", "dlc bytes"},
		gameFile{"manual/Readme.pdf", "manual", "manual"},
	)
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if c := f.ownershipChange(t); c.ROMID != 13 || c.State != domain.StateUpdate {
		t.Fatalf("update change %+v", c)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateInstalled || u.Problem != nil || u.Total != int64(len("update bytes")+len("dlc bytes")) {
		t.Fatalf("download end %+v", u)
	}
	var downloaded []int
	for len(f.server.started) > 0 {
		downloaded = append(downloaded, <-f.server.started)
	}
	if !slices.Equal(downloaded, []int{1301, 1302}) {
		t.Fatalf("downloaded file IDs %v", downloaded)
	}
	if got := readFile(t, basePath); got != "base bytes" {
		t.Fatalf("unchanged base was modified: %q", got)
	}
	if readFile(t, filepath.Join(root, "update", "4.0.1.nsp")) != "update bytes" ||
		readFile(t, filepath.Join(root, "dlc", "Wave 1.nsp")) != "dlc bytes" {
		t.Fatalf("Switch content layout %v", listDir(t, root))
	}
	for _, rel := range []string{"update/noload.txt", "dlc/noload.txt"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Fatalf("missing ES-DE exclusion marker %s: %v", rel, err)
		}
	}
	owned, ok, err := f.owned.Owned(ctx, 13)
	if err != nil || !ok || len(owned.Snapshot) != 3 || len(owned.Files) != 5 {
		t.Fatalf("updated ownership %+v, ok=%t, err=%v", owned, ok, err)
	}

	// Removing a RomM update and DLC removes only those plugin-owned files, without another base transfer.
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base bytes"})
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if c := f.ownershipChange(t); c.State != domain.StateUpdate {
		t.Fatalf("removal change %+v", c)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateInstalled || u.Problem != nil {
		t.Fatalf("removal end %+v", u)
	}
	if got := readFile(t, basePath); got != "base bytes" {
		t.Fatalf("base changed during content removal: %q", got)
	}
	for _, rel := range []string{"update/4.0.1.nsp", "dlc/Wave 1.nsp"} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Fatalf("removed content still exists at %s: %v", rel, err)
		}
	}
	if got := listDir(t, root); !slices.Contains(got, "Mario Kart 8 Deluxe.nsp") {
		t.Fatalf("root lost the ES-DE base entry: %v", got)
	}
}

func legacySwitchBase(t *testing.T, f *downloadFixture, loose bool) (string, string, string) {
	t.Helper()
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "legacy base"})
	f.server.mu.Lock()
	base := f.server.files[13][0]
	f.server.mu.Unlock()
	romRoot := filepath.Join(f.root, "roms", "switch")
	root := filepath.Join(romRoot, base.Name)
	path := filepath.Join(root, base.Name)
	if loose {
		path = root
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("legacy base"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := domain.OwnedROM{
		ROMID:      13,
		PlatformID: 13,
		Files: []domain.PlacedFile{{
			Path: path, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), SHA1: sha1Hex("legacy base"), SourceFileID: 0,
		}},
		Snapshot: []domain.FileSnapshot{domain.SnapshotOf(base)},
	}
	if err := f.owned.SaveOwned(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return path, root, "legacy base"
}

func TestLegacySwitchBaseUpdateReplacesZeroSourceCohortThenAddsDLCIncrementally(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	basePath, _, _ := legacySwitchBase(t, f, false)
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "updated base"})
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateInstalled || u.Problem != nil {
		t.Fatalf("legacy base update %+v", u)
	}
	if id := f.started(t); id != 1300 {
		t.Fatalf("base update downloaded %d", id)
	}
	if got := readFile(t, basePath); got != "updated base" {
		t.Fatalf("legacy base was not replaced: %q", got)
	}
	updated, ok, err := f.owned.Owned(ctx, 13)
	if err != nil || !ok || len(updated.Files) != 1 || updated.Files[0].SourceFileID != 1300 {
		t.Fatalf("base ownership after update %+v, ok=%t, err=%v", updated, ok, err)
	}

	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "updated base"},
		gameFile{"dlc/Wave 1.nsp", "dlc", "new DLC"},
	)
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateInstalled || u.Problem != nil {
		t.Fatalf("incremental DLC update %+v", u)
	}
	if id := f.started(t); id != 1301 {
		t.Fatalf("DLC-only update downloaded %d", id)
	}
	if got := readFile(t, basePath); got != "updated base" {
		t.Fatalf("DLC-only update changed base: %q", got)
	}
}

func TestLegacySwitchExtrasOnlyKeepsZeroSourceBase(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	basePath, _, _ := legacySwitchBase(t, f, false)
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "legacy base"},
		gameFile{"dlc/Wave 1.nsp", "dlc", "new DLC"},
	)
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateInstalled || u.Problem != nil {
		t.Fatalf("legacy extra update %+v", u)
	}
	if id := f.started(t); id != 1301 {
		t.Fatalf("extras-only update downloaded %d", id)
	}
	if got := readFile(t, basePath); got != "legacy base" {
		t.Fatalf("extras-only update changed base: %q", got)
	}
	rec, ok, err := f.owned.Owned(ctx, 13)
	if err != nil || !ok || len(rec.Files) < 2 || rec.Files[0].SourceFileID != 0 {
		t.Fatalf("legacy base ownership wasn't retained: %+v, ok=%t, err=%v", rec, ok, err)
	}
}

func TestLegacySwitchArchivePayloadCohortSurvivesMultipleExtraUpdates(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	baseArchive := string(zipOf(t,
		member{"Mario Kart 8 Deluxe.nsp", "legacy base v1"},
		member{"README.txt", "legacy readme v1"},
	))
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.zip", "game", baseArchive})
	rec := f.installed(t, 13)
	if len(rec.Files) != 2 || len(rec.Snapshot) != 1 {
		t.Fatalf("expected one archived base snapshot with two payloads: %+v", rec)
	}
	root := filepath.Join(f.root, "roms", "switch", "Example [0100000000000000]")
	basePath := filepath.Join(root, "Mario Kart 8 Deluxe.nsp")
	readmePath := filepath.Join(root, "README.txt")
	for i := range rec.Files {
		rec.Files[i].SourceFileID = 0 // v0.3 records did not retain archive-member provenance.
	}
	if err := f.owned.SaveOwned(ctx, rec); err != nil {
		t.Fatal(err)
	}
	drainStarted := func(want ...int) {
		t.Helper()
		var got []int
		for len(f.server.started) > 0 {
			got = append(got, <-f.server.started)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("downloaded IDs %v, want %v", got, want)
		}
	}
	drainStarted(1300)

	update := func(files []gameFile, wantID int) {
		t.Helper()
		f.serveSet(13, files...)
		if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.downloads.Queue(ctx, 13); err != nil {
			t.Fatal(err)
		}
		if done := f.end(t, 13); done.State != domain.StateInstalled || done.Problem != nil {
			t.Fatalf("legacy archive update %+v", done)
		}
		drainStarted(wantID)
	}

	update([]gameFile{
		{"Mario Kart 8 Deluxe.zip", "game", baseArchive},
		{"dlc/Wave 1.nsp", "dlc", "wave 1"},
	}, 1301)
	if readFile(t, basePath) != "legacy base v1" || readFile(t, readmePath) != "legacy readme v1" {
		t.Fatal("first DLC update changed the legacy base archive payloads")
	}

	update([]gameFile{
		{"Mario Kart 8 Deluxe.zip", "game", baseArchive},
		{"dlc/Wave 1.nsp", "dlc", "wave 1"},
		{"dlc/Wave 2.nsp", "dlc", "wave 2"},
	}, 1302)
	if readFile(t, basePath) != "legacy base v1" || readFile(t, readmePath) != "legacy readme v1" {
		t.Fatal("second DLC update changed the legacy base archive payloads")
	}

	updatedArchive := string(zipOf(t,
		member{"Mario Kart 8 Deluxe.nsp", "updated base v2"},
		member{"README.txt", "updated readme v2"},
	))
	update([]gameFile{
		{"Mario Kart 8 Deluxe.zip", "game", updatedArchive},
		{"dlc/Wave 1.nsp", "dlc", "wave 1"},
		{"dlc/Wave 2.nsp", "dlc", "wave 2"},
	}, 1300)
	if readFile(t, basePath) != "updated base v2" || readFile(t, readmePath) != "updated readme v2" {
		t.Fatal("base update did not replace every legacy archive payload")
	}
	if readFile(t, filepath.Join(root, "dlc", "Wave 1.nsp")) != "wave 1" ||
		readFile(t, filepath.Join(root, "dlc", "Wave 2.nsp")) != "wave 2" {
		t.Fatal("base update changed the incrementally installed DLC")
	}
}

func TestLegacySwitchMultipartBaseCohortSurvivesMultipleDLCUpdates(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	all := []byte("synthetic multipart NSP payload")
	a, b, c := all[:8], all[8:16], all[16:]
	volumeFiles := func() []gameFile {
		return []gameFile{
			{"Mario Kart 8 Deluxe.part3.rar", "game", string(storedRAR("Mario Kart 8 Deluxe.nsp", all, c, 3, 3, 0))},
			{"Mario Kart 8 Deluxe.part1.rar", "game", string(storedRAR("Mario Kart 8 Deluxe.nsp", all, a, 1, 3, 0))},
			{"Mario Kart 8 Deluxe.part2.rar", "game", string(storedRAR("Mario Kart 8 Deluxe.nsp", all, b, 2, 3, 0))},
		}
	}
	f.serveSet(13, volumeFiles()...)
	rec := f.installed(t, 13)
	if len(rec.Snapshot) != 3 {
		t.Fatalf("expected snapshots for three RAR volumes: %+v", rec.Snapshot)
	}
	for i := range rec.Files {
		rec.Files[i].SourceFileID = 0 // v0.3 recorded the extracted cohort without its source IDs.
	}
	if err := f.owned.SaveOwned(ctx, rec); err != nil {
		t.Fatal(err)
	}

	drainStarted := func(want ...int) {
		t.Helper()
		var got []int
		for len(f.server.started) > 0 {
			got = append(got, <-f.server.started)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("downloaded IDs %v, want %v", got, want)
		}
	}
	drainStarted(1300, 1301, 1302)

	update := func(dlc []gameFile, wantID int) {
		t.Helper()
		files := append(volumeFiles(), dlc...)
		f.serveSet(13, files...)
		if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.downloads.Queue(ctx, 13); err != nil {
			t.Fatal(err)
		}
		if done := f.end(t, 13); done.State != domain.StateInstalled || done.Problem != nil {
			t.Fatalf("legacy multipart update %+v", done)
		}
		drainStarted(wantID)
	}

	update([]gameFile{{"dlc/Wave 1.nsp", "dlc", "wave 1"}}, 1303)
	update([]gameFile{
		{"dlc/Wave 1.nsp", "dlc", "wave 1"},
		{"dlc/Wave 2.nsp", "dlc", "wave 2"},
	}, 1304)

	root := filepath.Join(f.root, "roms", "switch", "Example [0100000000000000]")
	if readFile(t, filepath.Join(root, "Mario Kart 8 Deluxe.nsp")) != string(all) ||
		readFile(t, filepath.Join(root, "dlc", "Wave 1.nsp")) != "wave 1" ||
		readFile(t, filepath.Join(root, "dlc", "Wave 2.nsp")) != "wave 2" {
		t.Fatalf("multipart legacy base or incremental DLC changed: %v", listDir(t, root))
	}
}

func TestLegacyLooseSwitchBaseMovesIntoFolderWithoutRedownload(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	loosePath, root, data := legacySwitchBase(t, f, true)
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", data},
		gameFile{"dlc/Wave 1.nsp", "dlc", "new DLC"},
	)
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateInstalled || u.Problem != nil {
		t.Fatalf("loose file migration %+v", u)
	}
	if id := f.started(t); id != 1301 {
		t.Fatalf("loose-file extras update downloaded %d", id)
	}
	newBase := filepath.Join(root, filepath.Base(root))
	if got := readFile(t, newBase); got != data {
		t.Fatalf("migrated base contains %q", got)
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("loose path wasn't promoted to the ES-DE folder: %v", err)
	}
	if _, err := os.Stat(loosePath + ".romm-legacy"); !os.IsNotExist(err) {
		t.Fatalf("left legacy rollback file: %v", err)
	}
	rec, ok, err := f.owned.Owned(ctx, 13)
	if err != nil || !ok {
		t.Fatalf("ownership lookup: %+v, ok=%t, err=%v", rec, ok, err)
	}
	foundLegacyBase := slices.ContainsFunc(rec.Files, func(file domain.PlacedFile) bool {
		return file.Path == newBase && file.SourceFileID == 0
	})
	if !foundLegacyBase {
		t.Fatalf("base provenance changed during local move: %+v", rec.Files)
	}
}

func TestAmbiguousLegacySwitchOwnershipFailsWithoutTouchingFiles(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	basePath, root, _ := legacySwitchBase(t, f, false)
	extraPath := filepath.Join(root, "dlc", "legacy.nsp")
	if err := os.MkdirAll(filepath.Dir(extraPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extraPath, []byte("ambiguous legacy extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	extraInfo, err := os.Stat(extraPath)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok, err := f.owned.Owned(ctx, 13)
	if err != nil || !ok {
		t.Fatalf("ownership lookup: %+v, ok=%t, err=%v", rec, ok, err)
	}
	rec.Files = append(rec.Files, domain.PlacedFile{Path: extraPath, SizeBytes: extraInfo.Size(), ModTime: extraInfo.ModTime().UTC(), SHA1: sha1Hex("ambiguous legacy extra"), SourceFileID: 0})
	rec.Snapshot = append(rec.Snapshot, domain.FileSnapshot{FileID: 1301, Category: "dlc", SizeBytes: 10, LastModified: time.Unix(100, 0)})
	if err := f.owned.SaveOwned(ctx, rec); err != nil {
		t.Fatal(err)
	}
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "changed base"},
		gameFile{"dlc/new.nsp", "dlc", "new extra"},
	)
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateFailed || kindOf(u.Problem) != domain.KindChangedOnDisk {
		t.Fatalf("ambiguous ownership wasn't refused: %+v", u)
	}
	if readFile(t, basePath) != "legacy base" || readFile(t, extraPath) != "ambiguous legacy extra" || len(f.server.started) != 0 {
		t.Fatal("ambiguous update touched data or started a download")
	}
}

func TestLegacySwitchBaseCohortRejectsZeroIDExtraSnapshot(t *testing.T) {
	cohort, ok, err := legacySwitchBaseCohort(domain.OwnedROM{
		Files: []domain.PlacedFile{{Path: "/roms/switch/Game.nsp/Game.nsp", SourceFileID: 0}},
		Snapshot: []domain.FileSnapshot{
			{FileID: 1300, Category: "game"},
			{FileID: 0, Category: "dlc"},
		},
	})
	if ok || err == nil || len(cohort) != 0 {
		t.Fatalf("zero-ID extra snapshot was treated as safely mapped: cohort=%+v ok=%t err=%v", cohort, ok, err)
	}
}

func TestSwitchBaseRenameKeepsFolderAsFileEntry(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "old base"})
	f.installed(t, 13)
	root := filepath.Join(f.root, "roms", "switch", "Mario Kart 8 Deluxe.nsp")
	oldEntry := filepath.Join(root, "Mario Kart 8 Deluxe.nsp")
	f.serveSet(13, gameFile{"Renamed Mario Kart.nsp", "game", "renamed updated base"})
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateInstalled || u.Problem != nil {
		t.Fatalf("renamed base update %+v", u)
	}
	if got := readFile(t, oldEntry); got != "renamed updated base" {
		t.Fatalf("homonymous launcher entry contains %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "Renamed Mario Kart.nsp")); !os.IsNotExist(err) {
		t.Fatalf("update created a non-launchable child path: %v", err)
	}
}

func TestSwitchBaseExtensionChangeIsRefusedWithoutMutation(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "old base"})
	f.installed(t, 13)
	for len(f.server.started) > 0 {
		<-f.server.started
	}
	root := filepath.Join(f.root, "roms", "switch", "Mario Kart 8 Deluxe.nsp")
	oldEntry := filepath.Join(root, "Mario Kart 8 Deluxe.nsp")
	f.serveSet(13, gameFile{"Renamed Mario Kart.xci", "game", "new XCI content"})
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateFailed || kindOf(u.Problem) != domain.KindCantDownload {
		t.Fatalf("extension change wasn't refused: %+v", u)
	}
	if readFile(t, oldEntry) != "old base" || len(f.server.started) != 0 {
		t.Fatal("extension change touched the installed base")
	}
}

func TestSwitchUpdatePathCollisionPreservesPublishedFiles(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base"},
		gameFile{"update/a.nsp", "update", "old update"},
	)
	f.installed(t, 13)
	root := filepath.Join(f.root, "roms", "switch", "Mario Kart 8 Deluxe.nsp")
	basePath := filepath.Join(root, "Mario Kart 8 Deluxe.nsp")
	updatePath := filepath.Join(root, "update", "a.nsp")
	for len(f.server.started) > 0 {
		<-f.server.started
	}
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base"},
		gameFile{"a.nsp", "update", "new update"},
		gameFile{"update/a.nsp", "update", "duplicate update"},
	)
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateFailed || kindOf(u.Problem) != domain.KindCantDownload {
		t.Fatalf("colliding update was accepted: %+v", u)
	}
	if readFile(t, basePath) != "base" || readFile(t, updatePath) != "old update" || len(f.server.started) != 0 {
		t.Fatal("colliding update altered the installed files")
	}
}

func TestSwitchUpdateCategoryPrefixMatchesMarkerPath(t *testing.T) {
	f := switchFixture(t)
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base"},
		gameFile{"Update/4.0.1.nsp", "update", "update"},
	)
	f.installed(t, 13)
	root := filepath.Join(f.root, "roms", "switch", "Mario Kart 8 Deluxe.nsp")
	if _, err := os.Stat(filepath.Join(root, "update", "4.0.1.nsp")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "update", "noload.txt")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "Update" {
			t.Fatalf("case variant created a second update folder: %v", listDir(t, root))
		}
	}
}

func TestCancelledSwitchReplacementKeepsPublishedContent(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base before"})
	f.installed(t, 13)
	basePath := filepath.Join(f.root, "roms", "switch", "Mario Kart 8 Deluxe.nsp", "Mario Kart 8 Deluxe.nsp")
	f.serveSet(13, gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base after, still downloading"})
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if c := f.ownershipChange(t); c.State != domain.StateUpdate {
		t.Fatalf("update change %+v", c)
	}
	f.server.mu.Lock()
	f.server.gates[1300] = make(chan struct{})
	f.server.mu.Unlock()
	if _, err := f.downloads.Queue(ctx, 13); err != nil {
		t.Fatal(err)
	}
	if id := f.started(t); id != 1300 {
		t.Fatalf("started file %d", id)
	}
	if !f.downloads.Cancel(13) {
		t.Fatal("cancel found no Switch update")
	}
	if u := f.end(t, 13); u.State != domain.StateUpdate {
		t.Fatalf("cancelled update state %+v", u)
	}
	if got := readFile(t, basePath); got != "base before" {
		t.Fatalf("partial replacement reached ROM folder: %q", got)
	}
	owned, ok, err := f.owned.Owned(ctx, 13)
	if err != nil || !ok || owned.Snapshot[0].SizeBytes != int64(len("base before")) {
		t.Fatalf("old ownership was not retained: %+v, ok=%t, err=%v", owned, ok, err)
	}
	f.stagingEmpty(t)
}

func TestSweepRunsAtMostDaily(t *testing.T) {
	f := newDownloadFixture(t, 2)
	ctx := context.Background()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	f.downloads.now = func() time.Time { return now }

	if wait, _ := f.downloads.untilSweep(ctx); wait != 0 {
		t.Fatalf("first sweep waits %v", wait)
	}
	if err := f.downloads.sweep(ctx); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if wait, _ := f.downloads.untilSweep(ctx); wait != 23*time.Hour {
		t.Fatalf("an hour after a sweep, the next waits %v", wait)
	}
	now = now.Add(23 * time.Hour)
	if wait, _ := f.downloads.untilSweep(ctx); wait != 0 {
		t.Fatalf("a day after a sweep, the next waits %v", wait)
	}
}

// chronoWithUpdate installs Chrono Trigger, then gives RomM an update of it: data, named name.
func (f *downloadFixture) chronoWithUpdate(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := f.installedChrono(t)
	f.serve(1, name, data)
	if err := f.downloads.CheckUpdates(context.Background(), []int{1}); err != nil {
		t.Fatal(err)
	}
	if c := f.ownershipChange(t); c.State != domain.StateUpdate {
		t.Fatalf("change %+v", c)
	}
	return path
}

func TestUpdateReplacesTheFiles(t *testing.T) {
	f := newDownloadFixture(t, 2)
	ctx := context.Background()
	revised := []byte("chrono trigger, revised")
	path := f.chronoWithUpdate(t, "Chrono Trigger (USA).sfc", revised)

	if _, err := f.downloads.Queue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 1); u.State != domain.StateInstalled {
		t.Fatalf("end %+v", u)
	}
	if data, _ := os.ReadFile(path); string(data) != string(revised) {
		t.Fatalf("file holds %q", data)
	}
	if _, err := os.Stat(path + newSuffix); !os.IsNotExist(err) {
		t.Fatalf("left the new file beside: %v", err)
	}
	if rec, _, _ := f.owned.Owned(ctx, 1); len(rec.Snapshot) != 1 || rec.Snapshot[0].SizeBytes != int64(len(revised)) {
		t.Fatalf("record %+v", rec)
	}
	if states, _ := f.downloads.States(ctx); states[1] != domain.StateInstalled {
		t.Fatalf("states %v", states)
	}
	f.stagingEmpty(t)
}

func TestUpdateUnderANewNameRemovesTheOldFile(t *testing.T) {
	f := newDownloadFixture(t, 2)
	ctx := context.Background()
	path := f.chronoWithUpdate(t, "Chrono Trigger (USA) (Rev 1).sfc", []byte("rev 1"))
	f.catalog.mu.Lock()
	r := f.catalog.roms[1]
	r.FileName = "Chrono Trigger (USA) (Rev 1).sfc"
	f.catalog.roms[1] = r
	f.catalog.mu.Unlock()

	if _, err := f.downloads.Queue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 1); u.State != domain.StateInstalled {
		t.Fatalf("end %+v", u)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("old file kept: %v", err)
	}
	rec, _, _ := f.owned.Owned(ctx, 1)
	if len(rec.Files) != 1 || filepath.Base(rec.Files[0].Path) != "Chrono Trigger (USA) (Rev 1).sfc" {
		t.Fatalf("record %+v", rec)
	}
}

func TestCancelledUpdateKeepsTheOldVersion(t *testing.T) {
	f := newDownloadFixture(t, 2)
	ctx := context.Background()
	path := f.chronoWithUpdate(t, "Chrono Trigger (USA).sfc", []byte("chrono trigger, revised"))
	f.server.mu.Lock()
	f.server.gates[10] = make(chan struct{})
	f.server.mu.Unlock()

	if _, err := f.downloads.Queue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	f.started(t)
	f.downloads.Cancel(1)
	if u := f.end(t, 1); u.State != domain.StateUpdate {
		t.Fatalf("end %+v", u)
	}
	if data, _ := os.ReadFile(path); string(data) != string(chrono) {
		t.Fatalf("file holds %q", data)
	}
	if states, _ := f.downloads.States(ctx); states[1] != domain.StateUpdate {
		t.Fatalf("states %v", states)
	}
	f.stagingEmpty(t)
}

func TestIgnoredUpdateComesBackOnlyWhenRomMChangesAgain(t *testing.T) {
	f := newDownloadFixture(t, 2)
	ctx := context.Background()
	f.chronoWithUpdate(t, "Chrono Trigger (USA).sfc", []byte("rev 1"))

	if err := f.downloads.IgnoreUpdate(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if c := f.ownershipChange(t); c.Kind != OwnershipUpdate || c.State != domain.StateInstalled {
		t.Fatalf("change %+v", c)
	}
	if err := f.downloads.CheckUpdates(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	if states, _ := f.downloads.States(ctx); states[1] != domain.StateInstalled {
		t.Fatalf("ignored update came back: %v", states)
	}
	if err := f.downloads.IgnoreUpdate(ctx, 1); kindOf(err) != domain.KindInvalidInput {
		t.Fatalf("ignoring with no update: %v", err)
	}

	f.serve(1, "Chrono Trigger (USA).sfc", []byte("rev 2, longer"))
	if err := f.downloads.CheckUpdates(ctx, []int{1}); err != nil {
		t.Fatal(err)
	}
	if states, _ := f.downloads.States(ctx); states[1] != domain.StateUpdate {
		t.Fatalf("a further change didn't show: %v", states)
	}
}
