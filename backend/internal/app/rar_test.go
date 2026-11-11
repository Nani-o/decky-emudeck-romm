package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
	"github.com/nwaples/rardecode/v2"
)

// Tiny synthetic, uncompressed RAR4 fixtures generated entirely from strings.
// No copyrighted game data or third-party fixture assets are included.
func rarBlock(kind byte, flags uint16, payload []byte) []byte {
	var body bytes.Buffer
	body.WriteByte(kind)
	_ = binary.Write(&body, binary.LittleEndian, flags)
	_ = binary.Write(&body, binary.LittleEndian, uint16(7+len(payload)))
	body.Write(payload)
	var out bytes.Buffer
	_ = binary.Write(&out, binary.LittleEndian, uint16(crc32.ChecksumIEEE(body.Bytes())))
	out.Write(body.Bytes())
	return out.Bytes()
}

func storedRAR(name string, all, part []byte, number, total int, extraFlags uint16) []byte {
	out := append([]byte{}, []byte("Rar!\x1a\x07\x00")...)
	arcFlags := uint16(0)
	if total > 1 {
		arcFlags = 0x0011
	}
	out = append(out, rarBlock(0x73, arcFlags, make([]byte, 6))...)
	flags := uint16(0x8000) | extraFlags
	if number > 1 {
		flags |= 1
	}
	if number < total {
		flags |= 2
	}
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.LittleEndian, uint32(len(part)))
	_ = binary.Write(&payload, binary.LittleEndian, uint32(len(all)))
	payload.WriteByte(3) // Unix host identifier in RAR4.
	sum := crc32.ChecksumIEEE(all)
	if number < total {
		sum = crc32.ChecksumIEEE(part)
	}
	_ = binary.Write(&payload, binary.LittleEndian, sum)
	_ = binary.Write(&payload, binary.LittleEndian, uint32(0))
	payload.WriteByte(20)
	payload.WriteByte(0x30)
	_ = binary.Write(&payload, binary.LittleEndian, uint16(len(name)))
	_ = binary.Write(&payload, binary.LittleEndian, uint32(0100644))
	payload.WriteString(name)
	out = append(out, rarBlock(0x74, flags, payload.Bytes())...)
	out = append(out, part...)
	end := uint16(0)
	if number < total {
		end = 1
	}
	return append(out, rarBlock(0x7b, end, nil)...)
}

