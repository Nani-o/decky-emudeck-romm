package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

func planFilesystem(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s/%d/%d", info.Mode(), info.Size(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += "/" + sha1Hex(string(data))
		}
		out[rel] = value
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertPreviewReadOnly(t *testing.T, f *downloadFixture, id int) (DownloadPlan, error) {
	t.Helper()
	ctx := context.Background()
	beforeFiles := planFilesystem(t, f.root)
	beforeQueue, _ := f.owned.Queued(ctx)
	beforeIDs, _ := f.owned.OwnedIDs(ctx)
	beforeOwned, _ := json.Marshal(f.owned.roms)
	beforeDownloads := f.downloads.List()
	previousFree := f.downloads.free
	f.downloads.free = func(string) (int64, error) {
		t.Error("preview called free-space reservation/hold logic")
		return 0, nil
	}
	plan, err := f.downloads.Plan(ctx, id)
	f.downloads.free = previousFree
	afterQueue, _ := f.owned.Queued(ctx)
	afterIDs, _ := f.owned.OwnedIDs(ctx)
	afterOwned, _ := json.Marshal(f.owned.roms)
	if !reflect.DeepEqual(beforeQueue, afterQueue) || !reflect.DeepEqual(beforeIDs, afterIDs) || string(beforeOwned) != string(afterOwned) || !reflect.DeepEqual(beforeDownloads, f.downloads.List()) {
		t.Fatal("preview changed download or ownership records")
	}
	if !reflect.DeepEqual(beforeFiles, planFilesystem(t, f.root)) {
		t.Fatal("preview created or changed a local file or directory")
	}
	return plan, err
}

func TestDownloadPlanSwitchMatchesQueueSelectedSources(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13, gameFile{mkBase, "game", "base"}, gameFile{"update/" + mkOld, "update", "historical"}, gameFile{"update/" + mkLatest, "update", "latest"}, gameFile{"dlc/Wave.nsp", "dlc", "dlc"})
	f.free.Store(0) // Preview remains useful when no download could presently fit.
	plan, err := assertPreviewReadOnly(t, f, 13)
	if err != nil || plan.FileCount != 3 || plan.TotalBytes != int64(len("base")+len("latest")+len("dlc")) {
		t.Fatalf("preview %+v err=%v", plan, err)
	}
	if len(f.server.started) != 0 {
		t.Fatal("preview started a content transfer")
	}
	f.free.Store(1 << 40)
	queued, err := f.downloads.Queue(ctx, 13)
	if err != nil || queued.FileCount != plan.FileCount || queued.Total != plan.TotalBytes {
		t.Fatalf("queue differs from preview: %+v, %+v, %v", plan, queued, err)
	}
	if ended := f.end(t, 13); ended.State != domain.StateInstalled {
		t.Fatalf("download %+v", ended)
	}
}

func TestDownloadPlanRejectsAmbiguousPackagesAndUnsafePaths(t *testing.T) {
	for _, files := range [][]gameFile{
		{{mkBase, "game", "base"}, {"update/unknown-one.nsp", "update", "one"}, {"update/unknown-two.nsp", "update", "two"}},
		{{mkBase, "game", "base"}, {"../escape.nsp", "dlc", "bad path"}},
	} {
		f := switchFixture(t)
		f.serveSet(13, files...)
		if _, err := assertPreviewReadOnly(t, f, 13); kindOf(err) != domain.KindCantDownload {
			t.Fatalf("unsafe preview accepted: %v", err)
		}
		if len(f.server.started) != 0 {
			t.Fatal("unsafe preview downloaded content")
		}
	}
}

func TestDownloadPlanReturnsConnectionFailuresWithoutChangingSignIn(t *testing.T) {
	for _, kind := range []domain.Kind{domain.KindSignedOut, domain.KindUnreachable} {
		f := switchFixture(t)
		f.serveSet(13, gameFile{mkBase, "game", "base"})
		in, _ := f.settings.SignIn()
		before := *in
		f.server.err = &domain.Error{Kind: kind, Message: "fixture server failure"}
		if _, err := assertPreviewReadOnly(t, f, 13); kindOf(err) != kind {
			t.Fatalf("failure %s became %v", kind, err)
		}
		after, _ := f.settings.SignIn()
		if !reflect.DeepEqual(before, *after) {
			t.Fatal("preview modified native sign-in")
		}
	}
}

func TestDownloadPlanOtherPlatformCountsActualGameFiles(t *testing.T) {
	f := newDownloadFixture(t, 1)
	plan, err := assertPreviewReadOnly(t, f, 1)
	if err != nil || plan.FileCount != 1 || plan.TotalBytes != int64(len(chrono)) {
		t.Fatalf("preview %+v err=%v", plan, err)
	}
}

