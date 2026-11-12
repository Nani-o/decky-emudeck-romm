package domain

import (
	"slices"
	"testing"
)

func TestPS3ManifestKeepsPatchChainAndTracksAttachments(t *testing.T) {
	files := []ROMFile{{ID: 1, Name: "Game.zip", Category: "game", SizeBytes: 100},
		{ID: 2, Name: "update-1.01.pkg", Category: "update", SizeBytes: 10},
		{ID: 3, Name: "update-1.02.pkg", Category: "update", SizeBytes: 20},
		{ID: 4, Name: "pack.zip", Category: "dlc", SizeBytes: 30},
		{ID: 5, Name: "manual.pdf", Category: "manual", SizeBytes: 5}}
	got, err := SelectPS3ContentFiles(files)
	if err != nil || len(got) != 4 {
		t.Fatalf("manifest %+v %v", got, err)
	}
	base := OwnedROM{Snapshot: SnapshotsOf(files)}
	transfer, err := PS3TransferFiles(files, &base)
	if err != nil || len(transfer) != 3 || slices.ContainsFunc(transfer, ROMFile.IsGame) {
		t.Fatalf("transfer %+v %v", transfer, err)
	}
	system := System{Folder: "ps3"}
	if !UpdateAvailableBySystem(system, base.Snapshot, files) {
		t.Fatal("new PS3 attachments were ignored")
	}
	if UpdateAvailableBySystem(system, SnapshotsBySystem(system, files), files) {
		t.Fatal("installed attachments created a perpetual update")
	}
	if UpdateAvailableBySystem(System{Folder: "gba"}, base.Snapshot, files) {
		t.Fatal("PS3 support changed other systems' manifest")
	}
}

func TestPS3TransferRefusesUninstallOrReplacementDisguisedAsUpdate(t *testing.T) {
	files := []ROMFile{{ID: 1, Name: "Game.zip", SizeBytes: 100}, {ID: 2, Name: "patch.pkg", Category: "update", SizeBytes: 20}}
	old := OwnedROM{Snapshot: SnapshotsBySystem(System{Folder: "ps3"}, files)}
	for name, now := range map[string][]ROMFile{
		"removed attachment": files[:1],
		"changed attachment": {files[0], {ID: 2, Name: "patch.pkg", Category: "update", SizeBytes: 21}},
		"changed base":       {{ID: 1, Name: "Game.zip", SizeBytes: 101}, files[1]},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PS3TransferFiles(now, &old); err == nil {
				t.Fatal("replacement requires explicit removal/reinstall")
			}
		})
	}
}

func TestPS3SelectionPreservesDiscAssetsAndRejectsUnsafeAttachments(t *testing.T) {
	base := []ROMFile{{ID: 1, Name: "PARAM.SFO", Path: "PS3_GAME/PARAM.SFO"},
		{ID: 2, Name: "demo.pkg", Path: "PS3_GAME/PKGDIR/demo.pkg"}}
	if got, err := SelectPS3ContentFiles(base); err != nil || len(got) != 2 || PS3HasAttachments(got) {
		t.Fatalf("disc files %+v %v", got, err)
	}
	for _, bad := range []ROMFile{{ID: 3, Name: "../patch.pkg", Category: "update"}, {ID: 3, Name: "theme.p3t", Category: "dlc"}, {ID: 1, Name: "patch.pkg", Category: "update"}} {
		if _, err := SelectPS3ContentFiles(append(slices.Clone(base), bad)); err == nil {
			t.Fatalf("unsafe input accepted %+v", bad)
		}
	}
}
