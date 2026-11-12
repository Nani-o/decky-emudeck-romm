package rpcs3

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

func putExtraFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func extraSFO(fields map[string]string) []byte {
	names := []string{}
	for k := range fields {
		names = append(names, k)
	}
	sort.Strings(names)
	keys, values := []byte{}, []byte{}
	entries := make([]byte, 16*len(names))
	for j, k := range names {
		e := entries[j*16:]
		binary.LittleEndian.PutUint16(e, uint16(len(keys)))
		binary.LittleEndian.PutUint16(e[2:], paramSFOString)
		value := append([]byte(fields[k]), 0)
		binary.LittleEndian.PutUint32(e[4:], uint32(len(value)))
		binary.LittleEndian.PutUint32(e[8:], uint32(len(value)))
		binary.LittleEndian.PutUint32(e[12:], uint32(len(values)))
		keys = append(keys, append([]byte(k), 0)...)
		values = append(values, value...)
	}
	h := make([]byte, 20)
	copy(h, []byte{0, 'P', 'S', 'F'})
	binary.LittleEndian.PutUint32(h[4:], 0x101)
	binary.LittleEndian.PutUint32(h[8:], uint32(20+len(entries)))
	binary.LittleEndian.PutUint32(h[12:], uint32(20+len(entries)+len(keys)))
	binary.LittleEndian.PutUint32(h[16:], uint32(len(names)))
	h = append(h, entries...)
	h = append(h, keys...)
	return append(h, values...)
}
func extraInput(t *testing.T, id int, kind domain.PS3PackageKind, version, contentID string, rap bool) domain.PS3ExtraPackage {
	t.Helper()
	root := filepath.Join(resolvedTestTempDir(t), "source")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	flags := uint32(0)
	if kind == domain.PS3PackageUpdate {
		flags |= pkgFlagPatch
	}
	if rap {
		flags |= pkgFlagRequireLicense
	}
	fields := []testPKGMetadata{{id: 2, value: be32(4)}, {id: 3, value: be32(flags)}, {id: 6, value: paddedTitleID(testTitleID)}}
	if version != "" {
		var a, b int
		if _, err := fmt.Sscanf(version, "%d.%d", &a, &b); err != nil {
			t.Fatal(err)
		}
		v := make([]byte, 8)
		v[6] = byte(a/10*16 + a%10)
		v[7] = byte(b/10*16 + b%10)
		fields = append(fields, testPKGMetadata{id: 8, value: v})
	}
	putExtraFile(t, filepath.Join(root, "payload.pkg"), makePKGWithFields(contentID, testTitleID, fields, 0))
	if rap {
		putExtraFile(t, filepath.Join(root, contentID+".rap"), []byte("license16-bytes!"))
	}
	return domain.PS3ExtraPackage{SourceFileID: id, Directory: root, Kind: kind}
}

type fakeExtra struct {
	initArtifacts         bool
	version, prerequisite string
	payload, category     string
	patchTouchesDLC       bool
	dlc, noLog, partial   bool
	err                   error
}

