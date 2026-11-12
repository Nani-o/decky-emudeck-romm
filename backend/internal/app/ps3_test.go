package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

type ps3InstallerFunc func(context.Context, int, string) (domain.PS3InstallResult, error)

func (f ps3InstallerFunc) Install(ctx context.Context, id int, path string) (domain.PS3InstallResult, error) {
	return f(ctx, id, path)
}

const testPS3Desktop = "[Desktop Entry]\nType=Application\nName=RPCS3 NPEB00059\nExec='/home/deck/Applications/rpcs3.AppImage' '/home/deck/Emulation/storage/rpcs3/dev_hdd0/game/NPEB00059/USRDIR/EBOOT.BIN'\nTerminal=false\n"

func ps3Result(id int) domain.PS3InstallResult {
	return domain.PS3InstallResult{ROMID: id, Identity: domain.PS3Identity{ContentID: "EP2007-NPEB00059_00-BZBDSIX000000001", TitleID: "NPEB00059"}, RPCS3User: "00000001", VerifiedAt: time.Now(), DesktopEntry: testPS3Desktop}
}

func servePS3Bundle(t *testing.T, f *downloadFixture) {
	t.Helper()
	f.catalog.mu.Lock()
	f.catalog.roms[11] = domain.ROM{ID: 11, PlatformID: 7, Name: "Burn Zombie Burn!", FileName: "Burn Zombie Burn! (Europe).zip"}
	f.catalog.mu.Unlock()
	f.serve(11, "Burn Zombie Burn! (Europe).zip", zipOf(t, member{"opaque.pkg", "mock-pkg"}, member{"EP2007-NPEB00059_00-BZBDSIX000000001.rap", "0123456789abcdef"}, member{"README.txt", "do not launch me"}))
}

