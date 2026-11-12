package app

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

// Native PS3 installation remains separate from generic ROM-file ownership.
// The adapter verifies every package and its native destination independently.
type PS3ExtrasInstaller interface {
	InstallExtras(context.Context, domain.PS3ExtrasRequest) (domain.PS3ExtrasResult, error)
	BaseIdentity(context.Context, int) (domain.PS3Identity, error)
}

func (d *Downloads) downloadPS3Content(ctx context.Context, j *job, rom domain.ROM, system domain.System, dir string, files []domain.ROMFile, old domain.OwnedROM, server DownloadServer) error {
	installer, ok := d.ps3.(PS3ExtrasInstaller)
	if !ok {
		return &domain.Error{Kind: domain.KindCantDownload, Message: "Native RPCS3 update and DLC installation is unavailable."}
	}
	var previous *domain.OwnedROM
	if j.update {
		previous = &old
	}
	transfer, err := domain.PS3TransferFiles(files, previous)
	if err != nil {
		return err
	}
	name := rom.FileName
	if name == "" {
		name = rom.Name
	}
	baseLayout, err := domain.PlanLayout(system, name, domain.GameFiles(files))
	if err != nil {
		return err
	}
	staging := d.staging(j.ROMID)
	snapshot := domain.SnapshotOfSet(transfer)
	manifest, err := ps3TransferManifest(transfer)
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(staging, ".ps3-transfer.json")
	manifestSame := false
	if info, err := os.Lstat(manifestPath); err == nil {
		if !info.Mode().IsRegular() || info.Size() > 16<<20 {
			return fmt.Errorf("unsafe PS3 transfer manifest")
		}
		prior, err := os.ReadFile(manifestPath)
		if err != nil {
			return err
		}
		manifestSame = string(prior) == string(manifest)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	d.mu.Lock()
	same := samePartial(j.partial, snapshot) && manifestSame
	d.mu.Unlock()
	if !same {
		for _, name := range []string{".ps3-package", ".ps3-extras"} {
			if _, err := os.Lstat(filepath.Join(staging, name)); err == nil {
				return fmt.Errorf("PS3 package inputs changed while an earlier native attempt is retained; review or remove the game before retrying")
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.RemoveAll(staging); err != nil {
			return err
		}
		if err := os.MkdirAll(staging, 0o700); err != nil {
			return err
		}
		if err := writePS3Launcher(manifestPath, manifest); err != nil {
			return err
		}
		d.mu.Lock()
		j.partial = snapshot
		record := j.record()
		d.mu.Unlock()
		d.save(record)
	}
	var total int64
	for _, f := range transfer {
		if total > (1<<63-1)-f.SizeBytes {
			return fmt.Errorf("invalid PS3 transfer size")
		}
		total += f.SizeBytes
	}
	if err := d.checkSpace(total, d.pendingExcept(j.ROMID), dir); err != nil {
		return err
	}
	d.mu.Lock()
	j.State, j.Total, j.FileCount, j.Phase = domain.StateDownloading, total, len(transfer), "download"
	started := j.Download
	d.mu.Unlock()
	d.onChange(started)
	paths, sums := map[int]string{}, map[int]string{}
	var done int64
	for _, f := range transfer {
		path := filepath.Join(staging, ".ps3-content", strconv.Itoa(f.ID), filepath.Base(f.Name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		get := func(from int64, open func(int64) (io.Writer, error)) error {
			return server.DownloadFile(ctx, f.ID, f.Name, from, open)
		}
		sum, err := d.fetch(get, f, path, j, done)
		if err != nil {
			return err
		}
		paths[f.ID], sums[f.ID], done = path, sum, done+f.SizeBytes
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.setPhase(j, "extract")
	var staged []stagedFile
	root := baseLayout.Root
	if !j.update {
		basePaths, baseSums := make([]string, len(baseLayout.Files)), make([]string, len(baseLayout.Files))
		for i, f := range baseLayout.Files {
			basePaths[i], baseSums[i] = paths[f.File.ID], sums[f.File.ID]
		}
		if len(baseLayout.Files) == 1 {
			f := baseLayout.Files[0]
			staged, err = d.unpack(ctx, j, system, basePaths[0], baseSums[0], dir, f.File.ID)
		} else {
			staged, err = stageSet(baseLayout, basePaths, staging, baseSums)
		}
		if err != nil {
			return err
		}
		if len(baseLayout.Files) == 1 && domain.IsArchive(baseLayout.Files[0].File.Name) && isPS3Disc(staged) {
			staged, root, err = ps3DiscArchiveLayout(staged, name)
			if err != nil {
				return err
			}
		}
		var shortcut string
		staged, shortcut, err = d.stagePS3PackageForExtras(ctx, j, rom, staged, staging, dir, nil)
		if err != nil {
			return err
		}
		if shortcut != "" {
			root = shortcut
		}
	} else if len(old.Files) == 0 {
		return fmt.Errorf("PS3 base ownership has no files")
	}
	request := domain.PS3ExtrasRequest{ROMID: j.ROMID}
	if !j.update && isPS3Disc(staged) {
		request.BaseSFOPath, err = stagedPS3SFO(staged)
		request.DiscBase = true
	} else if j.update && !nativePS3Shortcut(old) {
		request.BaseSFOPath, err = ownedPS3SFO(old)
		request.DiscBase = true
	}
	if err != nil {
		return err
	}
	if request.DiscBase {
		request.TitleID, err = ps3TitleFromSFO(request.BaseSFOPath)
	} else {
		identity, identityErr := installer.BaseIdentity(ctx, j.ROMID)
		err = identityErr
		request.TitleID = identity.TitleID
	}
	if err != nil {
		return fmt.Errorf("verify PS3 base before packages: %w", err)
	}
	request.Packages, err = d.preparePS3Extras(ctx, j, transfer, paths, sums, staging, dir)
	if err != nil {
		return err
	}
	if len(request.Packages) == 0 {
		return fmt.Errorf("RomM lists no installable PS3 package attachments")
	}
	if err := d.waitForGame(ctx, j); err != nil {
		return err
	}
	d.setPhase(j, "install")
	j.retainStaging = true
	result, err := installer.InstallExtras(ctx, request)
	if err != nil {
		return fmt.Errorf("RPCS3 package installation failed; private staging retained: %w", err)
	}
	expected := make([]int, len(request.Packages))
	for i, p := range request.Packages {
		expected[i] = p.SourceFileID
	}
	slices.Sort(expected)
	applied := slices.Clone(result.AppliedSourceFileIDs)
	slices.Sort(applied)
	if result.TitleID != request.TitleID || result.VerifiedAt.IsZero() || !slices.Equal(expected, applied) {
		return fmt.Errorf("RPCS3 did not verify the complete attachment manifest; private staging retained")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	placed, media := old.Files, old.Media
	if !j.update {
		placed, err = d.place(staged, dir)
		if err != nil {
			return err
		}
		media = d.placeMedia(context.WithoutCancel(ctx), j.ROMID, dir, root)
	}
	owned := domain.OwnedROM{ROMID: j.ROMID, PlatformID: rom.PlatformID, Files: placed, Media: media,
		Snapshot: domain.SnapshotsBySystem(system, files), InstalledAt: d.now().UTC()}
	if err := d.store.SaveOwned(context.WithoutCancel(ctx), owned); err != nil {
		if !j.update {
			d.removeFiles(placed, dir)
			d.removeMedia(media)
		}
		return fmt.Errorf("record PS3 download: %w", err)
	}
	return nil
}

func ps3TransferManifest(files []domain.ROMFile) ([]byte, error) {
	files = slices.Clone(files)
	slices.SortFunc(files, func(a, b domain.ROMFile) int { return a.ID - b.ID })
	snapshots := make([]domain.FileSnapshot, len(files))
	for i, f := range files {
		snapshots[i] = domain.SnapshotOf(f)
	}
	b, err := json.Marshal(snapshots)
	if err != nil || len(b) > 16<<20 {
		return nil, fmt.Errorf("PS3 transfer manifest exceeds its size limit")
	}
	return b, nil
}

func stagedPS3SFO(files []stagedFile) (string, error) {
	var paths []string
	for _, f := range files {
		if strings.HasSuffix(strings.ToUpper(filepath.ToSlash(f.Rel)), "/PS3_GAME/PARAM.SFO") || strings.EqualFold(filepath.ToSlash(f.Rel), "PS3_GAME/PARAM.SFO") {
			paths = append(paths, f.From)
		}
	}
	if len(paths) != 1 {
		return "", fmt.Errorf("PS3 disc has no unique PARAM.SFO")
	}
	return paths[0], nil
}

func ps3DiscArchiveLayout(files []stagedFile, name string) ([]stagedFile, string, error) {
	var prefix string
	found := false
	for _, f := range files {
		rel := filepath.ToSlash(f.Rel)
		const suffix = "PS3_GAME/PARAM.SFO"
		if strings.HasSuffix(strings.ToUpper(rel), suffix) {
			if found {
				return nil, "", fmt.Errorf("PS3 archive contains several disc roots")
			}
			prefix, found = rel[:len(rel)-len(suffix)], true
		}
	}
	name = filepath.Base(name)
	root := strings.TrimSuffix(name, filepath.Ext(name))
	if !strings.HasSuffix(strings.ToLower(root), ".ps3") {
		root += ".ps3"
	}
	if !found || !filepath.IsLocal(root) || !domain.SafeESDELaunchPath(root) {
		return nil, "", fmt.Errorf("PS3 archive has no safe game root")
	}
	out := slices.Clone(files)
	rels := make([]string, len(out))
	for i := range out {
		rel := filepath.ToSlash(out[i].Rel)
		if !strings.HasPrefix(rel, prefix) {
			return nil, "", fmt.Errorf("PS3 archive mixes game roots")
		}
		out[i].Rel = filepath.Join(root, filepath.FromSlash(strings.TrimPrefix(rel, prefix)))
		rels[i] = out[i].Rel
	}
	if err := validateRelativeFileTargets(rels); err != nil {
		return nil, "", err
	}
	return out, root, nil
}

func ownedPS3SFO(rec domain.OwnedROM) (string, error) {
	var staged []stagedFile
	for _, f := range rec.Files {
		if strings.HasSuffix(strings.ToUpper(filepath.ToSlash(f.Path)), "/PS3_GAME/PARAM.SFO") {
			same, err := unchanged(f)
			if err != nil || !same {
				return "", fmt.Errorf("installed PS3 disc metadata changed; native packages were not installed")
			}
			staged = append(staged, stagedFile{From: f.Path, Rel: "PS3_GAME/PARAM.SFO"})
		}
	}
	return stagedPS3SFO(staged)
}

// Only the SFO's independently stored title ID binds a disc to its packages.
func ps3TitleFromSFO(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return "", fmt.Errorf("PS3 PARAM.SFO is unavailable or unsafe")
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) < 20 || string(b[:4]) != "\x00PSF" {
		return "", fmt.Errorf("invalid PS3 PARAM.SFO")
	}
	keys, data, count := uint64(binary.LittleEndian.Uint32(b[8:])), uint64(binary.LittleEndian.Uint32(b[12:])), uint64(binary.LittleEndian.Uint32(b[16:]))
	if count > 4096 || 20+count*16 > uint64(len(b)) || keys < 20+count*16 || keys >= data || data > uint64(len(b)) {
		return "", fmt.Errorf("invalid PS3 PARAM.SFO bounds")
	}
	var title string
	seenTitle := false
	for i := uint64(0); i < count; i++ {
		e := b[20+i*16:]
		key := keys + uint64(binary.LittleEndian.Uint16(e))
		length, maximum, offset := uint64(binary.LittleEndian.Uint32(e[4:])), uint64(binary.LittleEndian.Uint32(e[8:])), uint64(binary.LittleEndian.Uint32(e[12:]))
		if key >= data || length > maximum || offset > uint64(len(b))-data || length > uint64(len(b))-data-offset {
			return "", fmt.Errorf("invalid PS3 PARAM.SFO entry")
		}
		name, _, found := strings.Cut(string(b[key:data]), "\x00")
		if !found {
			return "", fmt.Errorf("invalid PS3 PARAM.SFO key")
		}
		if name == "TITLE_ID" {
			if seenTitle || binary.LittleEndian.Uint16(e[2:]) != 0x204 || length < 10 || b[data+offset+length-1] != 0 {
				return "", fmt.Errorf("ambiguous PS3 title ID")
			}
			seenTitle = true
			title = strings.TrimRight(string(b[data+offset:data+offset+length]), "\x00")
		}
	}
	if len(title) != 9 || strings.Trim(title[:4], "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "" || strings.Trim(title[4:], "0123456789") != "" {
		return "", fmt.Errorf("invalid PS3 title ID in PARAM.SFO")
	}
	return title, nil
}

type ps3StagedInput struct {
	file stagedFile
	kind domain.PS3PackageKind
}

// Attachments are unpacked separately from the base game. In particular, a
// disc's own PKGDIR/demos never become installer inputs merely by extension.
func (d *Downloads) preparePS3Extras(ctx context.Context, j *job, files []domain.ROMFile, paths, sums map[int]string, staging, dir string) ([]domain.PS3ExtraPackage, error) {
	var packages, licenses []ps3StagedInput
	for _, f := range files {
		if f.IsGame() {
			continue
		}
		items, err := d.unpackPS3Attachment(ctx, j, f, paths[f.ID], sums[f.ID], staging, dir)
		if err != nil {
			return nil, err
		}
		count := 0
		for _, item := range items {
			input := ps3StagedInput{file: item, kind: domain.PS3PackageKind(f.Category)}
			switch strings.ToLower(filepath.Ext(item.From)) {
			case ".pkg":
				count++
				packages = append(packages, input)
			case ".rap":
				licenses = append(licenses, input)
			case ".txt", ".md", ".nfo", ".url", ".pdf":
			default:
				if !strings.EqualFold(filepath.Base(item.From), "README") && !strings.EqualFold(filepath.Base(item.From), "LICENSE") {
					return nil, fmt.Errorf("PS3 attachment mixes a package with unsupported content")
				}
			}
		}
		if count > 1 || count == 0 && !strings.EqualFold(filepath.Ext(f.Name), ".rap") {
			return nil, fmt.Errorf("each PS3 attachment archive must contain exactly one PKG and its optional RAP")
		}
	}
	usedLicenses := map[int]bool{}
	var out []domain.PS3ExtraPackage
	for _, pkg := range packages {
		contentID, err := ps3InputContentID(pkg.file.From)
		if err != nil {
			return nil, err
		}
		inputs := []stagedFile{pkg.file}
		matched := false
		for i, rap := range licenses {
			if strings.TrimSuffix(filepath.Base(rap.file.From), filepath.Ext(rap.file.From)) != contentID {
				continue
			}
			if matched || usedLicenses[i] || rap.kind != pkg.kind {
				return nil, fmt.Errorf("PS3 package has duplicate or differently categorized RAP licenses")
			}
			matched, usedLicenses[i] = true, true
			inputs = append(inputs, rap.file)
		}
		private := filepath.Join(staging, ".ps3-extras", strconv.Itoa(pkg.file.SourceFileID))
		if err := os.MkdirAll(filepath.Dir(private), 0o700); err != nil {
			return nil, err
		}
		if err := privatePS3Inputs(private, inputs); err != nil {
			return nil, err
		}
		out = append(out, domain.PS3ExtraPackage{SourceFileID: pkg.file.SourceFileID, Directory: private, Kind: pkg.kind})
	}
	if len(usedLicenses) != len(licenses) {
		return nil, fmt.Errorf("a PS3 attachment license does not match any attached package")
	}
	return out, nil
}

func (d *Downloads) unpackPS3Attachment(ctx context.Context, j *job, file domain.ROMFile, path, sum, staging, dir string) ([]stagedFile, error) {
	if !domain.IsArchive(path) {
		return []stagedFile{{From: path, Rel: filepath.Base(path), SHA1: sum, SourceFileID: file.ID}}, nil
	}
	members, closeArchive, err := openArchive(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = closeArchive() }()
	if len(members) == 0 || len(members) > 10000 {
		return nil, fmt.Errorf("PS3 attachment archive has an invalid member count")
	}
	var size int64
	for _, m := range members {
		if m.Size < 0 || m.Size > (64<<30)-size {
			return nil, fmt.Errorf("PS3 attachment archive exceeds the size limit")
		}
		size += m.Size
	}
	if err := d.waitForGame(ctx, j); err != nil {
		return nil, err
	}
	if err := d.checkSpace(size, 0, dir); err != nil {
		return nil, err
	}
	dest := filepath.Join(staging, ".ps3-unpacked", strconv.Itoa(file.ID))
	if err := os.RemoveAll(dest); err != nil {
		return nil, err
	}
	files, err := extract(members, dest)
	if err != nil {
		return nil, err
	}
	out := make([]stagedFile, 0, len(files))
	for _, item := range files {
		out = append(out, stagedFile{From: filepath.Join(dest, item.Rel), Rel: item.Rel, SHA1: item.SHA1, SourceFileID: file.ID})
	}
	return out, nil
}

// This header read only associates sidecars. The native adapter independently
// verifies the full package bounds, role, hashes and product identity.
func ps3InputContentID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var header [96]byte
	if _, err := io.ReadFull(f, header[:]); err != nil || string(header[:4]) != "\x7fPKG" {
		return "", fmt.Errorf("PS3 attachment is not a valid PKG")
	}
	identity, err := domain.ParsePS3ContentID(strings.TrimRight(string(header[48:]), "\x00"))
	if err != nil {
		return "", err
	}
	return identity.ContentID, nil
}
