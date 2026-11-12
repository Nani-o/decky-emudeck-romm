package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

const mkBase = "Mario Kart [0100152000022000][v0].nsp"
const mkOld = "Mario Kart - Update 4.0.0 [0100152000022800][v1441792].nsp"
const mkLatest = "Mario Kart - Update 4.0.1 [0100152000022800][v1507328].nsp"

func TestSwitchLatestUpdateQueueSizesAndProcessingPhases(t *testing.T) {
	f := switchFixture(t)
	archiveName := "Mario Kart - Update 4.0.1 [0100152000022800][v1507328].zip"
	archive := string(zipOf(t, member{"patch.nsp", "latest bytes"}))
	f.serveSet(13, gameFile{mkBase, "game", "base bytes"}, gameFile{"update/" + mkOld, "update", "historical bytes"}, gameFile{"update/" + archiveName, "update", archive}, gameFile{"dlc/Wave.nsp", "dlc", "dlc"})
	events := make(chan Download, 64)
	listener := f.downloads.onChange
	f.downloads.onChange = func(d Download) { events <- d; listener(d) }
	queued, err := f.downloads.Queue(context.Background(), 13)
	wantBytes := int64(len("base bytes") + len(archive) + len("dlc"))
	if err != nil || queued.Total != wantBytes || queued.FileCount != 3 {
		t.Fatalf("queued %+v err=%v wantbytes=%d", queued, err, wantBytes)
	}
	ended := f.end(t, 13)
	if ended.State != domain.StateInstalled || ended.Total != wantBytes || ended.Received != wantBytes {
		t.Fatalf("ended %+v", ended)
	}
	var downloaded []int
	for len(f.server.started) > 0 {
		downloaded = append(downloaded, <-f.server.started)
	}
	if !slices.Equal(downloaded, []int{1300, 1302, 1303}) {
		t.Fatalf("downloaded historical patch: %v", downloaded)
	}
	var phases []string
	for len(events) > 0 {
		event := <-events
		if event.Phase != "" && (len(phases) == 0 || phases[len(phases)-1] != event.Phase) {
			phases = append(phases, event.Phase)
		}
	}
	if !slices.Equal(phases, []string{"download", "extract", "install"}) {
		t.Fatalf("processing phases %v", phases)
	}
}

