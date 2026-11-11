package app

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

const testEdenProfile = "1D0041E0819ED519E310711A99E58874"
const testEdenTitle = "0100EA80032EA000"

func fixtureEden(t *testing.T, home, server, device string) (*EdenSaves, string) {
	t.Helper()
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, "homebrew/settings/romm")
	nand := filepath.Join(home, "Emulation/storage/eden/nand")
	root := filepath.Join(nand, "user/save/0000000000000000", testEdenProfile, testEdenTitle)
	for _, p := range []string{settings, filepath.Dir(root), filepath.Join(home, ".config/eden")} {
		if err = os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	b := edenBinding{Schema: 1, Owner: "homelab-romm-eden-v1", ServerURL: server, DeviceID: device, NANDPath: nand, ProfileID: testEdenProfile, Games: map[string]string{"319": testEdenTitle}}
	data, _ := json.Marshal(b)
	if err = os.WriteFile(filepath.Join(settings, "eden-saves.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	ini := "[Data%20Storage]\nnand_directory=" + nand + "\nnand_directory\\default=false\n[System]\ncurrent_user=0\ncurrent_user\\default=true\n[Other]\nignored=one\nignored=two\n"
	if err = os.WriteFile(filepath.Join(home, ".config/eden/qt-config.ini"), []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	return NewEdenSaves(home, settings), root
}

func edenZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for name, data := range entries {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, "/") {
			continue
		}
		if _, err = w.Write([]byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestEdenBindingRequiresExactServerDeviceAndMapping(t *testing.T) {
	e, root := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
	got, title, err := e.target("https://romm.nani", "deck-1", 319)
	if err != nil || got != root || title != testEdenTitle {
		t.Fatalf("target %q %q %v", got, title, err)
	}
	for _, c := range []struct {
		server, device string
		id             int
	}{{"https://elsewhere.nani", "deck-1", 319}, {"https://romm.nani", "deck-2", 319}, {"https://romm.nani", "deck-1", 320}} {
		if _, _, err = e.target(c.server, c.device, c.id); err == nil {
			t.Fatalf("unbound target accepted %+v", c)
		}
	}
}

func TestEdenBindingRejectsForeignOwnerDuplicatesUnknownKeysAndPermissions(t *testing.T) {
	for _, kind := range []string{"owner", "duplicate", "unknown", "profile", "title", "duplicate-title", "world-readable", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
			p := filepath.Join(e.settings, "eden-saves.json")
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "owner":
				data = bytes.ReplaceAll(data, []byte("homelab-romm-eden-v1"), []byte("foreign"))
			case "duplicate":
				data = append([]byte(`{"schema":1,`), data[1:]...)
			case "unknown":
				data = append([]byte(`{"extra":true,`), data[1:]...)
			case "profile":
				data = bytes.ReplaceAll(data, []byte(testEdenProfile), []byte(strings.Repeat("Z", 32)))
			case "title":
				data = bytes.ReplaceAll(data, []byte(testEdenTitle), []byte("0100EA80032EA800"))
			case "duplicate-title":
				data = bytes.ReplaceAll(data, []byte(`"319":"`+testEdenTitle+`"`), []byte(`"319":"`+testEdenTitle+`","320":"`+testEdenTitle+`"`))
			}
			if err = os.WriteFile(p, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if kind == "world-readable" {
				if err = os.Chmod(p, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "symlink" {
				if err = os.Rename(p, p+".original"); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(p+".original", p); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = e.binding(); err == nil {
				t.Fatal("unsafe binding accepted")
			}
		})
	}
}

func TestEdenBindingQSettingsMustIdentifyActualExplicitNAND(t *testing.T) {
	e, _ := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
	b, err := e.binding()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"[Other]\nnand_directory=" + b.NANDPath, "[Data%20Storage]\nnand_directory=" + b.NANDPath + "\nnand_directory\\default=true", "[Data%20Storage]\nnand_directory=" + b.NANDPath + "\nnand_directory=" + b.NANDPath + "\nnand_directory\\default=false", "[Data%20Storage]\nnand_directory=" + b.NANDPath + "\nnand_directory\\default=false\n[Data Storage]\n"} {
		if err = qualifyEdenNAND([]byte(s), b.NANDPath); err == nil {
			t.Fatalf("unqualified INI accepted %q", s)
		}
	}
}

func TestEdenFolderSnapshotStableAndOnlySelectedTitle(t *testing.T) {
	e, root := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
	writeFile(t, filepath.Join(root, "rp_savedata.dat"), []byte("world 1"))
	writeFile(t, filepath.Join(root, "nested/settings.bin"), []byte("settings"))
	other := filepath.Join(filepath.Dir(root), "0100000000000000", "other.dat")
	writeFile(t, other, []byte("other game"))
	f := edenFolder{root: root, home: e.home, guard: func() error { return nil }}
	a, err := f.read()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour)
	if err = os.Chtimes(filepath.Join(root, "rp_savedata.dat"), now, now); err != nil {
		t.Fatal(err)
	}
	b, err := f.read()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.data, b.data) || a.hash != b.hash {
		t.Fatal("ZIP changed from timestamps alone")
	}
	entries, err := parseEdenArchive(a.data)
	if err != nil || len(entries) != 2 || entries[0].name != "nested/settings.bin" {
		t.Fatalf("entries %+v %v", entries, err)
	}
	if readFile(t, other) != "other game" {
		t.Fatal("another title changed")
	}
}

func TestEdenFolderRefusesSymlinkHardlinkWritableAndLargeSources(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "writable", "large"} {
		t.Run(kind, func(t *testing.T) {
			e, root := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
			p := filepath.Join(root, "save.dat")
			writeFile(t, p, []byte("save"))
			switch kind {
			case "symlink":
				os.Remove(p)
				if err := os.Symlink(filepath.Join(e.home, "outside"), p); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(p, filepath.Join(e.home, "other-link")); err != nil {
					t.Fatal(err)
				}
			case "writable":
				if err := os.Chmod(p, 0o666); err != nil {
					t.Fatal(err)
				}
			case "large":
				if err := os.Truncate(p, edenMaxBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			f := edenFolder{root: root, home: e.home, guard: func() error { return nil }}
			if _, err := f.read(); err == nil {
				t.Fatal("unsafe source accepted")
			}
		})
	}
}

func TestEdenSnapshotRejectsChangesBeforeCompleting(t *testing.T) {
	e, root := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
	p := filepath.Join(root, "save.dat")
	writeFile(t, p, []byte("old"))
	calls := 0
	f := edenFolder{root: root, home: e.home, guard: func() error {
		calls++
		if calls == 2 {
			return errors.New("binding changed")
		}
		return nil
	}}
	if _, err := f.read(); err == nil {
		t.Fatal("changed binding accepted")
	}
}

func TestEdenArchiveRejectsUnsafeNamesAndTypesBeforeAnyWrite(t *testing.T) {
	for _, name := range []string{"../escape", "/absolute", `C:\\escape`, "folder/../escape", "folder//file", "folder/", "line\nbreak"} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseEdenArchive(edenZip(t, map[string]string{name: "payload"})); err == nil {
				t.Fatal("unsafe name accepted")
			}
		})
	}
	for _, kind := range []string{"duplicate", "symlink", "collision", "crc"} {
		t.Run(kind, func(t *testing.T) {
			var b bytes.Buffer
			z := zip.NewWriter(&b)
			if kind == "symlink" {
				h := &zip.FileHeader{Name: "link"}
				h.SetMode(os.ModeSymlink | 0o777)
				w, _ := z.CreateHeader(h)
				w.Write([]byte("outside"))
			} else {
				w, _ := z.CreateHeader(&zip.FileHeader{Name: "save", Method: zip.Store})
				w.Write([]byte("PAYLOAD"))
				if kind == "duplicate" {
					w, _ = z.Create("save")
					w.Write([]byte("again"))
				}
				if kind == "collision" {
					w, _ = z.Create("save/child")
					w.Write([]byte("child"))
				}
			}
			z.Close()
			data := b.Bytes()
			if kind == "crc" {
				i := bytes.Index(data, []byte("PAYLOAD"))
				if i < 0 {
					t.Fatal("fixture")
				}
				data[i] = 'X'
			}
			if _, err := parseEdenArchive(data); err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
		})
	}
}