func fakeExtras(t *testing.T, i *Installer, specs map[string]fakeExtra, calls *[]string) {
	t.Helper()
	i.extraRunner = func(config string) Runner {
		return runnerFunc(func(ctx context.Context, exe string, args ...string) error {
			if len(args) != 3 || args[0] != "--headless" || args[1] != "--installpkg" {
				return errors.New("invalid args")
			}
			pkg := args[2]
			*calls = append(*calls, pkg)
			spec := specs[pkg]
			if spec.err != nil {
				return spec.err
			}
			nand, err := readDevHDD0(filepath.Join(config, "vfs.yml"), i.config.HomeDir)
			if err != nil {
				return err
			}
			title := extraTitleRoot(nand, testTitleID)
			if spec.prerequisite != "" {
				sfo, err := inspectSFOStrings(filepath.Join(title, "PARAM.SFO"))
				if err != nil || sfo["APP_VER"] != spec.prerequisite {
					return fmt.Errorf("bad native prerequisite: %+v %v", sfo, err)
				}
			}
			if spec.dlc {
				payload := spec.payload
				if payload == "" {
					payload = "DLC"
				}
				putExtraFile(t, filepath.Join(title, "DLC", "pack.edat"), []byte(payload))
				if !spec.partial {
					putExtraFile(t, filepath.Join(title, "DLC", "asset.dat"), []byte("asset"))
				}
			} else {
				category := spec.category
				if category == "" {
					category = "GD"
				}
				putExtraFile(t, filepath.Join(title, "PARAM.SFO"), extraSFO(map[string]string{"TITLE_ID": testTitleID, "CATEGORY": category, "APP_VER": spec.version}))
				if !spec.partial {
					putExtraFile(t, filepath.Join(title, "USRDIR", "EBOOT.BIN"), []byte("boot-"+spec.version))
				}
			}
			if spec.patchTouchesDLC {
				putExtraFile(t, filepath.Join(title, "DLC", "pack.edat"), []byte("patch would replace DLC"))
				putExtraFile(t, filepath.Join(title, "DLC", "asset.dat"), []byte("patched asset"))
			}
			if spec.initArtifacts {
				if err := os.MkdirAll(filepath.Join(nand, "game", "TEST12345", "USRDIR"), 0700); err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Join(nand, "game", "＄locks"), 0700); err != nil {
					return err
				}
			}
			if !spec.noLog {
				loggedTitle := testTitleID
				if spec.dlc {
					loggedTitle = ""
				}
				putExtraFile(t, filepath.Join(filepath.Dir(filepath.Dir(config)), "cache", "rpcs3", "RPCS3.log"), []byte("·S 0:00:00.100000 GUI: Successfully installed "+pkg+" (title_id="+loggedTitle+", title=Fixture, version="+spec.version+").\n"))
			}
			return nil
		})
	}
}
func pk(in domain.PS3ExtraPackage) string { return filepath.Join(in.Directory, "payload.pkg") }
func reqExtras(inputs ...domain.PS3ExtraPackage) domain.PS3ExtrasRequest {
	return domain.PS3ExtrasRequest{ROMID: 77, TitleID: testTitleID, Packages: inputs}
}
func psnExtras(t *testing.T) (*Installer, string, string) {
	t.Helper()
	i, nand, base := installedRemovalFixture(t)
	putExtraFile(t, filepath.Join(extraTitleRoot(nand, testTitleID), "PARAM.SFO"), extraSFO(map[string]string{"TITLE_ID": testTitleID, "CATEGORY": "HG", "APP_VER": "01.00"}))
	in, _, _ := readIntent(i.config.IntentDir, testTitleID)
	inv, err := captureInstalledInventory(extraTitleRoot(nand, testTitleID))
	if err != nil {
		t.Fatal(err)
	}
	in.Inventory = &inv
	if err = savePrivateJSON(intentFile(i.config.IntentDir, testTitleID), in, false); err != nil {
		t.Fatal(err)
	}
	return i, nand, base
}

const extraDLCID = "EP2007-NPEB00059_00-DLC0000000000001"