func TestDownloadPlanBaseRARVolumeChangeMatchesActualCohort(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	oldAll, newAll := []byte("sameheadold-tail"), []byte("sameheadNEW-tail")
	first := string(storedRAR("Mario Kart.nsp", oldAll, oldAll[:8], 1, 2, 0))
	oldSecond := string(storedRAR("Mario Kart.nsp", oldAll, oldAll[8:], 2, 2, 0))
	newSecond := string(storedRAR("Mario Kart.nsp", newAll, newAll[8:], 2, 2, 0))
	f.serveSet(13, gameFile{"Mario Kart.part1.rar", "game", first}, gameFile{"Mario Kart.part2.rar", "game", oldSecond}, gameFile{"dlc/Wave.nsp", "dlc", "dlc"})
	rec := f.installed(t, 13)
	var basePath string
	for _, file := range rec.Files {
		if filepath.Base(file.Path) == "Mario Kart.nsp" {
			basePath = file.Path
		}
	}
	for len(f.server.started) > 0 {
		<-f.server.started
	}
	f.server.mu.Lock()
	f.server.content[1301] = []byte(newSecond)
	f.server.files[13][1].LastModified = f.server.files[13][1].LastModified.Add(1)
	f.server.mu.Unlock()
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if change := f.ownershipChange(t); change.State != domain.StateUpdate {
		t.Fatalf("change %+v", change)
	}
	plan, err := assertPreviewReadOnly(t, f, 13)
	if err != nil || plan.FileCount != 2 || plan.TotalBytes != int64(len(first)+len(newSecond)) {
		t.Fatalf("base volume preview %+v err=%v", plan, err)
	}
	queued, err := f.downloads.Queue(ctx, 13)
	if err != nil || queued.FileCount != plan.FileCount || queued.Total != plan.TotalBytes {
		t.Fatalf("base volume queue %+v err=%v", queued, err)
	}
	if ended := f.end(t, 13); ended.State != domain.StateInstalled || ended.Total != plan.TotalBytes || ended.FileCount != plan.FileCount {
		t.Fatalf("base volume actual %+v", ended)
	}
	if readFile(t, basePath) != string(newAll) {
		t.Fatal("base RAR update was not extracted")
	}
	var ids []int
	for len(f.server.started) > 0 {
		ids = append(ids, <-f.server.started)
	}
	if !reflect.DeepEqual(ids, []int{1300, 1301}) {
		t.Fatalf("wrong cohort or unrelated DLC transfer %v", ids)
	}
}

func TestDownloadPlanLegacyBaseChangeCountsWholeReplacement(t *testing.T) {
	f := switchFixture(t)
	ctx := context.Background()
	f.serveSet(13, gameFile{mkBase, "game", "base"}, gameFile{"README.txt", "game", "readme"}, gameFile{"dlc/Wave.nsp", "dlc", "dlc"})
	rec := f.installed(t, 13)
	for i, file := range rec.Files {
		if file.SourceFileID == 1300 || file.SourceFileID == 1301 {
			rec.Files[i].SourceFileID = 0
		}
	}
	if err := f.owned.SaveOwned(ctx, rec); err != nil {
		t.Fatal(err)
	}
	for len(f.server.started) > 0 {
		<-f.server.started
	}
	f.server.mu.Lock()
	f.server.content[1300] = []byte("BASE")
	f.server.files[13][0].LastModified = f.server.files[13][0].LastModified.Add(1)
	f.server.mu.Unlock()
	if err := f.downloads.CheckUpdates(ctx, []int{13}); err != nil {
		t.Fatal(err)
	}
	if change := f.ownershipChange(t); change.State != domain.StateUpdate {
		t.Fatalf("change %+v", change)
	}
	plan, err := assertPreviewReadOnly(t, f, 13)
	if err != nil || plan.FileCount != 2 || plan.TotalBytes != int64(len("BASE")+len("readme")) {
		t.Fatalf("legacy base preview %+v err=%v", plan, err)
	}
	queued, err := f.downloads.Queue(ctx, 13)
	if err != nil || queued.FileCount != plan.FileCount || queued.Total != plan.TotalBytes {
		t.Fatalf("legacy base queue %+v err=%v", queued, err)
	}
	if ended := f.end(t, 13); ended.State != domain.StateInstalled || ended.Total != plan.TotalBytes || ended.FileCount != plan.FileCount {
		t.Fatalf("legacy base actual %+v", ended)
	}
	var ids []int
	for len(f.server.started) > 0 {
		ids = append(ids, <-f.server.started)
	}
	if !reflect.DeepEqual(ids, []int{1300, 1301}) {
		t.Fatalf("wrong legacy replacement or unrelated DLC transfer %v", ids)
	}
}
