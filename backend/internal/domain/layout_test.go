package domain

import (
	"path/filepath"
	"slices"
	"testing"
)

func files(paths ...string) []ROMFile {
	out := make([]ROMFile, 0, len(paths))
	for i, p := range paths {
		out = append(out, ROMFile{ID: i + 1, Name: filepath.Base(p), Path: p, SizeBytes: 10})
	}
	return out
}

func rels(l Layout) []string {
	var out []string
	for _, f := range l.Files {
		out = append(out, filepath.ToSlash(f.Rel))
	}
	return out
}

func TestPlanLayoutSingleFile(t *testing.T) {
	// A Switch game's base stays under the folder that will also hold updates and DLC.
	l, err := PlanLayout(system(t, "switch"), "Zelda", files("Zelda.nsp"))
	if err != nil || l.Root != "Zelda.nsp" || rels(l)[0] != "Zelda.nsp/Zelda.nsp" || l.Playlist != nil {
		t.Fatalf("%+v, %v", l, err)
	}
}

func TestPlanLayoutRejectsUnsafeSwitchLaunchPaths(t *testing.T) {
	for _, bad := range []string{"`", "|", "\r", "\n", "\t"} {
		t.Run(bad, func(t *testing.T) {
			name := "Bad" + bad + "Name.nsp"
			_, err := PlanLayout(system(t, "switch"), "Game", []ROMFile{{ID: 1, Name: name, Path: name, Category: "game"}})
			if err == nil {
				t.Fatalf("accepted unsafe launch path %q", name)
			}
		})
	}
	_, err := PlanLayout(system(t, "switch"), "Bad`Game", []ROMFile{{ID: 1, Name: "Game.rar", Path: "Game.rar", Category: "game"}})
	if err == nil {
		t.Fatal("accepted unsafe game root")
	}
}

func TestPlanLayoutSwitchContent(t *testing.T) {
	content := []ROMFile{
		{ID: 1, Name: "Mario.nsp", Path: "Mario.nsp", Category: "game"},
		{ID: 2, Name: "update.nsp", Path: "update/update.nsp", Category: "update"},
		{ID: 3, Name: "wave1.nsp", Path: "Wave 1/wave1.nsp", Category: "dlc"},
		{ID: 4, Name: "manual.pdf", Path: "manual.pdf", Category: "manual"},
	}
	l, err := PlanLayout(system(t, "switch"), "Mario Kart", content)
	if err != nil {
		t.Fatal(err)
	}
	if l.Root != "Mario.nsp" || !slices.Equal(rels(l), []string{
		"Mario.nsp/Mario.nsp", "Mario.nsp/update/update.nsp", "Mario.nsp/dlc/Wave 1/wave1.nsp",
	}) {
		t.Fatalf("layout %+v, rels %q", l, rels(l))
	}
}

