package rpcs3

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func uruntimeFixture() []byte {
	data := make([]byte, 1024)
	copy(data, "\x7fELF")
	data[4] = 2
	data[5] = 1
	data[6] = 1
	copy(data[8:], "AI\x02")
	binary.LittleEndian.PutUint16(data[16:], 3)
	binary.LittleEndian.PutUint16(data[18:], 62)
	binary.LittleEndian.PutUint32(data[20:], 1)
	binary.LittleEndian.PutUint64(data[32:], 64)
	binary.LittleEndian.PutUint16(data[52:], 64)
	binary.LittleEndian.PutUint16(data[54:], 56)
	binary.LittleEndian.PutUint16(data[56:], 1)
	copy(data[512:], "DWARFS deliberately is not a SquashFS image")
	return data
}
func TestValidateInstalledAppImageDoesNotRequireSquashFS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rpcs3.AppImage")
	if err := os.WriteFile(path, uruntimeFixture(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateAppImage(path); err != nil {
		t.Fatal(err)
	}
}
func TestValidateInstalledAppImageRefusesWrongOrRedirectedExecutables(t *testing.T) {
	for _, kind := range []string{"not ELF", "wrong machine", "wrong appimage marker", "truncated program table", "not executable", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rpcs3.AppImage")
			data := uruntimeFixture()
			mode := os.FileMode(0o700)
			switch kind {
			case "not ELF":
				copy(data, "bad!")
			case "wrong machine":
				binary.LittleEndian.PutUint16(data[18:], 183)
			case "wrong appimage marker":
				data[10] = 1
			case "truncated program table":
				binary.LittleEndian.PutUint64(data[32:], 1000)
			case "not executable":
				mode = 0o600
			}
			if err := os.WriteFile(path, data, mode); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				target := path + ".target"
				if err := os.Rename(path, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := validateAppImage(path); err == nil {
				t.Fatal("unsafe executable accepted")
			}
		})
	}
}
