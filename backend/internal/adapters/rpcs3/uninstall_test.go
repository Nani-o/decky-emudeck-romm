package rpcs3

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func installedRemovalFixture(t *testing.T) (*Installer, string, string) {
	t.Helper()
	stage, rap := makeTestBundle(t)
	var root string
	i, r, _ := makeInstaller(t, runnerFunc(func(context.Context, string, ...string) error { return writeTestRPCS3Install(root, rap) }), nil, nil)
	root = r
	if _, err := i.Install(context.Background(), 77, stage); err != nil {
		t.Fatal(err)
	}
	return i, root, stage
}
func TestRemoveOwnedNativeTitlePreservesSavesAndAllowsReinstall(t *testing.T) {
	i, root, stage := installedRemovalFixture(t)
	save := filepath.Join(root, "home", "00000001", "savedata", "keep.dat")
	other := filepath.Join(root, "game", "NPEB99999", "keep.dat")
	for _, p := range []string{save, other} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := i.CheckRemoval(context.Background(), 77); !ok || err != nil {
		t.Fatalf("preflight %v %v", ok, err)
	}
	for n := 0; n < 2; n++ {
		if ok, err := i.Remove(context.Background(), 77); !ok || err != nil {
			t.Fatalf("remove %v %v", ok, err)
		}
	}
	for _, p := range []string{save, other} {
		if b, err := os.ReadFile(p); err != nil || string(b) != "keep" {
			t.Fatalf("preserved file %s: %v", p, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "game", testTitleID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("title retained")
	}
	if _, err := i.Install(context.Background(), 77, stage); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
}
func TestNativeRemovalRefusesChangedOrUnownedFilesBeforeDeleting(t *testing.T) {
	for _, kind := range []string{"title", "license", "extra", "unknown", "legacy", "running"} {
		t.Run(kind, func(t *testing.T) {
			i, root, _ := installedRemovalFixture(t)
			eboot := filepath.Join(root, "game", testTitleID, "USRDIR", "EBOOT.BIN")
			license := filepath.Join(root, "home", "00000001", "exdata", testContentID+".rap")
			switch kind {
			case "title":
				_ = os.WriteFile(eboot, []byte("modified"), 0600)
			case "license":
				_ = os.WriteFile(license, []byte("modified"), 0600)
			case "extra":
				_ = os.WriteFile(filepath.Join(root, "game", testTitleID, "extra"), []byte("user"), 0600)
			case "unknown":
				_ = os.Remove(intentFile(i.config.IntentDir, testTitleID))
			case "legacy":
				intent, _, _ := readIntent(i.config.IntentDir, testTitleID)
				intent.Version = 1
				intent.DevHDD0 = ""
				intent.LicenseOwned = false
				intent.EntryCount = 0
				intent.Inventory = nil
				_ = os.Remove(intentFile(i.config.IntentDir, testTitleID))
				if err := createIntent(i.config.IntentDir, intent); err != nil {
					t.Fatal(err)
				}
			case "running":
				i.processes = nil
			}
			ok, err := i.Remove(context.Background(), 77)
			if kind == "unknown" {
				if ok || err != nil {
					t.Fatal("unknown adopted")
				}
			} else if err == nil {
				t.Fatal("unsafe removal accepted")
			}
			for _, p := range []string{eboot, license} {
				if _, err := os.Stat(p); err != nil {
					t.Fatalf("file removed before refusal: %s %v", p, err)
				}
			}
		})
	}
}
func TestNativeRemovalRetriesPartialWithoutDeletingModifiedRemainingFile(t *testing.T) {
	i, root, _ := installedRemovalFixture(t)
	sfo := filepath.Join(root, "game", testTitleID, "PARAM.SFO")
	eboot := filepath.Join(root, "game", testTitleID, "USRDIR", "EBOOT.BIN")
	i.removeTitle = func(string, installedInventory) error {
		if err := os.Remove(sfo); err != nil {
			return err
		}
		return errors.New("injected removal interruption")
	}
	if _, err := i.Remove(context.Background(), 77); err == nil {
		t.Fatal("injected failure hidden")
	}
	i.removeTitle = nil
	if err := os.WriteFile(eboot, []byte("user changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Remove(context.Background(), 77); err == nil {
		t.Fatal("modified remaining file deleted")
	}
	if err := os.WriteFile(eboot, []byte("boot executable"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Remove(context.Background(), 77); err != nil {
		t.Fatalf("partial retry failed: %v", err)
	}
}
func TestReinstallRefusesUnmanagedTitleAfterRemoval(t *testing.T) {
	i, root, stage := installedRemovalFixture(t)
	if _, err := i.Remove(context.Background(), 77); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "game", testTitleID), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := i.Install(context.Background(), 77, stage); !errors.Is(err, ErrIntentNeedsReview) {
		t.Fatalf("unmanaged title accepted %v", err)
	}
}

func TestReinstallResumesInterruptedArchivalWithoutReinstallingUnknownTitle(t *testing.T) {
	for _, kind := range []string{"second rename failed", "second rename completed then interrupted", "unmanaged title introduced"} {
		t.Run(kind, func(t *testing.T) {
			i, root, stage := installedRemovalFixture(t)
			if _, err := i.Remove(context.Background(), 77); err != nil {
				t.Fatal(err)
			}
			renames := 0
			i.archiveRename = func(source, target string) error {
				renames++
				if renames == 2 {
					if kind == "second rename completed then interrupted" {
						if err := os.Rename(source, target); err != nil {
							return err
						}
					}
					return errors.New("injected archival interruption")
				}
				return os.Rename(source, target)
			}
			if _, err := i.Install(context.Background(), 77, stage); err == nil {
				t.Fatal("archival interruption hidden")
			}
			if _, err := os.Stat(i.restartPath(testTitleID)); err != nil {
				t.Fatal("restart receipt lost")
			}
			i.archiveRename = nil
			if kind == "unmanaged title introduced" {
				if err := os.Mkdir(filepath.Join(root, "game", testTitleID), 0700); err != nil {
					t.Fatal(err)
				}
				if _, err := i.Install(context.Background(), 77, stage); !errors.Is(err, ErrIntentNeedsReview) {
					t.Fatalf("unknown title accepted: %v", err)
				}
				return
			}
			if _, err := i.Install(context.Background(), 77, stage); err != nil {
				t.Fatalf("retry required manual repair: %v", err)
			}
			if _, err := os.Stat(i.restartPath(testTitleID)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("restart receipt not cleared")
			}
		})
	}
}
