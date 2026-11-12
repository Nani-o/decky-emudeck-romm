package rpcs3

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func inventoryFixture(t *testing.T) (string, installedInventory) {
	t.Helper()
	root := filepath.Join(resolvedTestTempDir(t), "dev_hdd0", "game", testTitleID)
	for name, data := range map[string]string{"PARAM.SFO": "title", "USRDIR/EBOOT.BIN": "executable", "USRDIR/ASSETS/data.bin": "game data"} {
		inventoryWrite(t, filepath.Join(root, filepath.FromSlash(name)), data)
	}
	if err := os.Mkdir(filepath.Join(root, "EMPTY"), 0700); err != nil {
		t.Fatal(err)
	}
	inv, err := captureInstalledInventory(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, inv
}

func inventoryWrite(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func inventoryClone(inv installedInventory) installedInventory {
	return installedInventory{Files: slices.Clone(inv.Files), Directories: slices.Clone(inv.Directories)}
}

func TestInstalledInventoryCapturesAndRemovesOnlyOwnedTitle(t *testing.T) {
	root, inv := inventoryFixture(t)
	if len(inv.Files) != 3 || !reflect.DeepEqual(inv.Directories, []string{"EMPTY", "USRDIR", "USRDIR/ASSETS"}) {
		t.Fatalf("unexpected inventory: %+v", inv)
	}
	sum := sha256.Sum256([]byte("title"))
	if inv.Files[0] != (installedFile{Path: "PARAM.SFO", Size: 5, SHA256: hex.EncodeToString(sum[:])}) {
		t.Fatalf("incorrect fingerprint: %+v", inv.Files[0])
	}
	if err := checkInstalledInventory(root, inv, false); err != nil {
		t.Fatal(err)
	}
	nand := filepath.Dir(filepath.Dir(root))
	preserved := []string{filepath.Join(nand, "home", "00000001", "savedata", "progress.dat"), filepath.Join(nand, "home", "00000001", "trophy", "progress.dat"), filepath.Join(nand, "game", "NPEB99999", "USRDIR", "EBOOT.BIN"), filepath.Join(nand, "firmware", "sentinel")}
	for _, name := range preserved {
		inventoryWrite(t, name, "keep")
	}
	// Journal ordering is not significant, including parents after children.
	slices.Reverse(inv.Files)
	slices.Reverse(inv.Directories)
	if err := removeInstalledInventory(root, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("title remains: %v", err)
	}
	for _, name := range preserved {
		data, err := os.ReadFile(name)
		if err != nil || string(data) != "keep" {
			t.Fatalf("outside title changed %s: %q, %v", name, data, err)
		}
	}
	if err := checkInstalledInventory(root, inv, true); err != nil {
		t.Fatal(err)
	}
	if err := removeInstalledInventory(root, inv); err != nil {
		t.Fatal(err)
	}
	if err := checkInstalledInventory(root, inv, false); err == nil {
		t.Fatal("strict check accepted removed title")
	}
}

func TestInstalledInventoryRefusesChangedOrExtraEntriesBeforeRemoval(t *testing.T) {
	for _, kind := range []string{"same-size change", "extra file", "extra directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, inv := inventoryFixture(t)
			switch kind {
			case "same-size change":
				inventoryWrite(t, filepath.Join(root, "PARAM.SFO"), "other")
			case "extra file":
				inventoryWrite(t, filepath.Join(root, "user.dat"), "keep")
			case "extra directory":
				if err := os.Mkdir(filepath.Join(root, "USER"), 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(filepath.Dir(filepath.Dir(root)), "outside.dat")
				inventoryWrite(t, target, "keep")
				if err := os.Symlink(target, filepath.Join(root, "escape")); err != nil {
					t.Fatal(err)
				}
				if _, err := captureInstalledInventory(root); err == nil {
					t.Fatal("captured symlink")
				}
			}
			if err := checkInstalledInventory(root, inv, false); err == nil {
				t.Fatal("strict check accepted changed tree")
			}
			if err := checkInstalledInventory(root, inv, true); err == nil {
				t.Fatal("resume check accepted changed tree")
			}
			if err := removeInstalledInventory(root, inv); err == nil {
				t.Fatal("removed changed tree")
			}
			data, err := os.ReadFile(filepath.Join(root, "USRDIR", "EBOOT.BIN"))
			if err != nil || string(data) != "executable" {
				t.Fatalf("preflight deleted known content: %q %v", data, err)
			}
		})
	}
}

func TestInstalledInventoryMissingEntriesAndInterruptedRemoval(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		root, inv := inventoryFixture(t)
		if err := os.Remove(filepath.Join(root, "PARAM.SFO")); err != nil {
			t.Fatal(err)
		}
		if err := checkInstalledInventory(root, inv, false); err == nil {
			t.Fatal("strict check accepted missing file")
		}
		if err := checkInstalledInventory(root, inv, true); err != nil {
			t.Fatal(err)
		}
		if err := removeInstalledInventory(root, inv); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		root, inv := inventoryFixture(t)
		calls := 0
		interrupted := errors.New("injected unlink failure")
		err := removeInstalledInventoryWith(root, inv, func(parent *os.Root, name string) error {
			calls++
			if calls == 2 {
				return interrupted
			}
			return parent.Remove(name)
		})
		if !errors.Is(err, interrupted) {
			t.Fatalf("wrong failure: %v", err)
		}
		if err := checkInstalledInventory(root, inv, false); err == nil {
			t.Fatal("strict check accepted partial removal")
		}
		if err := checkInstalledInventory(root, inv, true); err != nil {
			t.Fatal(err)
		}
		if err := removeInstalledInventory(root, inv); err != nil {
			t.Fatal(err)
		}
	})
}