func TestPS3ExtrasChainNativeOrderReplayAndFullUninstall(t *testing.T) {
	i, nand, base := psnExtras(t)
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	b := extraInput(t, 2, domain.PS3PackageUpdate, "01.02", testContentID, false)
	d := extraInput(t, 3, domain.PS3PackageDLC, "01.00", extraDLCID, true)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01", prerequisite: "01.00"}, pk(b): {version: "01.02", prerequisite: "01.01"}, pk(d): {dlc: true}}, &calls)
	save := filepath.Join(nand, "home", "00000001", "savedata", "keep")
	putExtraFile(t, save, []byte("progress"))
	other := filepath.Join(nand, "game", "NPEB99999", "other")
	putExtraFile(t, other, []byte("other"))
	req := reqExtras(d, b, a)
	got, err := i.InstallExtras(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.AppliedSourceFileIDs, []int{1, 2, 3}) || !reflect.DeepEqual(calls, []string{pk(a), pk(b), pk(d)}) {
		t.Fatalf("order %v %v", got, calls)
	}
	in, found, err := readIntent(i.config.IntentDir, testTitleID)
	if err != nil || !found || in.Version != 3 || len(in.Extras) != 3 {
		t.Fatalf("v3 %+v %v", in, err)
	}
	if _, err = i.BaseIdentity(context.Background(), 77); err != nil {
		t.Fatal(err)
	}
	if _, err = i.Install(context.Background(), 77, base); err != nil {
		t.Fatal(err)
	}
	if _, err = i.InstallExtras(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 3 {
		t.Fatal("idempotent extras rerun")
	}
	if _, err = i.Remove(context.Background(), 77); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{save: "progress", other: "other"} {
		raw, err := os.ReadFile(p)
		if err != nil || string(raw) != want {
			t.Fatal("unrelated data changed")
		}
	}
	if _, err = os.Stat(filepath.Join(nand, "home", "00000001", "exdata", extraDLCID+".rap")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("owned RAP retained")
	}
	if _, err = i.Install(context.Background(), 77, base); err != nil {
		t.Fatal(err)
	}
}
func TestPS3DiscExtrasUninstallReinstallAndBorrowedLicense(t *testing.T) {
	i, nand, _ := makeInstaller(t, nil, nil, nil)
	disc := filepath.Join(resolvedTestTempDir(t), "PS3_GAME", "PARAM.SFO")
	putExtraFile(t, disc, extraSFO(map[string]string{"TITLE_ID": testTitleID, "CATEGORY": "DG", "APP_VER": "01.00"}))
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	d := extraInput(t, 2, domain.PS3PackageDLC, "01.00", extraDLCID, true)
	rap := filepath.Join(nand, "home", "00000001", "exdata", extraDLCID+".rap")
	putExtraFile(t, rap, []byte("license16-bytes!"))
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01", prerequisite: "01.00"}, pk(d): {dlc: true}}, &calls)
	req := reqExtras(d, a)
	req.DiscBase = true
	req.BaseSFOPath = disc
	if _, err := i.InstallExtras(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	in, _, _ := readIntent(i.config.IntentDir, testTitleID)
	if in.BaseKind != "disc" || in.LicenseOwned || len(in.ExtraLicenses) != 1 || in.ExtraLicenses[0].Owned {
		t.Fatalf("ownership %+v", in)
	}
	if _, err := i.Remove(context.Background(), 77); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{disc, rap} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("borrowed/source removed: %v", err)
		}
	}
	if _, err := i.InstallExtras(context.Background(), req); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
}
func TestPS3ExtrasPrecommitFailuresKeepNANDAndCleanUncommittedWork(t *testing.T) {
	for _, kind := range []string{"native-failure", "missing-proof", "partial", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			i, nand, _ := psnExtras(t)
			a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
			spec := fakeExtra{version: "01.01"}
			switch kind {
			case "native-failure":
				spec.err = errors.New("native failure")
			case "missing-proof":
				spec.noLog = true
			case "partial":
				spec.partial = true
			case "cancel":
				spec.err = context.Canceled
			}
			var calls []string
			fakeExtras(t, i, map[string]fakeExtra{pk(a): spec}, &calls)
			before, _, _ := readIntent(i.config.IntentDir, testTitleID)
			if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err == nil {
				t.Fatal("failure hidden")
			}
			after, _, _ := readIntent(i.config.IntentDir, testTitleID)
			if ownershipHash(before) != ownershipHash(after) {
				t.Fatal("receipt mutated")
			}
			fields, _ := inspectSFOStrings(filepath.Join(extraTitleRoot(nand, testTitleID), "PARAM.SFO"))
			if fields["APP_VER"] != "01.00" {
				t.Fatal("NAND changed")
			}
			entries, _ := os.ReadDir(i.config.IntentDir)
			for _, e := range entries {
				if strings.HasPrefix(e.Name(), ".ps3-extras-") {
					t.Fatal("uncommitted native copies retained")
				}
			}
		})
	}
}
func TestPS3ExtrasCommitRecoveryAndForeignMutation(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprint(foreign), func(t *testing.T) {
			i, nand, _ := psnExtras(t)
			a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
			var calls []string
			fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01"}}, &calls)
			count := 0
			i.overlayWrite = func(_, _ string) error {
				count++
				if count == 2 {
					return errors.New("IO interrupted")
				}
				return nil
			}
			if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err == nil {
				t.Fatal("IO failure hidden")
			}
			if _, err := i.CheckRemoval(context.Background(), 77); err == nil {
				t.Fatal("partial uninstall accepted")
			}
			r, found, err := i.readOverlay(testTitleID)
			if err != nil || !found {
				t.Fatal("no recovery receipt")
			}
			i.overlayWrite = nil
			if foreign {
				p := filepath.Join(extraTitleRoot(nand, testTitleID), "USRDIR", "EBOOT.BIN")
				putExtraFile(t, p, []byte("foreign"))
				if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err == nil {
					t.Fatal("foreign overwrite")
				}
				raw, _ := os.ReadFile(p)
				if string(raw) != "foreign" {
					t.Fatal("foreign changed")
				}
				return
			}
			if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err != nil {
				t.Fatal(err)
			}
			if len(calls) != 1 {
				t.Fatal("native reran")
			}
			if _, err := os.Stat(r.WorkDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("work retained")
			}
			if _, err := i.CheckRemoval(context.Background(), 77); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestPS3ExtrasDiscFirstWriteInterruptionAndPartialTempAreResumable(t *testing.T) {
	i, _, _ := makeInstaller(t, nil, nil, nil)
	disc := filepath.Join(resolvedTestTempDir(t), "PS3_GAME", "PARAM.SFO")
	putExtraFile(t, disc, extraSFO(map[string]string{"TITLE_ID": testTitleID, "CATEGORY": "DG", "APP_VER": "01.00"}))
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01"}}, &calls)
	i.overlayWrite = func(_, _ string) error { return errors.New("stop before first file") }
	req := reqExtras(a)
	req.DiscBase = true
	req.BaseSFOPath = disc
	_, _ = i.InstallExtras(context.Background(), req)
	r, ok, err := i.readOverlay(testTitleID)
	if err != nil || !ok {
		t.Fatal("pending missing")
	}
	if err = os.MkdirAll(filepath.Join(extraTitleRoot(r.After.DevHDD0, testTitleID), "USRDIR"), 0700); err != nil {
		t.Fatal(err)
	}
	temp := filepath.Join(r.After.DevHDD0, filepath.FromSlash(overlayTempPath(r, r.Changes[0].Path)))
	putExtraFile(t, temp, []byte("part"))
	i.overlayWrite = nil
	if _, err = i.InstallExtras(context.Background(), req); err != nil {
		t.Fatalf("fresh empty inventory/partial temp retry: %v", err)
	}
	if len(calls) != 1 {
		t.Fatal("native reran")
	}
}
func TestPS3ExtrasCannotDropDLCOrChangeDuplicatedPackageLicense(t *testing.T) {
	i, _, _ := psnExtras(t)
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	d := extraInput(t, 2, domain.PS3PackageDLC, "01.00", extraDLCID, true)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01"}, pk(d): {dlc: true}}, &calls)
	if _, err := i.InstallExtras(context.Background(), reqExtras(a, d)); err != nil {
		t.Fatal(err)
	}
	if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err == nil || !strings.Contains(err.Error(), "uninstall and reinstall") {
		t.Fatal("dropped source accepted")
	}
	duplicate := extraInput(t, 3, domain.PS3PackageDLC, "01.00", extraDLCID, true)
	putExtraFile(t, filepath.Join(duplicate.Directory, extraDLCID+".rap"), []byte("DIFFERENT16BYTES"))
	if _, err := i.InstallExtras(context.Background(), reqExtras(a, d, duplicate)); err == nil {
		t.Fatal("duplicate differing license accepted")
	}
	if len(calls) != 2 {
		t.Fatal("rejected request ran native")
	}
}
func TestPS3ExtraInspectRejectsRoleTitleAndAmbiguousVersion(t *testing.T) {
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	if _, err := InspectExtraBundle(a.Directory, domain.PS3PackageDLC, testTitleID); err == nil {
		t.Fatal("role mismatch")
	}
	if _, err := InspectExtraBundle(a.Directory, a.Kind, "NPEB99999"); err == nil {
		t.Fatal("title mismatch")
	}
	b := extraInput(t, 2, domain.PS3PackageUpdate, "01.01", testContentID, false)
	data, _ := os.ReadFile(pk(b))
	data[len(data)-1] = 42
	putExtraFile(t, pk(b), data)
	i, _, _ := psnExtras(t)
	if _, err := i.InstallExtras(context.Background(), reqExtras(a, b)); err == nil {
		t.Fatal("ambiguous same version accepted")
	}
}
func TestCommandRunnerUsesPrivateHeadlessProofCache(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "gui-cache"))
	r := commandRunner{home: t.TempDir(), xdgConfigHome: t.TempDir()}
	if err := r.Run(context.Background(), "/bin/sh", "-c", `[ "$XDG_CACHE_HOME" = "$XDG_CONFIG_HOME" ]`); err != nil {
		t.Fatal(err)
	}
	r.xdgCacheHome = t.TempDir()
	if err := r.Run(context.Background(), "/bin/sh", "-c", `[ "$XDG_CACHE_HOME" != "$XDG_CONFIG_HOME" ]`); err != nil {
		t.Fatal(err)
	}
}

