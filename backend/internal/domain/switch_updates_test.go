package domain

import (
	"fmt"
	"slices"
	"testing"
)

func patchFile(id int, raw uint32, ext string) ROMFile {
	name := fmt.Sprintf("Mario Kart - Update readable [0100152000022800][v%d].%s", raw, ext)
	return ROMFile{ID: id, Name: name, Path: "update/" + name, Category: "update", SizeBytes: int64(id)}
}
func switchBase() ROMFile {
	return ROMFile{ID: 1, Name: "Mario Kart [0100152000022000][v0].nsp", Category: "game"}
}
func ids(files []ROMFile) []int {
	var out []int
	for _, f := range files {
		out = append(out, f.ID)
	}
	return out
}

func TestSelectSwitchLatestPatchAndEveryDLC(t *testing.T) {
	base := switchBase()
	old, latest, oldest := patchFile(2, 1441792, "rar"), patchFile(3, 1507328, "rar"), patchFile(4, 1376256, "nsp")
	files := []ROMFile{base, old, latest, oldest, {ID: 5, Name: "wave1.nsp", Category: "DLC"}, {ID: 6, Name: "wave2.nsp", Category: "dlc"}, {ID: 7, Category: "manual"}}
	selected, err := SelectContentFiles(files)
	if err != nil || !slices.Equal(ids(selected), []int{1, 3, 5, 6}) {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	// Adding another historical version has no effect on the installed snapshot.
	snapshot := ContentSnapshotsOf(files)
	files = append(files, patchFile(8, 65536, "nsp"))
	if ContentUpdateAvailable(snapshot, files) {
		t.Fatal("a historical patch caused a perpetual update")
	}
}
func TestCanonicalPatchTagsOverrideScannerNCAMetadata(t *testing.T) {
	old, latest := patchFile(2, 65536, "nsp"), patchFile(3, 131072, "nsp")
	latest.TitleID, latest.TitleVersion = "0179F08B812296D1", "0"
	selected, err := SelectContentFiles([]ROMFile{switchBase(), old, latest})
	if err != nil || !slices.Equal(ids(selected), []int{1, 3}) {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}
func TestSwitchPatchMetadataFallback(t *testing.T) {
	base := ROMFile{ID: 1, Name: "base.nsp", TitleID: "0100152000022000", Category: "game"}
	old := ROMFile{ID: 2, Name: "old.nsp", TitleID: "0100152000022800", TitleVersion: "v65536", Category: "update"}
	latest := old
	latest.ID = 3
	latest.Name = "latest.nsp"
	latest.TitleVersion = "131072"
	selected, err := SelectContentFiles([]ROMFile{base, old, latest})
	if err != nil || !slices.Equal(ids(selected), []int{1, 3}) {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}
func TestSwitchPatchKeepsAllLatestRARVolumes(t *testing.T) {
	first := patchFile(3, 131072, "part1.rar")
	second := patchFile(4, 131072, "part2.rar")
	selected, err := SelectContentFiles([]ROMFile{switchBase(), patchFile(2, 65536, "nsp"), second, first})
	if err != nil || !slices.Equal(ids(selected), []int{1, 4, 3}) {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	third := patchFile(5, 131072, "part3.rar")
	if _, err := SelectContentFiles([]ROMFile{switchBase(), first, third}); err == nil {
		t.Fatal("accepted a missing RAR volume")
	}
}
func TestSwitchPatchPrefersUncompressedEquivalentWithoutDoubleDownload(t *testing.T) {
	selected, err := SelectContentFiles([]ROMFile{switchBase(), patchFile(2, 131072, "rar"), patchFile(3, 131072, "nsp")})
	if err != nil || !slices.Equal(ids(selected), []int{1, 3}) {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
	duplicate := patchFile(4, 131072, "nsp")
	duplicate.Name = "Other " + duplicate.Name
	duplicate.Path = "update/" + duplicate.Name
	if _, err := SelectContentFiles([]ROMFile{switchBase(), patchFile(3, 131072, "nsp"), duplicate}); err == nil {
		t.Fatal("arbitrarily chose equally preferred copies")
	}
}
func TestSwitchPatchRejectsForeignTitleAndAmbiguousLegacyCandidates(t *testing.T) {
	foreign := patchFile(2, 131072, "nsp")
	foreign.Name = "Other [01007EF00011E800][v131072].nsp"
	for _, files := range [][]ROMFile{{switchBase(), foreign}, {switchBase(), {ID: 2, Name: "unknown.nsp", Category: "update"}, patchFile(3, 131072, "nsp")}} {
		if _, err := SelectContentFiles(files); err == nil {
			t.Fatalf("accepted ambiguous patch %+v", files)
		}
		// Comparison callers must not turn errors into an empty manifest/purge.
		if len(ContentFiles(files)) == 0 {
			t.Fatal("ambiguous metadata erased the comparison manifest")
		}
	}
	single := []ROMFile{switchBase(), {ID: 2, Name: "only-patch.nsp", Category: "update"}}
	if selected, err := SelectContentFiles(single); err != nil || len(selected) != 2 {
		t.Fatalf("single legacy update required no ordering: %+v %v", selected, err)
	}
}
func TestSwitchPatchRetiresOnlyOldOwnedIDsWithoutRedownload(t *testing.T) {
	base, old, latest := switchBase(), patchFile(2, 65536, "nsp"), patchFile(3, 131072, "nsp")
	dlc := ROMFile{ID: 4, Name: "wave.nsp", Category: "dlc"}
	snapshot := []FileSnapshot{SnapshotOf(base), SnapshotOf(old), SnapshotOf(latest), SnapshotOf(dlc)}
	files := []ROMFile{base, old, latest, dlc}
	changed, removed := ChangedContent(snapshot, files)
	if len(changed) != 0 || len(removed) != 1 || !removed[old.ID] {
		t.Fatalf("download=%+v removed=%v", changed, removed)
	}
	if !ContentUpdateAvailable(snapshot, files) || ContentUpdateAvailable(ContentSnapshotsOf(files), files) {
		t.Fatal("retirement did not converge")
	}
}

func TestChangedSwitchUpdateVolumeSelectsWholeCohort(t *testing.T) {
	base := switchBase()
	first, second := patchFile(2, 131072, "part1.rar"), patchFile(3, 131072, "part2.rar")
	snapshot := ContentSnapshotsOf([]ROMFile{base, first, second})
	second.SizeBytes++
	changed, retired := ChangedContent(snapshot, []ROMFile{base, first, second})
	if !slices.Equal(ids(changed), []int{2, 3}) || !retired[2] || !retired[3] || retired[base.ID] {
		t.Fatalf("incomplete cohort downloaded=%+v retired=%v", changed, retired)
	}
}
