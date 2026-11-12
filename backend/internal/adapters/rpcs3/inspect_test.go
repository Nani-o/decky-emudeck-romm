package rpcs3

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectBundleUsesPackageIdentityAndBoundsTheInput(t *testing.T) {
	directory, _ := makeTestBundle(t)
	bundle, err := InspectBundle(directory)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Identity.ContentID != testContentID || bundle.Identity.TitleID != testTitleID {
		t.Fatalf("unexpected identity %+v", bundle.Identity)
	}
	if bundle.PackageSize <= pkgHeaderSize || len(bundle.PackageSHA256) != 64 || len(bundle.RAPSHA256) != 64 {
		t.Fatalf("bundle is missing size or hashes: %+v", bundle)
	}
}

func TestInspectPKGAcceptsQualifiedBurnZombieHeaderLayout(t *testing.T) {
	const fileSize = int64(168098128)
	const dataOffset = uint64(352)
	const dataSize = uint64(168097680)
	qualified := makeQualifiedBurnZombiePKG()
	binary.BigEndian.PutUint32(qualified[20:24], 968)
	binary.BigEndian.PutUint64(qualified[24:32], uint64(fileSize))
	binary.BigEndian.PutUint64(qualified[32:40], dataOffset)
	binary.BigEndian.PutUint64(qualified[40:48], dataSize)
	path := filepath.Join(t.TempDir(), "sparse-qualified.pkg")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(qualified[:pkgHeaderSize+160]); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Truncate(fileSize); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	identity, gotSize, err := inspectPKG(path)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ContentID != testContentID || identity.TitleID != testTitleID || gotSize != fileSize {
		t.Fatalf("identity=%+v size=%d", identity, gotSize)
	}
}

func TestInspectBundleRejectsNonBaseAndMismatchedPackages(t *testing.T) {
	tests := []struct {
		name string
		pkg  func() []byte
	}{
		{name: "patch flag", pkg: func() []byte {
			return makeTestPKG(testContentID, testTitleID, pkgContentGameExec, pkgFlagEBOOT|pkgFlagRequireLicense|pkgFlagPatch)
		}},
		{name: "wrong content type", pkg: func() []byte {
			return makeTestPKG(testContentID, testTitleID, 0x04, pkgFlagEBOOT|pkgFlagRequireLicense)
		}},
		{name: "metadata title mismatch", pkg: func() []byte {
			return makeTestPKG(testContentID, "NPEB00058", pkgContentGameExec, pkgFlagEBOOT|pkgFlagRequireLicense)
		}},
		{name: "header title mismatch", pkg: func() []byte {
			pkg := makeTestPKG(testContentID, testTitleID, pkgContentGameExec, pkgFlagEBOOT|pkgFlagRequireLicense)
			pkg[0x37] = 'X'
			return pkg
		}},
		{name: "no license requirement", pkg: func() []byte {
			return makeTestPKG(testContentID, testTitleID, pkgContentGameExec, pkgFlagEBOOT)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory, _ := makeTestBundle(t)
			if err := os.WriteFile(filepath.Join(directory, "opaque-download.pkg"), tt.pkg(), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := InspectBundle(directory); err == nil {
				t.Fatal("expected package to be rejected")
			}
		})
	}
}

func TestInspectBundleRejectsUnexpectedFilesAndRAPName(t *testing.T) {
	t.Run("unexpected file", func(t *testing.T) {
		directory, _ := makeTestBundle(t)
		if err := os.WriteFile(filepath.Join(directory, "README.txt"), []byte("extra"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectBundle(directory); !errors.Is(err, ErrInvalidBundle) {
			t.Fatalf("expected ErrInvalidBundle, got %v", err)
		}
	})
	t.Run("RAP name is not the package ContentID", func(t *testing.T) {
		directory, rap := makeTestBundle(t)
		if err := os.Remove(filepath.Join(directory, testContentID+".rap")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "wrong-content.rap"), rap, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectBundle(directory); err == nil {
			t.Fatal("expected mismatched RAP name to be rejected")
		}
	})
	t.Run("shared staging root", func(t *testing.T) {
		directory, _ := makeTestBundle(t)
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := InspectBundle(directory); !errors.Is(err, ErrInvalidBundle) {
			t.Fatalf("expected non-private staging root to be rejected, got %v", err)
		}
	})
}

func TestInspectParamsfoTitleID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "PARAM.SFO")
	if err := os.WriteFile(path, makeTestSFO(testTitleID), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := inspectParamsfoTitleID(path)
	if err != nil || got != testTitleID {
		t.Fatalf("title ID = %q, error = %v", got, err)
	}
	if err := os.WriteFile(path, makeTestSFO("NPEB00058"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = inspectParamsfoTitleID(path)
	if err != nil || got != "NPEB00058" {
		t.Fatalf("valid different SFO title ID = %q, error = %v", got, err)
	}
}