func TestPlanLayoutCanonicalizesSwitchCategoryPrefix(t *testing.T) {
	l, err := PlanLayout(system(t, "switch"), "Mario Kart", []ROMFile{
		{ID: 1, Name: "Mario.nsp", Path: "Mario.nsp", Category: "game"},
		{ID: 2, Name: "4.0.1.nsp", Path: "Update/4.0.1.nsp", Category: "update"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(l); len(got) != 2 || got[1] != "Mario.nsp/update/4.0.1.nsp" {
		t.Fatalf("category path wasn't canonicalized: %q", got)
	}
}

func TestPlanLayoutRejectsNormalizedAndParentPathCollisions(t *testing.T) {
	for _, test := range []struct {
		name  string
		files []ROMFile
	}{
		{
			name: "category normalization",
			files: []ROMFile{
				{ID: 1, Name: "Game.nsp", Path: "Game.nsp", Category: "game"},
				{ID: 2, Name: "a.nsp", Path: "a.nsp", Category: "update"},
				{ID: 3, Name: "a.nsp", Path: "Update/a.nsp", Category: "update"},
			},
		},
		{
			name: "file parent conflict",
			files: []ROMFile{
				{ID: 1, Name: "Game.nsp", Path: "Game.nsp", Category: "game"},
				{ID: 2, Name: "a.nsp", Path: "update/a.nsp", Category: "update"},
				{ID: 3, Name: "child.nsp", Path: "update/a.nsp/child.nsp", Category: "update"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := PlanLayout(system(t, "switch"), "Game", test.files); err == nil {
				t.Fatal("accepted overlapping Switch paths")
			}
		})
	}
}

func TestPlanLayoutPlaylist(t *testing.T) {
	l, err := PlanLayout(system(t, "psx"), "Final Fantasy VII (USA)", files(
		"Final Fantasy VII (USA) (Disc 3).cue", "Final Fantasy VII (USA) (Disc 3).bin",
		"Final Fantasy VII (USA) (Disc 1).cue", "Final Fantasy VII (USA) (Disc 1).bin",
		"Final Fantasy VII (USA) (Disc 2).cue", "Final Fantasy VII (USA) (Disc 2).bin",
		"Final Fantasy VII (USA).m3u",
	))
	if err != nil {
		t.Fatal(err)
	}
	if l.Root != "Final Fantasy VII (USA).m3u" || len(l.Files) != 6 {
		t.Fatalf("%+v", l)
	}
	// The Server's own playlist is dropped for the plugin's, which sits in the folder of its name.
	if got := filepath.ToSlash(l.Playlist.Rel); got != "Final Fantasy VII (USA).m3u/Final Fantasy VII (USA).m3u" {
		t.Fatalf("playlist at %s", got)
	}
	want := []string{"Final Fantasy VII (USA) (Disc 1).cue", "Final Fantasy VII (USA) (Disc 2).cue", "Final Fantasy VII (USA) (Disc 3).cue"}
	if !slices.Equal(l.Playlist.Lines, want) {
		t.Fatalf("lines %q", l.Playlist.Lines)
	}
}

func TestPlanLayoutGDI(t *testing.T) {
	l, err := PlanLayout(system(t, "dc"), "Shenmue", files("disc.gdi", "track01.bin", "track02.raw", "track03.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.Playlist.Lines, []string{"disc.gdi"}) || rels(l)[1] != "Shenmue.m3u/track01.bin" {
		t.Fatalf("%+v", l)
	}
}

func TestPlanLayoutFolders(t *testing.T) {
	l, err := PlanLayout(system(t, "ps3"), "Demon's Souls", files("PS3_GAME/PARAM.SFO", "PS3_GAME/USRDIR/EBOOT.BIN"))
	if err != nil || l.Root != "Demon's Souls.ps3" || rels(l)[1] != "Demon's Souls.ps3/PS3_GAME/USRDIR/EBOOT.BIN" {
		t.Fatalf("ps3: %+v, %v", l, err)
	}
	l, err = PlanLayout(system(t, "wiiu"), "Mario Kart 8", files("code/app.xml", "content/a.bin", "meta/meta.xml"))
	if err != nil || l.Root != "Mario Kart 8" || rels(l)[0] != "Mario Kart 8/code/app.xml" {
		t.Fatalf("wiiu: %+v, %v", l, err)
	}
	l, err = PlanLayout(system(t, "dos"), "Doom", files("DOOM.EXE", "DOOM1.WAD"))
	if err != nil || l.Root != "Doom.zip" || !l.Zip || rels(l)[0] != "DOOM.EXE" {
		t.Fatalf("dos: %+v, %v", l, err)
	}
}

func TestPlanLayoutRefusals(t *testing.T) {
	if _, err := PlanLayout(system(t, "snes"), "Two", files("a.sfc", "b.sfc")); err == nil {
		t.Fatal("placed several files where there's no layout")
	}
	if _, err := PlanLayout(system(t, "psx"), "Escape", files("../x.cue", "x.bin")); err == nil {
		t.Fatal("accepted a path outside the game")
	}
	if _, err := PlanLayout(system(t, "psx"), "No discs", files("readme.txt", "notes.txt")); err != nil {
		t.Fatalf("files that aren't tracks are discs of their own: %v", err)
	}
}

func TestDiscOrder(t *testing.T) {
	got := Discs(files("Game (Disc B).chd", "Game (Disc 1 of 2).chd", "Extra.chd", "Game (CD 2A).chd"))
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
	}
	want := []string{"Game (Disc 1 of 2).chd", "Game (CD 2A).chd", "Game (Disc B).chd", "Extra.chd"}
	if !slices.Equal(names, want) {
		t.Fatalf("order %q", names)
	}
}
