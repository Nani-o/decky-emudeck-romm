package rpcs3

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testInstallIntent() installIntent {
	return installIntent{
		Version:       1,
		ROMID:         77,
		ContentID:     testContentID,
		TitleID:       testTitleID,
		RPCS3User:     "00000001",
		PackageSHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		RAPSHA256:     "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		Phase:         intentInstalling,
		StartedAt:     time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
	}
}

func TestIntentPersistsAtomicallyAndOnlyAllowsOneTransition(t *testing.T) {
	directory := filepath.Join(resolvedTestTempDir(t), "install-intents")
	intent := testInstallIntent()
	if err := createIntent(directory, intent); err != nil {
		t.Fatal(err)
	}
	current, found, err := readIntent(directory, intent.TitleID)
	if err != nil || !found || current.Phase != intentInstalling {
		t.Fatalf("initial intent = %+v, found=%t, error=%v", current, found, err)
	}
	intent.Phase = intentVerified
	intent.VerifiedAt = intent.StartedAt.Add(time.Minute)
	if err := updateIntent(directory, intent); err != nil {
		t.Fatal(err)
	}
	current, found, err = readIntent(directory, intent.TitleID)
	if err != nil || !found || current.Phase != intentVerified || !current.VerifiedAt.Equal(intent.VerifiedAt) {
		t.Fatalf("verified intent = %+v, found=%t, error=%v", current, found, err)
	}
	intent.Phase = intentNeedsReview
	if err := updateIntent(directory, intent); !errors.Is(err, ErrIntentNeedsReview) {
		t.Fatalf("expected invalid second transition to be refused, got %v", err)
	}
}

func TestIntentRequiresPrivateDirectoryAndFile(t *testing.T) {
	root := resolvedTestTempDir(t)
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDirectory(shared); err == nil {
		t.Fatal("expected shared intent directory to be rejected")
	}
	directory := filepath.Join(root, "private")
	intent := testInstallIntent()
	if err := createIntent(directory, intent); err != nil {
		t.Fatal(err)
	}
	path := intentFile(directory, intent.TitleID)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readIntent(directory, intent.TitleID); err == nil {
		t.Fatal("expected broadly readable intent file to be rejected")
	}
}