func TestInstalledInventoryPreservesNewFilesDuringRemoval(t *testing.T) {
	for _, relative := range []string{"user.dat", "USRDIR/ASSETS/user.dat"} {
		t.Run(relative, func(t *testing.T) {
			root, inv := inventoryFixture(t)
			added := false
			err := removeInstalledInventoryWith(root, inv, func(parent *os.Root, name string) error {
				if !added {
					added = true
					inventoryWrite(t, filepath.Join(root, filepath.FromSlash(relative)), "keep")
				}
				return parent.Remove(name)
			})
			if err == nil {
				t.Fatal("removed title with newly introduced content")
			}
			data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
			if err != nil || string(data) != "keep" {
				t.Fatalf("new file lost: %q %v", data, err)
			}
		})
	}
}

func TestInstalledInventoryRefusesRootReplacementDuringRemoval(t *testing.T) {
	root, inv := inventoryFixture(t)
	moved := root + "-moved"
	calls := 0
	err := removeInstalledInventoryWith(root, inv, func(parent *os.Root, name string) error {
		calls++
		if calls == 1 {
			if err := os.Rename(root, moved); err != nil {
				t.Fatal(err)
			}
			inventoryWrite(t, filepath.Join(root, "replacement.dat"), "keep")
		}
		return parent.Remove(name)
	})
	if err == nil {
		t.Fatal("continued removing replaced root")
	}
	if calls != 1 {
		t.Fatalf("mutations continued after replacement: %d", calls)
	}
	if data, err := os.ReadFile(filepath.Join(root, "replacement.dat")); err != nil || string(data) != "keep" {
		t.Fatalf("replacement changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(moved, "PARAM.SFO")); err != nil {
		t.Fatalf("continued deleting renamed title: %v", err)
	}
}

func TestInstalledInventoryRejectsUnsafeRoots(t *testing.T) {
	root, inv := inventoryFixture(t)
	nand := filepath.Dir(filepath.Dir(root))
	redirected := filepath.Join(filepath.Dir(root), "NPEB99998")
	if err := os.Symlink(root, redirected); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(filepath.Dir(nand), "redirect")
	if err := os.Symlink(nand, parentLink); err != nil {
		t.Fatal(err)
	}
	dotfiles := filepath.Join(filepath.Dir(nand), ".dotfiles", "game", testTitleID)
	inventoryWrite(t, filepath.Join(dotfiles, "PARAM.SFO"), "keep")
	for _, bad := range []string{nand, filepath.Dir(root), filepath.Join(nand, "home", "00000001", "savedata"), "game/" + testTitleID, filepath.Join(filepath.Dir(root), "npeb00059"), redirected, filepath.Join(parentLink, "game", testTitleID), dotfiles, root + "/../" + testTitleID} {
		t.Run(bad, func(t *testing.T) {
			if _, err := captureInstalledInventory(bad); err == nil {
				t.Fatal("captured unsafe root")
			}
			if err := checkInstalledInventory(bad, inv, true); err == nil {
				t.Fatal("accepted unsafe root")
			}
			if err := removeInstalledInventory(bad, inv); err == nil {
				t.Fatal("removed unsafe root")
			}
		})
	}
	// The absent-root retry must reject redirected ancestors too.
	if err := checkInstalledInventory(filepath.Join(parentLink, "game", "NPEB99997"), inv, true); err == nil {
		t.Fatal("accepted missing title under redirected parent")
	}
	if err := checkInstalledInventory(root, inv, false); err != nil {
		t.Fatalf("safe title affected: %v", err)
	}
}

func TestValidateInstalledInventoryRejectsMalformedManifest(t *testing.T) {
	_, valid := inventoryFixture(t)
	cases := map[string]func(*installedInventory){
		"empty":                    func(i *installedInventory) { i.Files = nil },
		"absolute":                 func(i *installedInventory) { i.Files[0].Path = "/etc/passwd" },
		"traversal":                func(i *installedInventory) { i.Files[0].Path = "../outside" },
		"backslash":                func(i *installedInventory) { i.Files[0].Path = `USRDIR\file` },
		"noncanonical":             func(i *installedInventory) { i.Files[0].Path = "USRDIR//file" },
		"control":                  func(i *installedInventory) { i.Files[0].Path = "bad\nfile" },
		"duplicate file":           func(i *installedInventory) { i.Files = append(i.Files, i.Files[0]) },
		"duplicate dir":            func(i *installedInventory) { i.Directories = append(i.Directories, i.Directories[0]) },
		"file directory collision": func(i *installedInventory) { i.Directories = append(i.Directories, i.Files[0].Path) },
		"file ancestor":            func(i *installedInventory) { i.Files[1].Path = i.Files[0].Path + "/child" },
		"undeclared parent":        func(i *installedInventory) { i.Directories = nil },
		"negative":                 func(i *installedInventory) { i.Files[0].Size = -1 },
		"over limit":               func(i *installedInventory) { i.Files[0].Size = maxInstalledBytes + 1 },
		"combined over limit":      func(i *installedInventory) { i.Files[0].Size = maxInstalledBytes },
		"bad hash":                 func(i *installedInventory) { i.Files[0].SHA256 = "zz" },
		"uppercase hash":           func(i *installedInventory) { i.Files[0].SHA256 = strings.ToUpper(i.Files[0].SHA256) },
	}
	for name, modify := range cases {
		t.Run(name, func(t *testing.T) {
			inv := inventoryClone(valid)
			modify(&inv)
			if err := validateInstalledInventory(inv); err == nil {
				t.Fatalf("accepted %+v", inv)
			}
		})
	}
}

func TestValidateInstalledInventoryBoundsManifestSize(t *testing.T) {
	_, inv := inventoryFixture(t)
	for j := 0; j < 2100; j++ {
		inv.Directories = append(inv.Directories, fmt.Sprintf("%04d", j)+strings.Repeat("x", 4080))
	}
	if err := validateInstalledInventory(inv); err == nil {
		t.Fatal("accepted manifest exceeding journal size bound")
	}
}

func TestInstalledInventoryRechecksChangedLeafBeforeUnlink(t *testing.T) {
	root, inv := inventoryFixture(t)
	outside := filepath.Join(filepath.Dir(filepath.Dir(root)), "outside.dat")
	inventoryWrite(t, outside, "keep")
	changed := false
	err := removeInstalledInventoryWith(root, inv, func(parent *os.Root, name string) error {
		if !changed {
			changed = true
			leaf := filepath.Join(root, "PARAM.SFO")
			if err := os.Remove(leaf); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, leaf); err != nil {
				t.Fatal(err)
			}
		}
		return parent.Remove(name)
	})
	if err == nil {
		t.Fatal("removed newly redirected file")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("outside data changed: %q %v", data, err)
	}
	if info, err := os.Lstat(filepath.Join(root, "PARAM.SFO")); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Fatalf("unexpected symlink removed: %v", err)
	}
}

func TestInstalledInventoryRequiresParentFlushBeforeSuccess(t *testing.T) {
	root, inv := inventoryFixture(t)
	err := removeInstalledInventoryWith(root, inv, func(parent *os.Root, name string) error {
		if err := parent.Remove(name); err != nil {
			return err
		}
		// Force the required parent-directory flush to fail after one unlink.
		return parent.Close()
	})
	if err == nil {
		t.Fatal("reported removal success without flushing parent")
	}
	if err := checkInstalledInventory(root, inv, true); err != nil {
		t.Fatalf("partial tree cannot resume: %v", err)
	}
	if err := removeInstalledInventory(root, inv); err != nil {
		t.Fatal(err)
	}
}
