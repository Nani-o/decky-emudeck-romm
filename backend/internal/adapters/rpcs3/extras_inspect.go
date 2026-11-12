package rpcs3

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

// ExtraBundle carries metadata read from the actual PKG, never its filename.
type ExtraBundle struct {
	Bundle
	Kind             domain.PS3PackageKind
	AppVersion       string
	InstallDirectory string
	RequiresLicense  bool
	SourceFileID     int
}

func InspectExtraBundle(directory string, kind domain.PS3PackageKind, titleID string) (ExtraBundle, error) {
	var out ExtraBundle
	if !validTitle(titleID) || (kind != domain.PS3PackageUpdate && kind != domain.PS3PackageDLC) {
		return out, fmt.Errorf("invalid PS3 attachment identity or role")
	}
	root, err := filepath.Abs(directory)
	if err != nil {
		return out, err
	}
	if strings.ContainsAny(root, "\x00\r\n") {
		return out, ErrInvalidBundle
	}
	info, err := os.Lstat(root)
	if err != nil {
		return out, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return out, ErrInvalidBundle
	}
	var pkg, rap string
	var count int
	var total int64
	err = filepath.WalkDir(root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		count++
		if count > maxBundleEntries || e.Type()&fs.ModeSymlink != 0 || strings.ContainsAny(p, "\x00\r\n") {
			return ErrInvalidBundle
		}
		if e.IsDir() {
			return nil
		}
		inf, err := e.Info()
		if err != nil {
			return err
		}
		if !inf.Mode().IsRegular() || inf.Size() < 0 || inf.Size() > maxBundleSize-total {
			return ErrInvalidBundle
		}
		total += inf.Size()
		switch strings.ToLower(filepath.Ext(p)) {
		case ".pkg":
			if pkg != "" {
				return ErrInvalidBundle
			}
			pkg = p
		case ".rap":
			if rap != "" {
				return ErrInvalidBundle
			}
			rap = p
		default:
			return ErrInvalidBundle
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	if pkg == "" {
		return out, ErrInvalidBundle
	}
	out, err = readExtraMetadata(pkg, kind, titleID)
	if err != nil {
		return ExtraBundle{}, err
	}
	out.Directory = root
	out.PackagePath = pkg
	out.RAPPath = rap
	if out.RequiresLicense && rap == "" {
		return ExtraBundle{}, fmt.Errorf("PS3 attachment requires its matching RAP")
	}
	if rap != "" {
		if filepath.Base(rap) != out.Identity.ContentID+filepath.Ext(rap) {
			return ExtraBundle{}, fmt.Errorf("RAP does not match attachment ContentID")
		}
		out.RAPSHA256, err = sha256File(rap, 16)
		if err != nil {
			return ExtraBundle{}, err
		}
	}
	out.PackageSHA256, err = sha256File(pkg, out.PackageSize)
	return out, err
}

func readExtraMetadata(p string, kind domain.PS3PackageKind, titleID string) (ExtraBundle, error) {
	out := ExtraBundle{Kind: kind}
	f, err := os.Open(p)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	if !info.Mode().IsRegular() || info.Size() < pkgHeaderSize || info.Size() > maxPackageSize {
		return out, ErrInvalidBundle
	}
	h := make([]byte, pkgHeaderSize)
	if _, err = io.ReadFull(f, h); err != nil {
		return out, err
	}
	if !bytes.Equal(h[:4], []byte{0x7f, 'P', 'K', 'G'}) || binary.BigEndian.Uint16(h[4:6]) != pkgReleaseRetail || binary.BigEndian.Uint16(h[6:8]) != ps3PlatformType {
		return out, ErrUnsupportedPKG
	}
	off, n, size := uint64(binary.BigEndian.Uint32(h[8:12])), uint64(binary.BigEndian.Uint32(h[12:16])), uint64(binary.BigEndian.Uint32(h[16:20]))
	pkgsize, dataoff, datasize := binary.BigEndian.Uint64(h[24:32]), binary.BigEndian.Uint64(h[32:40]), binary.BigEndian.Uint64(h[40:48])
	entries := binary.BigEndian.Uint32(h[20:24])
	if pkgsize != uint64(info.Size()) || off < pkgHeaderSize || n == 0 || n > 1024 || size < n*8 || size > maxMetadataSize || off > pkgsize || size > pkgsize-off || dataoff < off+size || dataoff > pkgsize || datasize > pkgsize-dataoff || entries == 0 || uint64(entries) > datasize/32 {
		return out, ErrInvalidBundle
	}
	rawid := h[0x30:0x60]
	end := bytes.IndexByte(rawid, 0)
	if end < 0 {
		end = len(rawid)
	} else if !allZero(rawid[end:]) {
		return out, ErrInvalidBundle
	}
	out.Identity, err = domain.ParsePS3ContentID(string(rawid[:end]))
	if err != nil || out.Identity.TitleID != titleID {
		return out, fmt.Errorf("PS3 attachment ContentID does not match the independently identified base title")
	}
	data := make([]byte, int(size))
	if _, err = f.ReadAt(data, int64(off)); err != nil {
		return out, err
	}
	seen := map[uint32]bool{}
	var cursor uint64
	var ct, flags uint32
	installDir := titleID
	for range n {
		if cursor > size || size-cursor < 8 {
			return out, ErrInvalidBundle
		}
		id, l := binary.BigEndian.Uint32(data[cursor:cursor+4]), uint64(binary.BigEndian.Uint32(data[cursor+4:cursor+8]))
		cursor += 8
		if l > size-cursor {
			return out, ErrInvalidBundle
		}
		v := data[cursor : cursor+l]
		cursor += l
		if id != 2 && id != 3 && id != 6 && id != 8 && id != 10 {
			continue
		}
		if seen[id] {
			return out, ErrInvalidBundle
		}
		seen[id] = true
		switch id {
		case 2:
			if len(v) != 4 {
				return out, ErrInvalidBundle
			}
			ct = binary.BigEndian.Uint32(v)
		case 3:
			if len(v) != 4 {
				return out, ErrInvalidBundle
			}
			flags = binary.BigEndian.Uint32(v)
		case 6:
			if len(v) != 12 || strings.TrimRight(string(v), "\x00 ") != titleID {
				return out, ErrInvalidBundle
			}
		case 8:
			if len(v) != 8 {
				return out, fmt.Errorf("invalid PS3 software revision metadata")
			}
			major, ok := bcd(v[6])
			if !ok {
				return out, ErrInvalidBundle
			}
			minor, ok := bcd(v[7])
			if !ok {
				return out, ErrInvalidBundle
			}
			out.AppVersion = fmt.Sprintf("%02d.%02d", major, minor)
		case 10:
			if len(v) < 9 || len(v) > 1032 {
				return out, ErrInvalidBundle
			}
			name := v[8:]
			term := bytes.IndexByte(name, 0)
			if term >= 0 {
				if !allZero(name[term:]) {
					return out, ErrInvalidBundle
				}
				name = name[:term]
			}
			installDir = string(name)
		}
	}
	if !seen[2] || !seen[3] || ct != 4 {
		return out, fmt.Errorf("PS3 attachments must be retail game-data PKGs")
	}
	if kind == domain.PS3PackageUpdate {
		if flags&pkgFlagPatch == 0 || out.AppVersion == "" || out.AppVersion == "00.00" {
			return out, fmt.Errorf("PS3 update requires PATCH metadata and an unambiguous APP_VER")
		}
	} else if flags&pkgFlagPatch != 0 || flags&pkgFlagEBOOT != 0 {
		return out, fmt.Errorf("PS3 DLC metadata instead identifies a patch or executable")
	}
	if !inventoryRelative(installDir) || strings.Split(installDir, "/")[0] != titleID {
		return out, fmt.Errorf("PS3 attachment installation directory targets another title")
	}
	if kind == domain.PS3PackageUpdate && installDir != titleID {
		return out, fmt.Errorf("PS3 update installation directory is not the base title")
	}
	out.InstallDirectory = installDir
	out.RequiresLicense = flags&pkgFlagRequireLicense != 0
	out.PackageSize = info.Size()
	out.EntryCount = entries
	return out, nil
}
func bcd(v byte) (int, bool) {
	if v>>4 > 9 || v&15 > 9 {
		return 0, false
	}
	return int(v>>4)*10 + int(v&15), true
}
func validTitle(title string) bool {
	_, err := domain.ParsePS3ContentID("EP0000-" + title + "_00-0000000000000000")
	return err == nil
}
func versionNumber(v string) (int, error) {
	parts := strings.Split(v, ".")
	if len(parts) != 2 || len(parts[0]) > 2 || len(parts[1]) != 2 {
		return 0, fmt.Errorf("invalid PS3 APP_VER")
	}
	a, e := strconv.Atoi(parts[0])
	b, e2 := strconv.Atoi(parts[1])
	if e != nil || e2 != nil || a < 0 || a > 99 || b < 0 || b > 99 {
		return 0, fmt.Errorf("invalid PS3 APP_VER")
	}
	return a*100 + b, nil
}
func sortExtraBundles(xs []ExtraBundle) error {
	sort.SliceStable(xs, func(a, b int) bool {
		x, y := xs[a], xs[b]
		if x.Kind != y.Kind {
			return x.Kind == domain.PS3PackageUpdate
		}
		if x.AppVersion != y.AppVersion {
			return x.AppVersion < y.AppVersion
		}
		if x.Identity.ContentID != y.Identity.ContentID {
			return x.Identity.ContentID < y.Identity.ContentID
		}
		return x.SourceFileID < y.SourceFileID
	})
	versions := map[string]string{}
	payloads := map[string]extraReceipt{}
	for _, x := range xs {
		now := extraReceipt{Kind: x.Kind, ContentID: x.Identity.ContentID, PackageSHA256: x.PackageSHA256, RAPSHA256: x.RAPSHA256, AppVersion: x.AppVersion}
		if old, found := payloads[x.PackageSHA256]; found && old != now {
			return fmt.Errorf("duplicate PS3 package has inconsistent role, version or license")
		}
		payloads[x.PackageSHA256] = now
		key := string(x.Kind) + ":" + x.AppVersion
		if x.Kind == domain.PS3PackageDLC {
			key += "/" + x.Identity.ContentID
		}
		if sha, found := versions[key]; found && sha != x.PackageSHA256 {
			return fmt.Errorf("PS3 attachments have the same version with different payloads; install order is ambiguous")
		}
		versions[key] = x.PackageSHA256
	}
	return nil
}

// inspectSFOStrings reads only the well-formed string fields needed for title,
// version and prerequisite checks. Unknown fields remain opaque.
func inspectSFOStrings(p string) (map[string]string, error) {
	info, err := validRegular(p, maxSFOSize)
	if err != nil {
		return nil, err
	}
	if info.Size() < 20 {
		return nil, ErrInvalidBundle
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data[:4], []byte{0, 'P', 'S', 'F'}) {
		return nil, ErrInvalidBundle
	}
	keys, values, n := uint64(binary.LittleEndian.Uint32(data[8:12])), uint64(binary.LittleEndian.Uint32(data[12:16])), uint64(binary.LittleEndian.Uint32(data[16:20]))
	size := uint64(len(data))
	if n == 0 || n > 4096 || 20+n*16 > keys || keys > values || values > size {
		return nil, ErrInvalidBundle
	}
	out := map[string]string{}
	for j := uint64(0); j < n; j++ {
		e := 20 + j*16
		k := keys + uint64(binary.LittleEndian.Uint16(data[e:e+2]))
		format := binary.LittleEndian.Uint16(data[e+2 : e+4])
		l, m, o := uint64(binary.LittleEndian.Uint32(data[e+4:e+8])), uint64(binary.LittleEndian.Uint32(data[e+8:e+12])), uint64(binary.LittleEndian.Uint32(data[e+12:e+16]))
		if k >= values {
			return nil, ErrInvalidBundle
		}
		term := bytes.IndexByte(data[k:values], 0)
		if term < 0 {
			return nil, ErrInvalidBundle
		}
		key := string(data[k : k+uint64(term)])
		if key != "TITLE_ID" && key != "APP_VER" && key != "TARGET_APP_VER" && key != "CATEGORY" {
			continue
		}
		if _, found := out[key]; found || format != paramSFOString || l == 0 || l > m || o > size-values || l > size-values-o {
			return nil, ErrInvalidBundle
		}
		out[key] = strings.TrimRight(string(data[values+o:values+o+l]), "\x00 ")
	}
	if !validTitle(out["TITLE_ID"]) {
		return nil, fmt.Errorf("PS3 PARAM.SFO has no valid title identity")
	}
	return out, nil
}
func extraInstallRoot(nand string, b ExtraBundle) string {
	return filepath.Join(nand, "game", filepath.FromSlash(b.InstallDirectory))
}
func extraTitleRoot(nand, title string) string { return filepath.Join(nand, "game", title) }
func installDirPrefix(title string, install string) []string {
	if install == title {
		return nil
	}
	return strings.Split(strings.TrimPrefix(install, title+"/"), "/")
}
func parentDirectories(name string) []string {
	var xs []string
	for p := path.Dir(name); p != "."; p = path.Dir(p) {
		xs = append(xs, p)
	}
	return xs
}
