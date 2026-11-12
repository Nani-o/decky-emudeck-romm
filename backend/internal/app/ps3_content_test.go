package app

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

const discContentID = "EP0001-BCES00001_00-TESTCONTENT00001"
const discDLCContentID = "EP0001-BCES00001_00-TESTCONTENT00002"

type ps3ContentInstaller struct {
	baseCalls     int
	extraCalls    int
	preparedCalls int
	removed       bool
	nativeOwned   bool
	extra         func(context.Context, domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error)
	base          func(context.Context, int, string) (domain.PS3InstallResult, error)
	prepare       func(context.Context, int, string) (domain.PS3InstallResult, bool, error)
	identity      func(context.Context, int) (domain.PS3Identity, error)
}

func (m *ps3ContentInstaller) Install(ctx context.Context, id int, dir string) (domain.PS3InstallResult, error) {
	m.baseCalls++
	if m.base != nil {
		return m.base(ctx, id, dir)
	}
	return ps3Result(id), nil
}

func (m *ps3ContentInstaller) PreparedBase(ctx context.Context, id int, dir string) (domain.PS3InstallResult, bool, error) {
	m.preparedCalls++
	if m.prepare != nil {
		return m.prepare(ctx, id, dir)
	}
	return domain.PS3InstallResult{}, false, nil
}

func (m *ps3ContentInstaller) BaseIdentity(ctx context.Context, id int) (domain.PS3Identity, error) {
	if m.identity != nil {
		return m.identity(ctx, id)
	}
	return ps3Result(11).Identity, nil
}

func (m *ps3ContentInstaller) InstallExtras(ctx context.Context, r domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error) {
	m.extraCalls++
	if m.extra != nil {
		return m.extra(ctx, r)
	}
	var ids []int
	for _, p := range r.Packages {
		ids = append(ids, p.SourceFileID)
	}
	return domain.PS3ExtrasResult{TitleID: r.TitleID, AppliedSourceFileIDs: ids, VerifiedAt: time.Now()}, nil
}

func (m *ps3ContentInstaller) CheckRemoval(context.Context, int) (bool, error) {
	return m.nativeOwned, nil
}

func (m *ps3ContentInstaller) Remove(context.Context, int) (bool, error) {
	m.removed = true
	return true, nil
}

func testPS3SFO(title string) string {
	const key = "TITLE_ID\x00"
	b := make([]byte, 20+16+len(key)+len(title)+1)
	copy(b, "\x00PSF")
	binary.LittleEndian.PutUint32(b[8:], 36)
	binary.LittleEndian.PutUint32(b[12:], uint32(36+len(key)))
	binary.LittleEndian.PutUint32(b[16:], 1)
	binary.LittleEndian.PutUint16(b[22:], 0x204)
	binary.LittleEndian.PutUint32(b[24:], uint32(len(title)+1))
	binary.LittleEndian.PutUint32(b[28:], uint32(len(title)+1))
	copy(b[36:], key)
	copy(b[36+len(key):], title)
	return string(b)
}

func testPS3Package(content string) string {
	b := make([]byte, 96)
	copy(b, "\x7fPKG")
	copy(b[48:], content)
	return string(b)
}

func appendPS3Attachment(f *downloadFixture, id int, category, name, content string) {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	f.server.files[11] = append(f.server.files[11], domain.ROMFile{ID: id, Name: name, Path: category + "/" + name, Category: category, SizeBytes: int64(len(content))})
	f.server.content[id] = []byte(content)
}

func servePS3Disc(t *testing.T, f *downloadFixture) {
	t.Helper()
	f.serveSet(11, gameFile{"PS3_GAME/PARAM.SFO", "game", testPS3SFO("BCES00001")},
		gameFile{"PS3_GAME/USRDIR/EBOOT.BIN", "game", "disc boot"},
		gameFile{"PS3_GAME/PKGDIR/other-title.pkg", "game", "opaque unrelated demo"})
}