func TestSwitchRetiresHistoricalOwnedPatchWithoutDownloadingAnything(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13, gameFile{mkBase, "game", "base bytes"}, gameFile{"update/" + mkLatest, "update", "latest bytes"}, gameFile{"dlc/Wave.nsp", "dlc", "dlc"})
	rec := f.installed(t, 13)
	basePath := ""
	for _, file := range rec.Files {
		if file.SourceFileID == 1300 {
			basePath = file.Path
		}
	}
	if basePath == "" {
		t.Fatal("missing owned base")
	}
	root := filepath.Dir(basePath)
	oldPath := filepath.Join(root, "update", mkOld)
	if err := os.WriteFile(oldPath, []byte("historical bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a record made by the previous client that installed every patch.
	oldFile := domain.ROMFile{ID: 1303, Name: mkOld, Path: "update/" + mkOld, Category: "update", SizeBytes: info.Size()}
	rec.Files = append(rec.Files, domain.PlacedFile{Path: oldPath, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), SHA1: sha1Hex("historical bytes"), SourceFileID: 1303})
	rec.Snapshot = append(rec.Snapshot, domain.SnapshotOf(oldFile))
	if err := f.owned.SaveOwned(ctx, rec); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(root, "update", "user-managed.nsp")
	if err := os.WriteFile(userFile, []byte("user bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.server.mu.Lock()
	f.server.files[13] = append(f.server.files[13], oldFile)
	f.server.content[1303] = []byte("historical bytes")
	f.server.mu.Unlock()
	for len(f.server.started) > 0 {
		<-f.server.started
	}
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if change := f.ownershipChange(t); change.State != domain.StateUpdate {
		t.Fatalf("change %+v", change)
	}
	queued, err := f.downloads.Queue(ctx, 13)
	if err != nil || queued.Total != 0 || queued.FileCount != 0 {
		t.Fatalf("remove-only queue %+v err=%v", queued, err)
	}
	if ended := f.end(t, 13); ended.State != domain.StateInstalled || ended.Problem != nil || ended.Total != 0 {
		t.Fatalf("remove-only failed %+v", ended)
	}
	if len(f.server.started) != 0 {
		t.Fatal("remove-only operation downloaded existing content")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old patch remains: %v", err)
	}
	if readFile(t, basePath) != "base bytes" || readFile(t, userFile) != "user bytes" || readFile(t, filepath.Join(root, "update", mkLatest)) != "latest bytes" {
		t.Fatal("unchanged or user-managed content was modified")
	}
	after, _, _ := f.owned.Owned(ctx, 13)
	if domain.ContentUpdateAvailable(after.Snapshot, f.server.files[13]) {
		t.Fatal("cleanup leaves a perpetual update")
	}
}

func TestSwitchUpdateMultipartArchiveDownloadsAllLatestVolumesOnce(t *testing.T) {
	f := switchFixture(t)
	all := []byte("synthetic patch payload")
	a, b := all[:8], all[8:]
	stem := "Mario Kart - Update 4.0.1 [0100152000022800][v1507328]"
	f.serveSet(13, gameFile{mkBase, "game", "base"}, gameFile{"update/" + mkOld, "update", "old"}, gameFile{"update/" + stem + ".part2.rar", "update", string(storedRAR("patch.nsp", all, b, 2, 2, 0))}, gameFile{"update/" + stem + ".part1.rar", "update", string(storedRAR("patch.nsp", all, a, 1, 2, 0))})
	rec := f.installed(t, 13)
	if len(rec.Snapshot) != 3 {
		t.Fatalf("snapshot did not retain both volumes: %+v", rec.Snapshot)
	}
	var patchPath string
	for _, file := range rec.Files {
		if filepath.Base(file.Path) == "patch.nsp" {
			patchPath = file.Path
		}
	}
	if patchPath == "" || readFile(t, patchPath) != string(all) {
		t.Fatal("latest multipart patch was not extracted")
	}
	var downloaded []int
	for len(f.server.started) > 0 {
		downloaded = append(downloaded, <-f.server.started)
	}
	if !slices.Equal(downloaded, []int{1300, 1302, 1303}) {
		t.Fatalf("unexpected multipart transfers: %v", downloaded)
	}
}

func TestSwitchCancellationAtExtractionNeverPublishesDownloadedContent(t *testing.T) {
	f := switchFixture(t)
	f.serveSet(13, gameFile{mkBase, "game", "base"}, gameFile{"update/Patch.zip", "update", string(zipOf(t, member{"patch.nsp", "patch"}))})
	listener := f.downloads.onChange
	f.downloads.onChange = func(d Download) {
		if d.Phase == "extract" && d.State == domain.StateDownloading {
			f.downloads.Cancel(d.ROMID)
		}
		listener(d)
	}
	if _, err := f.downloads.Queue(context.Background(), 13); err != nil {
		t.Fatal(err)
	}
	if ended := f.end(t, 13); ended.State != domain.StateOnRomM {
		t.Fatalf("cancelled extraction %+v", ended)
	}
	if entries := listDir(t, filepath.Join(f.root, "roms", "switch")); len(entries) != 0 {
		t.Fatalf("cancelled content published: %v", entries)
	}
}

func TestSwitchChangedUpdateVolumeReplacesWholeCohortWithoutBaseTransfer(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	oldAll, newAll := []byte("sameheadold-tail"), []byte("sameheadNEW-tail")
	stem := "Mario Kart - Update 4.0.1 [0100152000022800][v1507328]"
	first := string(storedRAR("patch.nsp", oldAll, oldAll[:8], 1, 2, 0))
	oldSecond := string(storedRAR("patch.nsp", oldAll, oldAll[8:], 2, 2, 0))
	newSecond := string(storedRAR("patch.nsp", newAll, newAll[8:], 2, 2, 0))
	if first != string(storedRAR("patch.nsp", newAll, newAll[:8], 1, 2, 0)) {
		t.Fatal("fixture unexpectedly changed first volume")
	}
	f.serveSet(13, gameFile{mkBase, "game", "base"}, gameFile{"update/" + stem + ".part1.rar", "update", first}, gameFile{"update/" + stem + ".part2.rar", "update", oldSecond})
	rec := f.installed(t, 13)
	var patchPath string
	for _, file := range rec.Files {
		if filepath.Base(file.Path) == "patch.nsp" {
			patchPath = file.Path
		}
	}
	for len(f.server.started) > 0 {
		<-f.server.started
	}
	f.server.mu.Lock()
	f.server.content[1302] = []byte(newSecond)
	f.server.files[13][2].LastModified = f.server.files[13][2].LastModified.Add(1)
	f.server.mu.Unlock()
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if change := f.ownershipChange(t); change.State != domain.StateUpdate {
		t.Fatalf("change %+v", change)
	}
	queued, err := f.downloads.Queue(ctx, 13)
	if err != nil || queued.FileCount != 2 || queued.Total != int64(len(first)+len(newSecond)) {
		t.Fatalf("incomplete cohort queue %+v err=%v", queued, err)
	}
	if ended := f.end(t, 13); ended.State != domain.StateInstalled || ended.Problem != nil {
		t.Fatalf("changed volume failed %+v", ended)
	}
	if readFile(t, patchPath) != string(newAll) {
		t.Fatal("updated cohort was not extracted")
	}
	var downloaded []int
	for len(f.server.started) > 0 {
		downloaded = append(downloaded, <-f.server.started)
	}
	if !slices.Equal(downloaded, []int{1301, 1302}) {
		t.Fatalf("base or incomplete cohort downloaded %v", downloaded)
	}
}