func TestEdenRestoreReplacesOnlyTitleAndPreservesPreviousTree(t *testing.T) {
	e, root := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
	writeFile(t, filepath.Join(root, "old.dat"), []byte("original"))
	other := filepath.Join(filepath.Dir(root), "0100000000000000", "other.dat")
	writeFile(t, other, []byte("untouched"))
	f := edenFolder{root: root, home: e.home, guard: func() error { return nil }}
	if err := f.write(edenZip(t, map[string]string{"rp_savedata.dat": "restored"})); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(root, "rp_savedata.dat")) != "restored" {
		t.Fatal("restore missing")
	}
	gone(t, filepath.Join(root, "old.dat"))
	if readFile(t, other) != "untouched" {
		t.Fatal("other title changed")
	}
	ops, err := filepath.Glob(filepath.Join(filepath.Dir(root), ".romm-eden-restore-*"))
	if err != nil || len(ops) != 1 {
		t.Fatal(ops, err)
	}
	if readFile(t, filepath.Join(ops[0], "previous/old.dat")) != "original" {
		t.Fatal("previous tree missing")
	}
	if err = edenRecoveryReady(e.home, root); err != nil {
		t.Fatal(err)
	}
}

func TestEdenInterruptedRestoreKeepsBothTreesAndBlocksRetry(t *testing.T) {
	e, root := fixtureEden(t, t.TempDir(), "https://romm.nani", "deck-1")
	writeFile(t, filepath.Join(root, "old.dat"), []byte("original"))
	calls := 0
	f := edenFolder{root: root, home: e.home, guard: func() error { return nil }, rename: func(a, b string) error {
		calls++
		if calls == 2 {
			return errors.New("injected rename failure")
		}
		return os.Rename(a, b)
	}}
	if err := f.write(edenZip(t, map[string]string{"new.dat": "remote"})); err == nil {
		t.Fatal("injected failure hidden")
	}
	gone(t, root)
	ops, _ := filepath.Glob(filepath.Join(filepath.Dir(root), ".romm-eden-restore-*"))
	if len(ops) != 1 {
		t.Fatal(ops)
	}
	if readFile(t, filepath.Join(ops[0], "previous/old.dat")) != "original" || readFile(t, filepath.Join(ops[0], "staged/new.dat")) != "remote" {
		t.Fatal("recovery bytes lost")
	}
	if err := f.write(edenZip(t, map[string]string{"later.dat": "must not write"})); err == nil {
		t.Fatal("incomplete restore retry was allowed")
	}
	gone(t, root)
}

