package app

import (
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"unicode"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
	"github.com/nwaples/rardecode/v2"
)

// multipartRAR accepts exactly one complete, consecutive .partN.rar set. No
// other game files may be mixed into it; updates/DLC were already filtered out.
func multipartRAR(files []domain.ROMFile) (int, error) {
	if len(files) <= 1 {
		return -1, nil
	}
	anyRAR := false
	for _, f := range files {
		anyRAR = anyRAR || strings.EqualFold(filepath.Ext(f.Name), ".rar")
	}
	if !anyRAR {
		return -1, nil
	}
	pattern := regexp.MustCompile(`^(.*)\.part([0-9]+)\.rar$`)
	width := 0
	for _, f := range files {
		match := pattern.FindStringSubmatch(f.Name)
		if match != nil && len(match[2]) > 1 && match[2][0] == '0' {
			if width != 0 && width != len(match[2]) {
				return -1, badArchive(fmt.Errorf("mixed RAR volume padding"))
			}
			width = len(match[2])
		}
	}
	seen := map[int]bool{}
	stem, parent, first := "", "", -1
	for index, f := range files {
		match := pattern.FindStringSubmatch(f.Name)
		rel := f.Path
		if rel == "" {
			rel = f.Name
		}
		if match == nil || filepath.Base(filepath.FromSlash(rel)) != f.Name {
			return -1, badArchive(fmt.Errorf("ambiguous RAR volume set"))
		}
		number, err := strconv.Atoi(match[2])
		if err != nil || number < 1 || number > len(files) || seen[number] {
			return -1, badArchive(fmt.Errorf("missing or duplicate RAR volume"))
		}
		p := filepath.Dir(filepath.FromSlash(rel))
		if index == 0 {
			stem, parent = match[1], p
		}
		digits := strconv.Itoa(number)
		if width > 0 {
			digits = fmt.Sprintf("%0*d", width, number)
		}
		if match[1] != stem || p != parent || match[2] != digits {
			return -1, badArchive(fmt.Errorf("mixed RAR volume set"))
		}
		seen[number] = true
		if number == 1 {
			first = index
		}
	}
	if first < 0 {
		return -1, badArchive(fmt.Errorf("first RAR volume is absent"))
	}
	return first, nil
}

// Only exact files downloaded by this job can be opened as RAR volumes. The
// decoder never receives the OS filesystem, so guessed .r00 or other siblings
// outside this list cannot be read. Root anchoring and O_NOFOLLOW reject links.
type rarVolumeFS struct {
	root   *os.Root
	files  map[string]os.FileInfo
	opened map[string]bool
}

func (v *rarVolumeFS) Open(name string) (fs.File, error) {
	expected, ok := v.files[name]
	if !ok || !fs.ValidPath(name) || filepath.Base(name) != name {
		return nil, fs.ErrNotExist
	}
	f, err := v.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !independentRARFile(actual) || !os.SameFile(actual, expected) || actual.Size() != expected.Size() || !actual.ModTime().Equal(expected.ModTime()) {
		_ = f.Close()
		return nil, fmt.Errorf("RAR volume changed or is redirected")
	}
	v.opened[name] = true
	return f, nil
}

func independentRARFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() {
		return false
	}
	native, ok := info.Sys().(*syscall.Stat_t)
	return ok && native.Nlink == 1
}

func newRARVolumeFS(first string, volumes []string) (*rarVolumeFS, error) {
	if len(volumes) == 0 {
		volumes = []string{first}
	}
	parent, err := filepath.Abs(filepath.Dir(first))
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, fmt.Errorf("RAR staging path is redirected")
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, err
	}
	anchor, err := root.Stat(".")
	expected, statErr := os.Stat(parent)
	if err != nil || statErr != nil || !os.SameFile(anchor, expected) {
		_ = root.Close()
		return nil, fmt.Errorf("RAR staging directory changed")
	}
	v := &rarVolumeFS{root: root, files: map[string]os.FileInfo{}, opened: map[string]bool{}}
	found := false
	for _, path := range volumes {
		absolute, err := filepath.Abs(path)
		if err != nil || filepath.Dir(absolute) != parent {
			_ = root.Close()
			return nil, fmt.Errorf("RAR volumes must share their owned staging directory")
		}
		name := filepath.Base(absolute)
		if _, duplicate := v.files[name]; duplicate {
			_ = root.Close()
			return nil, fmt.Errorf("duplicate RAR volume")
		}
		info, err := root.Lstat(name)
		if err != nil || !independentRARFile(info) {
			_ = root.Close()
			return nil, fmt.Errorf("RAR volume is missing or redirected")
		}
		v.files[name] = info
		if absolute == filepath.Join(parent, filepath.Base(first)) {
			found = true
		}
	}
	if !found {
		_ = root.Close()
		return nil, fmt.Errorf("first RAR volume was not downloaded by this job")
	}
	return v, nil
}

