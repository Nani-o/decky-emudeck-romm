package rpcs3

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

func TestReadDevHDD0ReadsExplicitConfiguredRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "RPCS3 NAND")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "vfs.yml")
	data := "$(EmulatorDir): \"\"\n/dev_hdd0/: \"" + root + "\" # active NAND\n/dev_flash/: other\n"
	if err := os.WriteFile(config, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readDevHDD0(config, t.TempDir())
	if err != nil || got != root {
		t.Fatalf("dev_hdd0 = %q, error = %v", got, err)
	}
}

func TestReadDevHDD0RejectsMissingDuplicateAndSymlinkMappings(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{name: "missing", data: "/dev_flash/: path\n"},
		{name: "duplicate", data: "/dev_hdd0/: /a\n/dev_hdd0/: /b\n"},
		{name: "relative", data: "/dev_hdd0/: relative/path\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "vfs.yml")
			if err := os.WriteFile(config, []byte(tt.data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readDevHDD0(config, t.TempDir()); err == nil {
				t.Fatal("expected invalid mapping")
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		base := t.TempDir()
		actual := filepath.Join(base, "actual")
		if err := os.Mkdir(actual, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "linked")
		if err := os.Symlink(actual, link); err != nil {
			t.Fatal(err)
		}
		config := filepath.Join(base, "vfs.yml")
		if err := os.WriteFile(config, []byte("/dev_hdd0/: "+link+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readDevHDD0(config, base); err == nil {
			t.Fatal("expected symlinked VFS root to be rejected")
		}
	})
}

func TestReadActiveUserParsesActiveUserAndDefaultsOnlyWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent_settings.dat")
	if user, err := readActiveUser(path); err != nil || user != "00000001" {
		t.Fatalf("default user = %q, error = %v", user, err)
	}
	if err := os.WriteFile(path, []byte("[Other]\nactive_user=00000002\n[Users]\nactive_user=\"00000042\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if user, err := readActiveUser(path); err != nil || user != "00000042" {
		t.Fatalf("configured user = %q, error = %v", user, err)
	}
	if err := os.WriteFile(path, []byte("[Users]\nactive_user=not-a-user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readActiveUser(path); err == nil {
		t.Fatal("expected invalid active user to fail closed")
	}
}

func TestProfileAllowsRPCS3ToCreateMissingExdataButKeepsNANDExplicit(t *testing.T) {
	root := filepath.Join(t.TempDir(), "dev_hdd0")
	userDir := filepath.Join(root, "home", "00000001")
	if err := os.MkdirAll(userDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(t.TempDir(), "rpcs3")
	if err := os.MkdirAll(filepath.Join(configDir, "GuiConfigs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "vfs.yml"), []byte("/dev_hdd0/: "+root+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "GuiConfigs", "persistent_settings.dat"), []byte("[Users]\nactive_user=00000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	profile, err := (Config{ConfigDir: configDir, HomeDir: t.TempDir()}).profile()
	if err != nil || profile.DevHDD0 != root || profile.UserID != "00000001" {
		t.Fatalf("profile %+v, error %v", profile, err)
	}
}

func TestProcessIsRPCS3ChecksExecutableOnly(t *testing.T) {
	if !processIsRPCS3(domain.Process{Args: []string{"/home/deck/Applications/rpcs3.AppImage", "--headless"}}) {
		t.Fatal("RPCS3 executable was not detected")
	}
	if processIsRPCS3(domain.Process{Args: []string{"/usr/bin/sh", "-c", "rpcs3.AppImage"}}) {
		t.Fatal("argument text was incorrectly treated as a running RPCS3 process")
	}
}

func TestValidateAppImageFileRequiresNamedExecutableRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "rpcs3.AppImage")
	if err := os.WriteFile(path, []byte("not needed by this test"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateAppImageFile(path); err != nil {
		t.Fatal(err)
	}
	if err := validateAppImageFile(filepath.Join(directory, "wrong-name")); err == nil {
		t.Fatal("expected unexpected executable name to be rejected")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateAppImageFile(path); err == nil {
		t.Fatal("expected non-executable file to be rejected")
	}
}