func TestPS3PackageDownloadPlacesOnlyVerifiedDesktop(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Bundle(t, f)
	called := 0
	f.downloads.InstallPS3PackagesWith(ps3InstallerFunc(func(ctx context.Context, id int, dir string) (domain.PS3InstallResult, error) {
		called++
		info, err := os.Stat(dir)
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("private stage %v %v", info, err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 2 {
			t.Fatalf("inputs %v %v", entries, err)
		}
		if got := readFile(t, filepath.Join(dir, "opaque.pkg")); got != "mock-pkg" {
			t.Fatalf("PKG %q", got)
		}
		if filepath.Base(dir) != ".ps3-package" {
			t.Fatal(dir)
		}
		return ps3Result(id), nil
	}))
	rec := f.installed(t, 11)
	if called != 1 || len(rec.Files) != 1 || len(rec.Snapshot) != 1 {
		t.Fatalf("calls %d rec %+v", called, rec)
	}
	file := rec.Files[0]
	if file.Path != filepath.Join(f.root, "roms", "ps3", "Burn Zombie Burn! (Europe).desktop") || file.SourceFileID != 110 {
		t.Fatalf("owned %+v", file)
	}
	if readFile(t, file.Path) != testPS3Desktop {
		t.Fatal("wrong launcher")
	}
	if rec.HasUpdate(f.server.files[11]) {
		t.Fatal("the converted package must not report a permanent update")
	}
}

func TestPS3InstallFailureOrUnverifiedResultNeverBecomesReady(t *testing.T) {
	for _, kind := range []string{"native failure", "unverified result", "cancelled native install"} {
		t.Run(kind, func(t *testing.T) {
			f := newDownloadFixture(t, 1)
			servePS3Bundle(t, f)
			f.downloads.InstallPS3PackagesWith(ps3InstallerFunc(func(ctx context.Context, id int, dir string) (domain.PS3InstallResult, error) {
				if kind == "unverified result" {
					r := ps3Result(id)
					r.DesktopEntry = ""
					return r, nil
				}
				if kind == "cancelled native install" {
					return domain.PS3InstallResult{}, context.Canceled
				}
				return domain.PS3InstallResult{}, errors.New("PKG command exited without verified files")
			}))
			if _, err := f.downloads.Queue(context.Background(), 11); err != nil {
				t.Fatal(err)
			}
			ended := f.end(t, 11)
			if ended.State != domain.StateFailed {
				t.Fatalf("state %+v", ended)
			}
			if _, ok, _ := f.owned.Owned(context.Background(), 11); ok {
				t.Fatal("unverified title became owned")
			}
			if !strings.Contains(ended.Problem.Error(), "retained") && kind != "unverified result" {
				t.Fatal(ended.Problem)
			}
			if _, err := os.Stat(filepath.Join(f.downloads.staging(11), ".ps3-package", "opaque.pkg")); err != nil {
				t.Fatalf("staging lost %v", err)
			}
			if entries := listDir(t, filepath.Join(f.root, "roms", "ps3")); len(entries) != 0 {
				t.Fatalf("ROM files placed %v", entries)
			}
		})
	}
}

func TestPS3ShortcutCollisionDoesNotInvokeNativeInstaller(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Bundle(t, f)
	called := false
	f.downloads.InstallPS3PackagesWith(ps3InstallerFunc(func(context.Context, int, string) (domain.PS3InstallResult, error) {
		called = true
		return ps3Result(11), nil
	}))
	target := filepath.Join(f.root, "roms", "ps3", "Burn Zombie Burn! (Europe).desktop")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("user-managed shortcut"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.downloads.Queue(context.Background(), 11); err == nil {
		if ended := f.end(t, 11); ended.State != domain.StateFailed {
			t.Fatalf("state %+v", ended)
		}
	} else if kindOf(err) != domain.KindTargetExists {
		t.Fatal(err)
	}
	if called || readFile(t, target) != "user-managed shortcut" {
		t.Fatal("collision was overwritten or native install ran")
	}
}

func TestPS3DiscDirectoryDoesNotInvokePackageInstaller(t *testing.T) {
	f := newDownloadFixture(t, 1)
	called := false
	f.downloads.InstallPS3PackagesWith(ps3InstallerFunc(func(context.Context, int, string) (domain.PS3InstallResult, error) {
		called = true
		return ps3Result(11), nil
	}))
	f.serveSet(11, gameFile{"PS3_GAME/PARAM.SFO", "game", "disc metadata"}, gameFile{"PS3_GAME/USRDIR/EBOOT.BIN", "game", "disc boot"}, gameFile{"PS3_EXTRA/DATA00/soundtrack.pkg", "game", "disc bonus package"})
	rec := f.installed(t, 11)
	if called || len(rec.Files) != 3 {
		t.Fatalf("called %v rec %+v", called, rec)
	}
}

func TestPS3LauncherStagingNeverOverwritesChangedOrRedirectedFiles(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "made", "game.desktop")
	if err := writePS3Launcher(path, []byte("expected")); err != nil {
		t.Fatal(err)
	}
	if err := writePS3Launcher(path, []byte("expected")); err != nil {
		t.Fatal(err)
	}
	if err := writePS3Launcher(path, []byte("different")); err == nil {
		t.Fatal("modified launcher overwritten")
	}
	if readFile(t, path) != "expected" {
		t.Fatal("launcher changed")
	}
	outside := filepath.Join(root, "keep.sav")
	if err := os.WriteFile(outside, []byte("save progress"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if err := writePS3Launcher(path, []byte("expected")); err == nil {
		t.Fatal("redirected launcher accepted")
	}
	if readFile(t, outside) != "save progress" {
		t.Fatal("save changed")
	}
}

func TestPS3OwnershipFailureRetainsInputsAndRetriesWithoutPublishingPKG(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Bundle(t, f)
	called := 0
	f.downloads.InstallPS3PackagesWith(ps3InstallerFunc(func(ctx context.Context, id int, dir string) (domain.PS3InstallResult, error) {
		called++
		return ps3Result(id), nil
	}))
	f.owned.saveErr = errors.New("state.db unavailable")
	if _, err := f.downloads.Queue(context.Background(), 11); err != nil {
		t.Fatal(err)
	}
	if end := f.end(t, 11); end.State != domain.StateFailed {
		t.Fatalf("state %+v", end)
	}
	if _, err := os.Stat(filepath.Join(f.downloads.staging(11), ".ps3-package", "opaque.pkg")); err != nil {
		t.Fatal("private inputs lost", err)
	}
	if entries := listDir(t, filepath.Join(f.root, "roms", "ps3")); len(entries) != 0 {
		t.Fatalf("unowned files left %v", entries)
	}
	f.owned.mu.Lock()
	f.owned.saveErr = nil
	f.owned.mu.Unlock()
	rec := f.installed(t, 11)
	if called != 2 || len(rec.Files) != 1 || !strings.HasSuffix(rec.Files[0].Path, ".desktop") {
		t.Fatalf("calls %d rec %+v", called, rec)
	}
}

type ps3RemovalFake struct {
	check  func(context.Context, int) (bool, error)
	remove func(context.Context, int) (bool, error)
}

func (f ps3RemovalFake) Install(_ context.Context, id int, _ string) (domain.PS3InstallResult, error) {
	return ps3Result(id), nil
}
func (f ps3RemovalFake) CheckRemoval(ctx context.Context, id int) (bool, error) {
	return f.check(ctx, id)
}
func (f ps3RemovalFake) Remove(ctx context.Context, id int) (bool, error) { return f.remove(ctx, id) }

func TestPS3DeleteRetainsLauncherAndOwnershipWhenNativeRemovalRefuses(t *testing.T) {
	for _, kind := range []string{"preflight", "partial native failure", "changed launcher", "missing journal", "success"} {
		t.Run(kind, func(t *testing.T) {
			f := newDownloadFixture(t, 1)
			servePS3Bundle(t, f)
			called := 0
			fake := ps3RemovalFake{check: func(context.Context, int) (bool, error) {
				if kind == "preflight" {
					return true, errors.New("native changed")
				}
				return kind != "missing journal", nil
			}, remove: func(context.Context, int) (bool, error) {
				called++
				if kind == "partial native failure" {
					return true, errors.New("interrupted")
				}
				return true, nil
			}}
			f.downloads.InstallPS3PackagesWith(fake)
			rec := f.installed(t, 11)
			launcher := rec.Files[0].Path
			if kind == "changed launcher" {
				if err := os.WriteFile(launcher, []byte("user changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := f.downloads.deleteGame(context.Background(), 11, false)
			if kind == "success" {
				if err != nil || called != 1 {
					t.Fatalf("success %v calls%d", err, called)
				}
				if _, ok, _ := f.owned.Owned(context.Background(), 11); ok {
					t.Fatal("ownership retained after success")
				}
				return
			}
			if err == nil {
				t.Fatal("failure accepted")
			}
			if _, err := os.Stat(launcher); err != nil {
				t.Fatal("launcher lost on native refusal")
			}
			if _, ok, _ := f.owned.Owned(context.Background(), 11); !ok {
				t.Fatal("ownership lost on native refusal")
			}
			if kind != "partial native failure" && called != 0 {
				t.Fatal("native delete ran before launcher/ownership preflight")
			}
		})
	}
}

type ps3DeleteStore struct {
	DownloadStore
	fail bool
}

func (s *ps3DeleteStore) DeleteOwned(ctx context.Context, id int) error {
	if s.fail {
		s.fail = false
		return errors.New("injected ownership deletion failure")
	}
	return s.DownloadStore.DeleteOwned(ctx, id)
}
func TestPS3DeleteRetryAfterOwnershipWriteFailureAndMissingLauncher(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Bundle(t, f)
	removals := 0
	f.downloads.InstallPS3PackagesWith(ps3RemovalFake{check: func(context.Context, int) (bool, error) { return true, nil }, remove: func(context.Context, int) (bool, error) { removals++; return true, nil }})
	rec := f.installed(t, 11)
	f.downloads.store = &ps3DeleteStore{DownloadStore: f.downloads.store, fail: true}
	if err := f.downloads.deleteGame(context.Background(), 11, false); err == nil {
		t.Fatal("injected failure hidden")
	}
	if _, err := os.Stat(rec.Files[0].Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("launcher was not deleted")
	}
	if _, ok, _ := f.owned.Owned(context.Background(), 11); !ok {
		t.Fatal("retry ownership lost")
	}
	if err := f.downloads.deleteGame(context.Background(), 11, false); err != nil {
		t.Fatalf("retry failed %v", err)
	}
	if removals != 2 {
		t.Fatal("native removal retry not called")
	}
}