func rarMemberPath(h rardecode.FileHeader) (string, error) {
	if h.Encrypted || h.HeaderEncrypted || h.LinkType != 0 || h.LinkTarget != "" || h.UnKnownSize || h.UnPackedSize < 0 || (!h.IsDir && !h.Mode().IsRegular()) {
		return "", fmt.Errorf("encrypted, linked, special or unknown-size RAR member")
	}
	if strings.ContainsAny(h.Name, ":`|") || strings.IndexFunc(h.Name, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("unsafe RAR member name")
	}
	return memberPath(h.Name)
}

// openRAR lists headers without extracting. Each member opens the sequential
// stream at its index, supporting solid archives while retaining checksum checks.
func openRAR(path string, volumes []string) ([]archiveMember, func() error, error) {
	filesystem, err := newRARVolumeFS(path, volumes)
	if err != nil {
		return nil, nil, badArchive(err)
	}
	opts := []rardecode.Option{rardecode.FileSystem(filesystem), rardecode.MaxDictionarySize(512 << 20)}
	files, err := rardecode.List(filepath.Base(path), opts...)
	if err != nil {
		_ = filesystem.root.Close()
		return nil, nil, badArchive(err)
	}
	if len(filesystem.opened) != len(filesystem.files) {
		_ = filesystem.root.Close()
		return nil, nil, badArchive(fmt.Errorf("unused or mismatched RAR volume"))
	}
	members := make([]archiveMember, 0, len(files))
	seen := map[string]bool{}
	names := make([]string, 0, len(files))
	totalSize := int64(0)
	for _, f := range files {
		rel, err := rarMemberPath(f.FileHeader)
		if err != nil {
			_ = filesystem.root.Close()
			return nil, nil, badArchive(err)
		}
		if f.IsDir {
			continue
		}
		if f.UnPackedSize > math.MaxInt64-totalSize {
			_ = filesystem.root.Close()
			return nil, nil, badArchive(fmt.Errorf("RAR size overflows disk reservation"))
		}
		totalSize += f.UnPackedSize
		key := strings.ToLower(filepath.ToSlash(rel))
		if seen[key] {
			_ = filesystem.root.Close()
			return nil, nil, badArchive(fmt.Errorf("duplicate RAR member"))
		}
		for _, old := range names {
			if strings.HasPrefix(key, old+"/") || strings.HasPrefix(old, key+"/") {
				_ = filesystem.root.Close()
				return nil, nil, badArchive(fmt.Errorf("conflicting RAR member paths"))
			}
		}
		seen[key], names = true, append(names, key)
	}
	var reader *rardecode.ReadCloser
	next := 0
	closeAll := func() error {
		if reader != nil {
			_ = reader.Close()
			reader = nil
		}
		return filesystem.root.Close()
	}
	for index, f := range files {
		if f.IsDir {
			continue
		}
		index, header := index, f.FileHeader
		members = append(members, archiveMember{Name: header.Name, Size: header.UnPackedSize, open: func() (io.ReadCloser, error) {
			if reader == nil || index < next {
				if reader != nil {
					_ = reader.Close()
				}
				reader, err = rardecode.OpenReader(filepath.Base(path), opts...)
				next = 0
				if err != nil {
					return nil, err
				}
			}
			for next <= index {
				actual, e := reader.Next()
				if e != nil {
					return nil, e
				}
				if next == index {
					if _, e := rarMemberPath(*actual); e != nil {
						return nil, e
					}
					if actual.Name != header.Name || actual.UnPackedSize != header.UnPackedSize {
						return nil, fmt.Errorf("RAR header changed")
					}
				}
				next++
			}
			return io.NopCloser(reader), nil
		}})
	}
	return members, closeAll, nil
}

// updateRARVolumes groups numbered patch archives without mixing their volumes
// with base-game or other downloaded content. Only this job's manifest is used.
func updateRARVolumes(files []domain.PlannedFile) (map[int][]int, map[int]bool, error) {
	pattern := regexp.MustCompile(`(?i)^(.*)\.part([0-9]+)\.rar$`)
	groups := map[string][]int{}
	for i, f := range files {
		if f.File.Category != "update" {
			continue
		}
		match := pattern.FindStringSubmatch(f.File.Name)
		if match != nil {
			key := filepath.Dir(f.File.Path) + "/" + match[1]
			groups[key] = append(groups[key], i)
		}
	}
	firsts, other := map[int][]int{}, map[int]bool{}
	for _, indices := range groups {
		group := make([]domain.ROMFile, 0, len(indices))
		for _, i := range indices {
			group = append(group, files[i].File)
		}
		first, err := multipartRAR(group)
		if err != nil {
			return nil, nil, err
		}
		if first < 0 {
			first = 0
		}
		firsts[files[indices[first]].File.ID] = indices
		for n, i := range indices {
			if n != first {
				other[files[i].File.ID] = true
			}
		}
	}
	return firsts, other, nil
}