func switchFixture(t *testing.T) *downloadFixture {
	t.Helper()
	f := newDownloadFixture(t, 2)
	err := f.catalog.Apply(context.Background(), domain.CatalogSync{
		Platforms: []domain.Platform{{ID: 13, Slug: "switch", Name: "Switch"}},
		ROMs:      []domain.ROM{{ID: 13, PlatformID: 13, Name: "Example", FileName: "Example [0100000000000000]", MultiFile: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	state := f.downloads.install.State(false)
	state.Systems = append(state.Systems, domain.SystemState{Folder: "switch", ROMPath: filepath.Join(f.root, "roms/switch"), Emulator: "ryujinx"})
	f.downloads.install.mu.Lock()
	f.downloads.install.prober = fixedProber{state}
	f.downloads.install.last = nil
	f.downloads.install.mu.Unlock()
	return f
}

func TestGroupedSwitchDownloadsEveryContentCategory(t *testing.T) {
	f := switchFixture(t)
	f.serveSet(13, gameFile{"Example.nsp", "game", "base"}, gameFile{"update/Update.nsp", "update", "update"}, gameFile{"dlc/Bonus.nsp", "dlc", "dlc"})
	rec := f.installed(t, 13)
	root := filepath.Join(f.root, "roms/switch", "Example.nsp")
	base := filepath.Join(root, "Example.nsp")
	if len(rec.Files) != 5 || len(rec.Snapshot) != 3 || readFile(t, base) != "base" ||
		readFile(t, filepath.Join(root, "update", "Update.nsp")) != "update" || readFile(t, filepath.Join(root, "dlc", "Bonus.nsp")) != "dlc" {
		t.Fatalf("record %+v", rec)
	}
	if len(f.server.froms) != 3 {
		t.Fatalf("didn't download all content files: %+v", f.server.froms)
	}
}

func TestSwitchArchiveRejectsUnsafeLaunchMemberBeforePlacement(t *testing.T) {
	f := switchFixture(t)
	f.serveSet(13, gameFile{"Example.zip", "game", string(zipOf(t, member{"Bad`Name.nsp", "synthetic nsp"}))})
	if _, err := f.downloads.Queue(context.Background(), 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateFailed || kindOf(u.Problem) != domain.KindCantDownload {
		t.Fatalf("unsafe extracted game was not rejected: %+v", u)
	}
	if got := listDir(t, filepath.Join(f.root, "roms/switch")); len(got) != 0 {
		t.Fatalf("placed unsafe game: %v", got)
	}
}

func TestSwitchKeepsFilesFromTwoDLCArchives(t *testing.T) {
	f := switchFixture(t)
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base"},
		gameFile{"dlc/pack-one.zip", "dlc", string(zipOf(t, member{"Wave One.nsp", "wave one"}))},
		gameFile{"dlc/pack-two.zip", "dlc", string(zipOf(t, member{"Wave Two.nsp", "wave two"}))},
	)
	f.installed(t, 13)
	root := filepath.Join(f.root, "roms", "switch", "Mario Kart 8 Deluxe.nsp")
	if readFile(t, filepath.Join(root, "dlc", "Wave One.nsp")) != "wave one" ||
		readFile(t, filepath.Join(root, "dlc", "Wave Two.nsp")) != "wave two" {
		t.Fatalf("one archive removed the other's extraction: %v", listDir(t, root))
	}
}

func TestSwitchRejectsCollidingDLCArchiveOutputsBeforePlacement(t *testing.T) {
	f := switchFixture(t)
	f.serveSet(13,
		gameFile{"Mario Kart 8 Deluxe.nsp", "game", "base"},
		gameFile{"dlc/pack-one.zip", "dlc", string(zipOf(t, member{"same.nsp", "wave one"}))},
		gameFile{"dlc/pack-two.zip", "dlc", string(zipOf(t, member{"same.nsp", "wave two"}))},
	)
	if _, err := f.downloads.Queue(context.Background(), 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateFailed || kindOf(u.Problem) != domain.KindCantDownload {
		t.Fatalf("colliding DLC archives were accepted: %+v", u)
	}
	if got := listDir(t, filepath.Join(f.root, "roms", "switch")); len(got) != 0 {
		t.Fatalf("published part of a colliding update: %v", got)
	}
}

func TestSwitchSingleRARExtractedToPlayableFile(t *testing.T) {
	f := switchFixture(t)
	rar := storedRAR("Example.nsp", []byte("synthetic nsp"), []byte("synthetic nsp"), 1, 1, 0)
	f.serveSet(13, gameFile{"Example.rar", "game", string(rar)})
	rec := f.installed(t, 13)
	root := filepath.Join(f.root, "roms/switch", "Example [0100000000000000]")
	if len(rec.Files) != 1 || len(rec.Snapshot) != 1 || readFile(t, filepath.Join(root, "Example.nsp")) != "synthetic nsp" {
		t.Fatalf("record %+v", rec)
	}
	if got := listDir(t, filepath.Join(f.root, "roms/switch")); len(got) != 1 || got[0] != filepath.Join("Example [0100000000000000]", "Example.nsp") {
		t.Fatal(got)
	}
}

func TestSwitchThreePartRARExtractsAndSnapshotsEveryVolume(t *testing.T) {
	f := switchFixture(t)
	all := []byte("synthetic multi volume nsp")
	a, b, c := all[:7], all[7:14], all[14:]
	f.serveSet(13,
		gameFile{"Example.part3.rar", "game", string(storedRAR("Example.nsp", all, c, 3, 3, 0))},
		gameFile{"Example.part1.rar", "game", string(storedRAR("Example.nsp", all, a, 1, 3, 0))},
		gameFile{"Example.part2.rar", "game", string(storedRAR("Example.nsp", all, b, 2, 3, 0))},
		gameFile{"dlc/Bonus.nsp", "dlc", "not fetched"},
	)
	rec := f.installed(t, 13)
	root := filepath.Join(f.root, "roms/switch", "Example [0100000000000000]")
	if len(rec.Files) != 3 || len(rec.Snapshot) != 4 || readFile(t, filepath.Join(root, "Example.nsp")) != string(all) ||
		readFile(t, filepath.Join(root, "dlc", "Bonus.nsp")) != "not fetched" {
		t.Fatalf("record %+v", rec)
	}
	if len(f.server.froms) != 4 {
		t.Fatalf("requested %+v", f.server.froms)
	}
}

func TestMultipartRARRejectsGapsMixedAndAmbiguousSets(t *testing.T) {
	for _, names := range [][]string{{"G.part1.rar", "G.part3.rar"}, {"G.part1.rar", "H.part2.rar"}, {"G.part1.rar", "G.part01.rar"}, {"G.part1.rar", "manual.nfo"}, {"G.rar", "H.rar"}} {
		t.Run(strings.Join(names, "+"), func(t *testing.T) {
			var files []domain.ROMFile
			for _, n := range names {
				files = append(files, domain.ROMFile{Name: n, Path: n})
			}
			if _, err := multipartRAR(files); err == nil {
				t.Fatal("accepted ambiguous volume set")
			}
		})
	}
}

func TestRARRejectsMissingVolumeBeforePlacement(t *testing.T) {
	f := switchFixture(t)
	all := []byte("synthetic nsp")
	f.serveSet(13, gameFile{"Example.part1.rar", "game", string(storedRAR("Example.nsp", all, all[:3], 1, 2, 0))})
	if _, err := f.downloads.Queue(context.Background(), 13); err != nil {
		t.Fatal(err)
	}
	if u := f.end(t, 13); u.State != domain.StateFailed {
		t.Fatalf("incomplete multipart archive installed: %+v", u)
	}
	if got := listDir(t, filepath.Join(f.root, "roms/switch")); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestRARVolumeFilesystemRejectsUndownloadedSiblingAndLinks(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "Example.rar")
	other := filepath.Join(dir, "unowned.rar")
	if err := os.WriteFile(first, []byte("a"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("b"), 0600); err != nil {
		t.Fatal(err)
	}
	v, err := newRARVolumeFS(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer v.root.Close()
	if _, err := v.Open("unowned.rar"); err == nil {
		t.Fatal("opened an unowned sibling")
	}
	link := filepath.Join(dir, "linked.rar")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if _, err := newRARVolumeFS(link, nil); err == nil {
		t.Fatal("accepted linked volume")
	}
}

func TestRARUnsafeHeadersRejected(t *testing.T) {
	for _, header := range []rardecode.FileHeader{
		{Name: "../escape.nsp"}, {Name: "/absolute.nsp"}, {Name: `C:\\escape.nsp`}, {Name: "unsafe`name.nsp"},
		{Name: "g.nsp", Encrypted: true}, {Name: "g.nsp", HeaderEncrypted: true}, {Name: "g.nsp", LinkType: 1, LinkTarget: "outside"},
		{Name: "g.nsp", UnKnownSize: true}, {Name: "g.nsp", UnPackedSize: -1},
	} {
		if _, err := rarMemberPath(header); err == nil {
			t.Fatalf("accepted %+v", header)
		}
	}
}

func TestRARTraversalAndEncryptedArchivesCannotExtract(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags uint16
	}{{"../escape.nsp", 0}, {"g.nsp", 4}} {
		dir := t.TempDir()
		p := filepath.Join(dir, "Example.rar")
		raw := storedRAR(test.name, []byte("data"), []byte("data"), 1, 1, test.flags)
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openArchive(p); err == nil {
			t.Fatal("unsafe archive accepted")
		}
	}
}

func TestRARRetainsChecksumValidation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "Example.rar")
	raw := storedRAR("g.nsp", []byte("data"), []byte("data"), 1, 1, 0)
	raw[len(raw)-8] ^= 1
	if err := os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	members, close, err := openArchive(p)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	r, err := members[0].open()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("corrupted payload checksum accepted")
	}
}

func TestSolidRARMembersUseSequentialReader(t *testing.T) {
	first := storedRAR("LICENSE", []byte("license"), []byte("license"), 1, 1, 0)
	second := storedRAR("Game.nsp", []byte("data"), []byte("data"), 1, 1, 0x10)
	raw := append(append([]byte{}, first[:len(first)-7]...), second[20:]...)
	dir := t.TempDir()
	path := filepath.Join(dir, "Example.rar")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	members, close, err := openArchive(path)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	if len(members) != 2 {
		t.Fatalf("members %d", len(members))
	}
	for index, want := range []string{"license", "data"} {
		r, err := members[index].open()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || string(got) != want {
			t.Fatalf("member %d=%q, %v", index, got, err)
		}
	}
}

func TestRARRejectsDuplicateAndConflictingMemberPaths(t *testing.T) {
	for _, pair := range [][2]string{{"Game.nsp", "game.nsp"}, {"dir", "dir/Game.nsp"}} {
		first := storedRAR(pair[0], []byte("a"), []byte("a"), 1, 1, 0)
		second := storedRAR(pair[1], []byte("b"), []byte("b"), 1, 1, 0)
		raw := append(append([]byte{}, first[:len(first)-7]...), second[20:]...)
		p := filepath.Join(t.TempDir(), "Example.rar")
		if err := os.WriteFile(p, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openArchive(p); err == nil {
			t.Fatalf("accepted %v", pair)
		}
	}
}

func TestRARDownloadDoesNotOverwriteUserGame(t *testing.T) {
	f := switchFixture(t)
	target := filepath.Join(f.root, "roms/switch", "Example [0100000000000000]", "Example.nsp")
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("user-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	data := storedRAR("Example.nsp", []byte("new game"), []byte("new game"), 1, 1, 0)
	f.serveSet(13, gameFile{"Example.rar", "game", string(data)})
	if _, err := f.downloads.Queue(context.Background(), 13); kindOf(err) != domain.KindTargetExists {
		t.Fatalf("queue over user content: %v", err)
	}
	if readFile(t, target) != "user-owned" {
		t.Fatal("replaced the user's game")
	}
}

func TestRARRejectsExtraDownloadedVolumeNotInArchive(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "Example.part1.rar")
	second := filepath.Join(dir, "Example.part2.rar")
	if err := os.WriteFile(first, storedRAR("Game.nsp", []byte("data"), []byte("data"), 1, 1, 0), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openArchive(first, first, second); err == nil {
		t.Fatal("accepted an unused volume")
	}
}

func TestRARVolumesRejectHardlinks(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.WriteFile(outside, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(dir, "Example.rar")
	if err := os.Link(outside, first); err != nil {
		t.Fatal(err)
	}
	if _, err := newRARVolumeFS(first, nil); err == nil {
		t.Fatal("accepted a shared inode")
	}
}

func TestMultipartRARAcceptsUnpaddedDoubleDigitNumbers(t *testing.T) {
	files := make([]domain.ROMFile, 10)
	for i := range files {
		name := fmt.Sprintf("Game.part%d.rar", i+1)
		files[i] = domain.ROMFile{Name: name, Path: name}
	}
	if first, err := multipartRAR(files); err != nil || first != 0 {
		t.Fatalf("first %d, %v", first, err)
	}
}

func TestMultipartRARAcceptsShuffledPaddedDoubleDigitNumbers(t *testing.T) {
	order := []int{10, 2, 7, 1, 8, 3, 9, 5, 6, 4}
	files := make([]domain.ROMFile, len(order))
	for i, n := range order {
		name := fmt.Sprintf("Game.part%02d.rar", n)
		files[i] = domain.ROMFile{Name: name, Path: name}
	}
	if first, err := multipartRAR(files); err != nil || first != 3 {
		t.Fatalf("first %d, %v", first, err)
	}
}