func (f *saveFixture) edenGame(t *testing.T) string {
	t.Helper()
	e, root := fixtureEden(t, f.root, "https://romm.lan", "deck-1")
	f.saves.eden = e
	state := f.saves.install.State(false)
	state.Systems = append(state.Systems, domain.SystemState{Folder: "switch", ROMPath: filepath.Join(f.root, "roms/switch"), Emulator: "eden"})
	f.saves.install = NewInstall(fixedProber{state}, f.saves.log)
	f.launchers["switch"] = domain.GameLauncher{ESDECommand: domain.ESDECommand{Label: "Eden (Standalone)", Command: "%INJECT%=%BASENAME%.esprefix %EMULATOR_EDEN% -f -g %ROM%"}, Known: true}
	f.catalog.mu.Lock()
	f.catalog.platforms = append(f.catalog.platforms, domain.Platform{ID: 319, Slug: "switch", Name: "Switch"})
	f.catalog.roms[319] = domain.ROM{ID: 319, PlatformID: 319, Name: "Mario", FileName: "Mario.nsp"}
	f.catalog.mu.Unlock()
	rom := filepath.Join(f.root, "roms/switch/Mario.nsp")
	writeFile(t, rom, []byte("ROM metadata fixture"))
	if err := f.owned.SaveLinked(context.Background(), domain.LinkedGame{ROMID: 319, PlatformID: 319, Path: rom, LinkedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestEdenFirstNativeSyncUsesDedicatedSlotAndFilter(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	writeFile(t, filepath.Join(root, "rp_savedata.dat"), []byte("world 2"))
	st := f.statusOf(t, 319)
	if !st.Syncs || st.Slot != EdenSlot || st.Emulator != "eden" || filepath.Base(st.Path) != testEdenTitle+".zip" {
		t.Fatalf("status %+v", st)
	}
	s := f.syncGame(t, 319)
	if s.Uploaded != 1 {
		t.Fatalf("summary %+v", s)
	}
	if len(f.romm.versions) != 1 || f.romm.versions[0].Slot != EdenSlot || f.romm.versions[0].Emulator != "eden" {
		t.Fatalf("versions %+v", f.romm.versions)
	}
	if r := f.romm.requests[0]; len(r.Emulators) != 1 || r.Emulators[0] != "eden" || len(r.ROMIDs) != 1 || r.ROMIDs[0] != 319 {
		t.Fatalf("request %+v", r)
	}
	if err := f.saves.SwitchSlot(context.Background(), 319, domain.AutosaveSlot); err == nil {
		t.Fatal("Eden switched into autosave")
	}
}

func TestEdenOwnedSwitchWithBaseUpdateAndDLCUsesOneMappedSaveTarget(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	gameRoot := filepath.Join(f.root, "roms/switch/Mario.nsp")
	if err := os.Remove(gameRoot); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(gameRoot, "Mario.nsp")
	update := filepath.Join(gameRoot, "update/Update.nsp")
	updateNoLoad := filepath.Join(gameRoot, "update/noload.txt")
	dlc := filepath.Join(gameRoot, "dlc/DLC.nsp")
	dlcNoLoad := filepath.Join(gameRoot, "dlc/noload.txt")
	for _, file := range []string{base, update, updateNoLoad, dlc, dlcNoLoad} {
		writeFile(t, file, []byte(filepath.Base(file)))
	}
	if err := f.owned.SaveOwned(context.Background(), domain.OwnedROM{
		ROMID: 319, PlatformID: 319, Files: []domain.PlacedFile{{Path: base}, {Path: update}, {Path: updateNoLoad}, {Path: dlc}, {Path: dlcNoLoad}},
	}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "rp_savedata.dat"), []byte("switch progress"))

	status := f.statusOf(t, 319)
	if !status.Syncs || status.Emulator != "eden" || status.Slot != EdenSlot || filepath.Base(status.Path) != testEdenTitle+".zip" {
		t.Fatalf("owned base/update/DLC entry lost its mapped Eden target: %+v", status)
	}
	if summary := f.syncGame(t, 319); summary.Uploaded != 1 || summary.Failed != 0 {
		t.Fatalf("sync %+v", summary)
	}
	if len(f.romm.versions) != 1 || f.romm.versions[0].ROMID != 319 || f.romm.versions[0].Slot != EdenSlot || f.romm.versions[0].Emulator != "eden" {
		t.Fatalf("versions %+v", f.romm.versions)
	}
}

func TestEdenBusySkipsEveryUploadAndDownload(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	writeFile(t, filepath.Join(root, "rp_savedata.dat"), []byte("local"))
	f.playing.Store("eden")
	s := f.syncGame(t, 319)
	if s.Waiting != 1 || s.Uploaded != 0 || s.Downloaded != 0 || len(f.romm.requests) != 0 {
		t.Fatalf("summary %+v requests %+v", s, f.romm.requests)
	}
	f.playing.Store("")
	if s = f.syncGame(t, 319); s.Uploaded != 1 {
		t.Fatalf("after close %+v", s)
	}
}

func TestEdenUnlinkedGameRemainsUnsupportedAndNoSaveRead(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	if err := f.downloads.Unlink(context.Background(), 319); err != nil {
		t.Fatal(err)
	}
	if st := f.statusOf(t, 319); st.Syncs || !strings.Contains(st.Why, "linked") {
		t.Fatalf("status %+v", st)
	}
	gone(t, root)
}

func TestEdenRemoteFolderRestoresAndNativeBackupIsReadable(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	file := filepath.Join(root, "rp_savedata.dat")
	writeFile(t, file, []byte("local initial"))
	if s := f.syncGame(t, 319); s.Uploaded != 1 {
		t.Fatalf("initial %+v", s)
	}
	f.romm.otherDeviceIn(319, EdenSlot, "eden", edenZip(t, map[string]string{"rp_savedata.dat": "remote progress", "settings/one": "remote setting"}))
	if s := f.syncGame(t, 319); s.Downloaded != 1 || s.Failed != 0 {
		t.Fatalf("pull %+v", s)
	}
	if readFile(t, file) != "remote progress" {
		t.Fatal("not restored")
	}
	backups, err := f.saves.Backups(319)
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups %+v %v", backups, err)
	}
	data, err := os.ReadFile(filepath.Join(f.saves.backupDir(319), backups[0].Name))
	if err != nil {
		t.Fatal(err)
	}
	es, err := parseEdenArchive(data)
	if err != nil || string(es[0].data) != "local initial" {
		t.Fatalf("backup %+v %v", es, err)
	}
	if s := f.syncGame(t, 319); s.Uploaded != 0 || s.Downloaded != 0 || s.Conflicts != 0 {
		t.Fatalf("repeat %+v", s)
	}
}

func TestEdenConflictNeverOverwritesEitherCopy(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	file := filepath.Join(root, "rp_savedata.dat")
	writeFile(t, file, []byte("initial"))
	f.syncGame(t, 319)
	remote := f.romm.otherDeviceIn(319, EdenSlot, "eden", edenZip(t, map[string]string{"rp_savedata.dat": "remote branch"}))
	writeFile(t, file, []byte("local branch"))
	if s := f.syncGame(t, 319); s.Conflicts != 1 || s.Downloaded != 0 {
		t.Fatalf("conflict %+v", s)
	}
	if readFile(t, file) != "local branch" {
		t.Fatal("local branch overwritten")
	}
	var head fakeVersion
	for _, v := range f.romm.versions {
		if v.ROMID == 319 && v.Slot == EdenSlot && v.ID > head.ID {
			head = v
		}
	}
	if head.ID != remote.ID {
		t.Fatal("server branch overwritten")
	}
	if st := f.statusOf(t, 319); st.State != domain.SaveConflict || st.Conflict == nil || !st.Conflict.Parked {
		t.Fatalf("status %+v", st)
	}
}

func TestEdenMixedSyncKeepsLegacyAutosaveAndNegotiationsSeparate(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	writeFile(t, filepath.Join(root, "rp_savedata.dat"), []byte("switch progress"))
	writeFile(t, filepath.Join(f.raSaves, "Chrono Trigger (USA).srm"), []byte("snes progress"))
	if s, err := f.saves.Sync(context.Background(), []int{319, 1}); err != nil || s.Uploaded != 2 {
		t.Fatalf("sync %+v %v", s, err)
	}
	if len(f.romm.requests) != 2 {
		t.Fatalf("requests %+v", f.romm.requests)
	}
	for _, r := range f.romm.requests {
		for _, s := range r.Saves {
			if s.ROMID == 319 {
				if s.Slot != EdenSlot || len(r.Emulators) != 1 || r.Emulators[0] != "eden" {
					t.Fatalf("Eden %+v", r)
				}
			} else if s.Slot != domain.AutosaveSlot || r.Emulators != nil {
				t.Fatalf("legacy %+v", r)
			}
		}
	}
}

func TestEdenWrongEmulatorServerArchiveCannotReplaceTitle(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	file := filepath.Join(root, "rp_savedata.dat")
	writeFile(t, file, []byte("local"))
	f.syncGame(t, 319)
	f.romm.otherDeviceIn(319, EdenSlot, "some-other-emulator", edenZip(t, map[string]string{"rp_savedata.dat": "incompatible"}))
	if s := f.syncGame(t, 319); s.Downloaded != 0 || s.Failed != 1 {
		t.Fatalf("sync %+v", s)
	}
	if readFile(t, file) != "local" {
		t.Fatal("wrong emulator replaced title")
	}
}

func TestEdenMissingBindingNeverFallsBackToAutosave(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	writeFile(t, filepath.Join(root, "rp_savedata.dat"), []byte("local"))
	if err := os.Remove(filepath.Join(f.saves.eden.settings, "eden-saves.json")); err != nil {
		t.Fatal(err)
	}
	if st := f.statusOf(t, 319); st.Syncs || st.Slot != "" || st.WhyCode != "" {
		t.Fatalf("status %+v", st)
	}
	if s := f.syncGame(t, 319); s.Uploaded != 0 || len(f.romm.requests) != 0 {
		t.Fatalf("sync %+v", s)
	}
}

func TestEdenWhyCodeDistinguishesUnmappedROMFromInvalidBinding(t *testing.T) {
	f := newSaveFixture(t)
	f.edenGame(t)
	unmapEdenTestROM(t, f)
	if st := f.statusOf(t, 319); st.Syncs || st.WhyCode != edenUnmappedWhyCode || !strings.Contains(st.Why, "explicit Eden title mapping") {
		t.Fatalf("unmapped status %+v", st)
	}

	qtConfig := filepath.Join(f.saves.eden.home, ".config/eden/qt-config.ini")
	data, err := os.ReadFile(qtConfig)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("current_user=0"), []byte("current_user=1"), 1)
	if err = os.WriteFile(qtConfig, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := f.statusOf(t, 319); st.Syncs || st.WhyCode != "" || !strings.Contains(st.Why, "current user") {
		t.Fatalf("invalid binding status %+v", st)
	}
}

func unmapEdenTestROM(t *testing.T, f *saveFixture) {
	t.Helper()
	config := filepath.Join(f.saves.eden.settings, "eden-saves.json")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	var binding edenBinding
	if err = json.Unmarshal(data, &binding); err != nil {
		t.Fatal(err)
	}
	binding.Games = map[string]string{"320": testEdenTitle}
	data, err = json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestEdenRemovedMappingDoesNotHideSaveHistory(t *testing.T) {
	for _, state := range []domain.SaveState{domain.SaveConflict, domain.SaveFailed, domain.SaveSynced} {
		t.Run(string(state), func(t *testing.T) {
			f := newSaveFixture(t)
			f.edenGame(t)
			unmapEdenTestROM(t, f)
			checked := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
			record := domain.SaveRecord{
				ROMID: 319, Path: "previous-eden-target.zip", Slot: EdenSlot, Emulator: "eden",
				State: state, Problem: "previous sync state", CheckedAt: checked,
			}
			if err := f.store.PutSaveRecord(context.Background(), record); err != nil {
				t.Fatal(err)
			}

			status := f.statusOf(t, 319)
			if status.Syncs || status.WhyCode != "" || status.State != state || status.Problem != record.Problem || !status.CheckedAt.Equal(checked) {
				t.Fatalf("history-hidden status %+v", status)
			}
		})
	}
}

func TestEdenChangedProfileIndexRefusesBeforeSaveAndAPI(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	file := filepath.Join(root, "rp_savedata.dat")
	writeFile(t, file, []byte("preserve this profile"))
	config := filepath.Join(f.saves.eden.home, ".config/eden/qt-config.ini")
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("current_user=0"), []byte("current_user=1"), 1)
	if err = os.WriteFile(config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if st := f.statusOf(t, 319); st.Syncs || st.WhyCode != "" || !strings.Contains(st.Why, "index 0") {
		t.Fatalf("status %+v", st)
	}
	if sum := f.syncGame(t, 319); sum.Uploaded != 0 || sum.Downloaded != 0 || len(f.romm.requests) != 0 {
		t.Fatalf("sync %+v requests %+v", sum, f.romm.requests)
	}
	if readFile(t, file) != "preserve this profile" {
		t.Fatal("profile save changed")
	}
}

func TestEdenROMRemovalCannotDeleteTheUsersNANDSave(t *testing.T) {
	f := newSaveFixture(t)
	root := f.edenGame(t)
	file := filepath.Join(root, "rp_savedata.dat")
	writeFile(t, file, []byte("preserve progress"))
	f.syncGame(t, 319)
	d, err := f.saves.Deletion(context.Background(), 319)
	if err != nil || !d.Has || d.Why == "" {
		t.Fatalf("deletion %+v %v", d, err)
	}
	if action, err := f.saves.setAside(context.Background(), 319); err == nil || action != nil {
		t.Fatal("NAND save deletion was allowed")
	}
	if readFile(t, file) != "preserve progress" {
		t.Fatal("save changed")
	}
}