func TestPS3DiscAttachmentsInstallPrivatelyAndPreviewIncludesEverySource(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Disc(t, f)
	appendPS3Attachment(f, 5001, "update", "patch.pkg", testPS3Package(discContentID))
	appendPS3Attachment(f, 5002, "dlc", "pack.zip", string(zipOf(t, member{"addon.pkg", testPS3Package(discDLCContentID)}, member{discDLCContentID + ".rap", "0123456789abcdef"})))
	m := &ps3ContentInstaller{extra: func(_ context.Context, r domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error) {
		if !r.DiscBase || r.TitleID != "BCES00001" || len(r.Packages) != 2 || filepath.Base(r.BaseSFOPath) != "PARAM.SFO" {
			t.Fatalf("request %+v", r)
		}
		for _, p := range r.Packages {
			if !strings.Contains(p.Directory, ".ps3-extras") {
				t.Fatal("not private", p)
			}
		}
		return domain.PS3ExtrasResult{TitleID: r.TitleID, AppliedSourceFileIDs: []int{5001, 5002}, VerifiedAt: time.Now()}, nil
	}}
	f.downloads.InstallPS3PackagesWith(m)
	plan, err := f.downloads.Plan(context.Background(), 11)
	if err != nil || plan.FileCount != 5 {
		t.Fatalf("plan %+v %v", plan, err)
	}
	rec := f.installed(t, 11)
	if m.baseCalls != 0 || m.extraCalls != 1 || len(rec.Files) != 3 || len(rec.Snapshot) != 5 {
		t.Fatalf("calls %d/%d rec %+v", m.baseCalls, m.extraCalls, rec)
	}
	for _, file := range rec.Files {
		if strings.Contains(file.Path, "patch.pkg") || strings.Contains(file.Path, "addon.pkg") || strings.HasSuffix(file.Path, ".rap") {
			t.Fatalf("installer payload published %+v", file)
		}
	}
	if !slices.ContainsFunc(rec.Files, func(p domain.PlacedFile) bool { return strings.Contains(p.Path, "other-title.pkg") }) {
		t.Fatal("embedded disc assets disappeared")
	}
}

func TestPS3ExtrasOnlyUpdatePreservesBaseAndDoesNotFetchIt(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Disc(t, f)
	old := f.installed(t, 11)
	for len(f.server.started) > 0 {
		<-f.server.started
	}
	appendPS3Attachment(f, 5001, "update", "patch.pkg", testPS3Package(discContentID))
	m := &ps3ContentInstaller{}
	f.downloads.InstallPS3PackagesWith(m)
	if err := f.owned.SetUpdateAvailable(context.Background(), 11, true); err != nil {
		t.Fatal(err)
	}
	plan, err := f.downloads.Plan(context.Background(), 11)
	if err != nil || plan.FileCount != 1 || plan.TotalBytes != 96 {
		t.Fatalf("delta preview %+v %v", plan, err)
	}
	rec := f.installed(t, 11)
	if !slices.Equal(old.Files, rec.Files) || len(rec.Snapshot) != 4 || m.baseCalls != 0 || m.extraCalls != 1 {
		t.Fatalf("base replaced or lost: %+v", rec)
	}
	for len(f.server.started) > 0 {
		if got := <-f.server.started; got != 5001 {
			t.Fatalf("base fetched again: %d", got)
		}
	}
}

func TestPS3PSNBaseAndExtraCreatesOnlyOneLauncher(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Bundle(t, f)
	appendPS3Attachment(f, 5001, "update", "patch.pkg", testPS3Package(ps3Result(11).Identity.ContentID))
	m := &ps3ContentInstaller{}
	f.downloads.InstallPS3PackagesWith(m)
	rec := f.installed(t, 11)
	if m.baseCalls != 1 || m.extraCalls != 1 || len(rec.Files) != 1 || !strings.HasSuffix(rec.Files[0].Path, ".desktop") {
		t.Fatalf("unexpected install %+v calls %d/%d", rec, m.baseCalls, m.extraCalls)
	}
}

func TestPS3InitialPSNRetryResumesExtrasBeforePublishingOwnership(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Bundle(t, f)
	appendPS3Attachment(f, 5001, "update", "patch.pkg", testPS3Package(ps3Result(11).Identity.ContentID))
	pending := false
	resumed := false
	m := &ps3ContentInstaller{}
	m.base = func(_ context.Context, id int, _ string) (domain.PS3InstallResult, error) {
		if pending {
			return domain.PS3InstallResult{}, errors.New("ordinary Install refuses pending overlay")
		}
		return ps3Result(id), nil
	}
	m.prepare = func(_ context.Context, id int, dir string) (domain.PS3InstallResult, bool, error) {
		if !pending {
			return domain.PS3InstallResult{}, false, nil
		}
		if _, ok, _ := f.owned.Owned(context.Background(), id); ok {
			t.Fatal("pending preparation already published ownership")
		}
		if _, err := os.Stat(filepath.Join(dir, "opaque.pkg")); err != nil {
			t.Fatal("retry did not bind its original private base", err)
		}
		return ps3Result(id), true, nil
	}
	m.extra = func(_ context.Context, r domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error) {
		if !pending {
			pending = true
			return domain.PS3ExtrasResult{}, errors.New("interrupted native overlay")
		}
		if _, ok, _ := f.owned.Owned(context.Background(), r.ROMID); ok {
			t.Fatal("retry became Ready before the overlay resumed")
		}
		resumed, pending = true, false
		return domain.PS3ExtrasResult{TitleID: r.TitleID, AppliedSourceFileIDs: []int{5001}, VerifiedAt: time.Now()}, nil
	}
	f.downloads.InstallPS3PackagesWith(m)
	if _, err := f.downloads.Queue(context.Background(), 11); err != nil {
		t.Fatal(err)
	}
	if end := f.end(t, 11); end.State != domain.StateFailed {
		t.Fatal("interrupted overlay was reported Ready", end)
	}
	if _, ok, _ := f.owned.Owned(context.Background(), 11); ok {
		t.Fatal("failed overlay acquired ownership")
	}
	rec := f.installed(t, 11)
	if !resumed || m.baseCalls != 1 || m.preparedCalls != 2 || m.extraCalls != 2 || len(rec.Snapshot) != 2 {
		t.Fatalf("base install blocked retry or extras skipped: %+v %d/%d/%d", rec, m.baseCalls, m.preparedCalls, m.extraCalls)
	}
}