func TestPS3FinalIntentCannotExceedItsReadBound(t *testing.T) {
	p := filepath.Join(resolvedTestTempDir(t), testTitleID+".json")
	if err := savePrivateJSON(p, map[string]string{"oversized": strings.Repeat("x", maxIntentSize)}, false); err == nil {
		t.Fatal("unreadable final receipt was published")
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("oversized intent left on disk")
	}
}

func TestPS3NewPatchReappliesPreviouslyKnownDLC(t *testing.T) {
	i, nand, _ := psnExtras(t)
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	b := extraInput(t, 2, domain.PS3PackageUpdate, "01.02", testContentID, false)
	d := extraInput(t, 3, domain.PS3PackageDLC, "01.00", extraDLCID, false)
	raw, _ := os.ReadFile(pk(b))
	raw = append(raw, make([]byte, 96)...)
	binary.BigEndian.PutUint32(raw[20:24], 6)
	binary.BigEndian.PutUint64(raw[24:32], uint64(len(raw)))
	binary.BigEndian.PutUint64(raw[40:48], 192)
	putExtraFile(t, pk(b), raw)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01"}, pk(b): {version: "01.02", prerequisite: "01.01", patchTouchesDLC: true}, pk(d): {dlc: true}}, &calls)
	if _, err := i.InstallExtras(context.Background(), reqExtras(a, d)); err != nil {
		t.Fatal(err)
	}
	if _, err := i.InstallExtras(context.Background(), reqExtras(b, d, a)); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{pk(a), pk(d), pk(b), pk(d)}) {
		t.Fatalf("native order/replay=%v", calls)
	}
	data, err := os.ReadFile(filepath.Join(extraTitleRoot(nand, testTitleID), "DLC", "pack.edat"))
	if err != nil || string(data) != "DLC" {
		t.Fatalf("known DLC overwritten by new patch: %q %v", data, err)
	}
}
func TestPS3DistinctDLCConflictingFilesAreRefusedBeforeNANDMutation(t *testing.T) {
	i, nand, _ := psnExtras(t)
	d := extraInput(t, 1, domain.PS3PackageDLC, "01.00", extraDLCID, false)
	other := extraInput(t, 2, domain.PS3PackageDLC, "01.00", "EP2007-NPEB00059_00-DLC0000000000002", false)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(d): {dlc: true}, pk(other): {dlc: true, payload: "conflicting other DLC"}}, &calls)
	if _, err := i.InstallExtras(context.Background(), reqExtras(d)); err != nil {
		t.Fatal(err)
	}
	before, _, _ := readIntent(i.config.IntentDir, testTitleID)
	if _, err := i.InstallExtras(context.Background(), reqExtras(other, d)); err == nil || !strings.Contains(err.Error(), "conflicting bytes") {
		t.Fatalf("DLC conflict accepted: %v", err)
	}
	after, _, _ := readIntent(i.config.IntentDir, testTitleID)
	if ownershipHash(before) != ownershipHash(after) {
		t.Fatal("conflict mutated ownership")
	}
	data, _ := os.ReadFile(filepath.Join(extraTitleRoot(nand, testTitleID), "DLC", "pack.edat"))
	if string(data) != "DLC" {
		t.Fatal("existing DLC changed")
	}
}
func TestPS3PendingBaseCanBePreparedOnlyForExtrasRetry(t *testing.T) {
	i, _, base := psnExtras(t)
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01"}}, &calls)
	count := 0
	i.overlayWrite = func(_, _ string) error {
		count++
		if count == 2 {
			return errors.New("interrupted overlay")
		}
		return nil
	}
	if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err == nil {
		t.Fatal("interruption hidden")
	}
	i.overlayWrite = nil
	if _, err := i.Install(context.Background(), 77, base); err == nil {
		t.Fatal("generic base installation reported success for a hybrid game")
	}
	identity, err := i.BaseIdentity(context.Background(), 77)
	if err != nil || identity.ContentID != testContentID {
		t.Fatalf("pending base identity routing: %+v %v", identity, err)
	}
	result, prepared, err := i.PreparedBase(context.Background(), 77, base)
	if err != nil || !prepared || result.DesktopEntry == "" {
		t.Fatalf("prepare result %+v %t %v", result, prepared, err)
	}
	if _, err := i.CheckRemoval(context.Background(), 77); err == nil {
		t.Fatal("preparation finalized the transaction")
	}
	if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatal("resume reran native")
	}
}
func TestPS3RejectedStageKeepsBoundedReasonAfterWorkCleanup(t *testing.T) {
	i, _, _ := psnExtras(t)
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01", category: "GP"}}, &calls)
	if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err == nil || !strings.Contains(err.Error(), `CATEGORY="GP"`) {
		t.Fatalf("reason lost: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(i.config.IntentDir, testTitleID+".last-stage-error.json"))
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic map[string]any
	if err = json.Unmarshal(raw, &diagnostic); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostic["reason"].(string), `CATEGORY="GP"`) {
		t.Fatal("bounded diagnosis lost")
	}
}

func TestPS3NativeInitializationDirectoriesAreNeverPromoted(t *testing.T) {
	i, nand, _ := psnExtras(t)
	a := extraInput(t, 1, domain.PS3PackageUpdate, "01.01", testContentID, false)
	var calls []string
	fakeExtras(t, i, map[string]fakeExtra{pk(a): {version: "01.01", initArtifacts: true}}, &calls)
	if _, err := i.InstallExtras(context.Background(), reqExtras(a)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"TEST12345", "＄locks"} {
		if _, err := os.Stat(filepath.Join(nand, "game", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("native initialization artifact promoted: %s", name)
		}
	}
}
func TestPS3NativeNamespaceRefusesUnknownOrNonemptyArtifacts(t *testing.T) {
	for _, kind := range []string{"other-title", "test-file", "lock-file", "test-symlink"} {
		t.Run(kind, func(t *testing.T) {
			nand := resolvedTestTempDir(t)
			if err := os.MkdirAll(filepath.Join(nand, "game", testTitleID), 0700); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "other-title":
				if err := os.Mkdir(filepath.Join(nand, "game", "NPEB99999"), 0700); err != nil {
					t.Fatal(err)
				}
			case "test-file":
				putExtraFile(t, filepath.Join(nand, "game", "TEST12345", "USRDIR", "unexpected"), []byte("not initialization"))
			case "lock-file":
				putExtraFile(t, filepath.Join(nand, "game", "＄locks", "unexpected"), nil)
			case "test-symlink":
				if err := os.MkdirAll(filepath.Join(nand, "game", "TEST12345"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(nand, "game", testTitleID), filepath.Join(nand, "game", "TEST12345", "USRDIR")); err != nil {
					t.Fatal(err)
				}
			}
			if err := verifyPrivateNativeNamespace(nand, testTitleID); err == nil {
				t.Fatal("unexpected native namespace accepted")
			}
		})
	}
}