func TestPS3ExtrasOnlyPSNRetryRoutesPendingIdentityIntoInstallExtras(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Bundle(t, f)
	pending, resumed := false, false
	pendingIdentityCalls := 0
	m := &ps3ContentInstaller{}
	m.identity = func(_ context.Context, id int) (domain.PS3Identity, error) {
		if pending {
			pendingIdentityCalls++
			kept, ok, _ := f.owned.Owned(context.Background(), id)
			if !ok || len(kept.Snapshot) != 1 {
				t.Fatal("pending identity advanced the ownership snapshot")
			}
		}
		return ps3Result(id).Identity, nil
	}
	m.extra = func(_ context.Context, r domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error) {
		if !pending {
			pending = true
			return domain.PS3ExtrasResult{}, errors.New("interrupted native overlay")
		}
		resumed, pending = true, false
		return domain.PS3ExtrasResult{TitleID: r.TitleID, AppliedSourceFileIDs: []int{5001}, VerifiedAt: time.Now()}, nil
	}
	f.downloads.InstallPS3PackagesWith(m)
	old := f.installed(t, 11)
	if m.preparedCalls != 0 {
		t.Fatal("ordinary base installer used the pending extras preparation path")
	}
	appendPS3Attachment(f, 5001, "update", "patch.pkg", testPS3Package(ps3Result(11).Identity.ContentID))
	_ = f.owned.SetUpdateAvailable(context.Background(), 11, true)
	if _, err := f.downloads.Queue(context.Background(), 11); err != nil {
		t.Fatal(err)
	}
	if end := f.end(t, 11); end.State != domain.StateFailed {
		t.Fatal("interrupted overlay was reported Ready", end)
	}
	rec := f.installed(t, 11)
	if !resumed || pendingIdentityCalls != 1 || m.baseCalls != 1 || m.preparedCalls != 0 || m.extraCalls != 2 || !slices.Equal(old.Files, rec.Files) || len(rec.Snapshot) != 2 {
		t.Fatalf("pending routing did not resume extras: %+v %d/%d/%d pendingid%d", rec, m.baseCalls, m.preparedCalls, m.extraCalls, pendingIdentityCalls)
	}
}

func TestPS3ArchivedDiscAndAttachmentKeepsOneGameDirectory(t *testing.T) {
	f := newDownloadFixture(t, 1)
	f.catalog.mu.Lock()
	rom := f.catalog.roms[11]
	rom.FileName = "Disc Game.zip"
	f.catalog.roms[11] = rom
	f.catalog.mu.Unlock()
	f.serve(11, "Disc Game.zip", zipOf(t, member{"Original Folder/PS3_GAME/PARAM.SFO", testPS3SFO("BCES00001")},
		member{"Original Folder/PS3_GAME/USRDIR/EBOOT.BIN", "disc boot"}))
	appendPS3Attachment(f, 5001, "update", "patch.pkg", testPS3Package(discContentID))
	f.downloads.InstallPS3PackagesWith(&ps3ContentInstaller{})
	rec := f.installed(t, 11)
	if len(rec.Files) != 2 {
		t.Fatal(rec)
	}
	for _, file := range rec.Files {
		if !strings.Contains(file.Path, "/Disc Game.ps3/PS3_GAME/") || strings.Contains(file.Path, "Original Folder") {
			t.Fatalf("disc root was not preserved %+v", file)
		}
	}
}

func TestPS3RawRAPIsBoundByContentIDAndKeptInSnapshot(t *testing.T) {
	f := newDownloadFixture(t, 1)
	servePS3Disc(t, f)
	appendPS3Attachment(f, 5001, "dlc", "opaque.pkg", testPS3Package(discDLCContentID))
	appendPS3Attachment(f, 5002, "dlc", discDLCContentID+".rap", "0123456789abcdef")
	m := &ps3ContentInstaller{extra: func(_ context.Context, r domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error) {
		if len(r.Packages) != 1 || r.Packages[0].SourceFileID != 5001 {
			t.Fatalf("bad input association %+v", r)
		}
		entries, err := os.ReadDir(r.Packages[0].Directory)
		if err != nil || len(entries) != 2 {
			t.Fatalf("RAP was not staged with package %+v %v", entries, err)
		}
		return domain.PS3ExtrasResult{TitleID: r.TitleID, AppliedSourceFileIDs: []int{5001}, VerifiedAt: time.Now()}, nil
	}}
	f.downloads.InstallPS3PackagesWith(m)
	rec := f.installed(t, 11)
	if len(rec.Snapshot) != 5 {
		t.Fatalf("separate RAP source was not recorded %+v", rec.Snapshot)
	}
}

func TestPS3RemovalJournalNeverInterceptsAnotherPlatform(t *testing.T) {
	f := newDownloadFixture(t, 1)
	f.installed(t, 1)
	f.downloads.InstallPS3PackagesWith(ps3RemovalFake{check: func(context.Context, int) (bool, error) {
		t.Fatal("unrelated platform consulted the PS3 journal")
		return false, errors.New("malformed PS3 journal")
	}, remove: func(context.Context, int) (bool, error) {
		t.Fatal("unrelated platform removed PS3 data")
		return false, nil
	}})
	if err := f.downloads.deleteGame(context.Background(), 1, false); err != nil {
		t.Fatal(err)
	}
}

func TestPS3ExtraFailureKeepsOwnedDiscAndPrivateInputs(t *testing.T) {
	for _, reason := range []string{"wrong title", "unverified result", "incomplete manifest", "cancel", "ownership write"} {
		t.Run(reason, func(t *testing.T) {
			f := newDownloadFixture(t, 1)
			servePS3Disc(t, f)
			old := f.installed(t, 11)
			appendPS3Attachment(f, 5001, "update", "patch.pkg", testPS3Package(discContentID))
			m := &ps3ContentInstaller{extra: func(_ context.Context, r domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error) {
				switch reason {
				case "wrong title":
					return domain.PS3ExtrasResult{}, errors.New("package targets another title")
				case "cancel":
					return domain.PS3ExtrasResult{}, context.Canceled
				case "incomplete manifest":
					return domain.PS3ExtrasResult{TitleID: r.TitleID, VerifiedAt: time.Now()}, nil
				case "ownership write":
					f.owned.mu.Lock()
					f.owned.saveErr = errors.New("database unavailable")
					f.owned.mu.Unlock()
					return domain.PS3ExtrasResult{TitleID: r.TitleID, AppliedSourceFileIDs: []int{5001}, VerifiedAt: time.Now()}, nil
				default:
					return domain.PS3ExtrasResult{TitleID: r.TitleID, AppliedSourceFileIDs: []int{5001}}, nil
				}
			}}
			f.downloads.InstallPS3PackagesWith(m)
			_ = f.owned.SetUpdateAvailable(context.Background(), 11, true)
			if _, err := f.downloads.Queue(context.Background(), 11); err != nil {
				t.Fatal(err)
			}
			if end := f.end(t, 11); end.State != domain.StateFailed {
				t.Fatalf("failed native install became ready %+v", end)
			}
			kept, ok, err := f.owned.Owned(context.Background(), 11)
			if err != nil || !ok || !slices.Equal(old.Files, kept.Files) || len(old.Snapshot) != len(kept.Snapshot) {
				t.Fatal("old ownership changed", kept, err)
			}
			if _, err := os.Stat(filepath.Join(f.downloads.staging(11), ".ps3-extras", "5001", "patch.pkg")); err != nil {
				t.Fatal("private recovery inputs lost", err)
			}
			for _, file := range old.Files {
				if _, err := os.Stat(file.Path); err != nil {
					t.Fatal("disc lost", err)
				}
			}
		})
	}
}

func TestPS3DiscDeleteRemovesNativeOverlayAndKeepsOwnershipOnChanges(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconvBool(changed), func(t *testing.T) {
			f := newDownloadFixture(t, 1)
			servePS3Disc(t, f)
			rec := f.installed(t, 11)
			m := &ps3ContentInstaller{nativeOwned: true}
			f.downloads.InstallPS3PackagesWith(m)
			if changed {
				if err := os.WriteFile(rec.Files[0].Path, []byte("user edit"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := f.downloads.deleteGame(context.Background(), 11, false)
			if changed {
				if err == nil || m.removed {
					t.Fatal("changed base triggered native deletion", err)
				}
				if _, ok, _ := f.owned.Owned(context.Background(), 11); !ok {
					t.Fatal("native overlay lost ownership")
				}
			} else if err != nil || !m.removed {
				t.Fatal("native overlay not removed", err)
			}
		})
	}
}

func strconvBool(v bool) string {
	if v {
		return "changed"
	}
	return "unchanged"
}
