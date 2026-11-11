package app

// Eden support is deliberately opt-in: a local binding selects one account/device,
// one NAND profile and an explicit set of base title ids. No profile guessing.
import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

const EdenSlot = "eden"
const edenMaxBytes = 64 << 20
const edenMaxFiles = 4096
const edenUnmappedWhyCode = "eden_unmapped"

var errEdenUnmapped = errors.New("This ROM has no explicit Eden title mapping")

type edenBinding struct {
	Owner     string            `json:"owner"`
	Schema    int               `json:"schema"`
	ServerURL string            `json:"server_url"`
	DeviceID  string            `json:"device_id"`
	NANDPath  string            `json:"nand_path"`
	ProfileID string            `json:"profile_id"`
	Games     map[string]string `json:"games"`
}

type EdenSaves struct{ home, settings string }

func NewEdenSaves(home, settings string) *EdenSaves {
	return &EdenSaves{home: home, settings: settings}
}

var edenProfile = regexp.MustCompile(`^[0-9A-F]{32}$`)
var edenTitle = regexp.MustCompile(`^010[0-9A-F]{10}000$`)

// safeEdenPath checks every descendant, allowing an absent final target only.
// Resolved EmuDeck roots may be supplied explicitly, but no links inside them.
func safeEdenPath(root, target string, absent bool) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	real, err := filepath.EvalSymlinks(root)
	if err != nil || real != root {
		return errors.New("Eden root must be a canonical directory")
	}
	for _, part := range strings.Split(target, string(filepath.Separator)) {
		if part == ".dotfiles" {
			return errors.New("Eden path enters dotfiles")
		}
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || !filepath.IsLocal(rel) {
		return errors.New("Eden path escapes its root")
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return err
	}
	rootStat, ok := rootInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("Eden ownership could not be read")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	here := root
	for i, part := range parts {
		here = filepath.Join(here, part)
		st, err := os.Lstat(here)
		if absent && errors.Is(err, os.ErrNotExist) && i == len(parts)-1 {
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 || (!st.IsDir() && !st.Mode().IsRegular()) {
			return errors.New("unsafe Eden path type")
		}
		if i < len(parts)-1 && !st.IsDir() {
			return errors.New("Eden parent is not a directory")
		}
		if st.Mode().Perm()&0o022 != 0 {
			return errors.New("Eden path is writable by another user")
		}
		if x, ok := st.Sys().(*syscall.Stat_t); ok && st.Mode().IsRegular() && x.Nlink != 1 {
			return errors.New("Eden file has hard links")
		}
		if x, ok := st.Sys().(*syscall.Stat_t); !ok || (x.Uid != 0 && x.Uid != rootStat.Uid) {
			return errors.New("Eden path has foreign ownership")
		}
	}
	return nil
}

func readEdenFile(root, file string, limit int64) ([]byte, error) {
	if err := safeEdenPath(root, file, false); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(file, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, errors.New("Eden file is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	current, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if len(data) > int(limit) || !sameEdenFile(before, after) || !sameEdenFile(after, current) {
		return nil, errors.New("Eden file changed while reading")
	}
	return data, nil
}

func sameEdenFile(a, b fs.FileInfo) bool {
	if !os.SameFile(a, b) || a.Mode() != b.Mode() || a.Size() != b.Size() || !a.ModTime().Equal(b.ModTime()) {
		return false
	}
	// Reading may update atime. Identity, ownership, links, mtime and ctime may not drift.
	x, y := reflect.Indirect(reflect.ValueOf(a.Sys())), reflect.Indirect(reflect.ValueOf(b.Sys()))
	for _, name := range []string{"Dev", "Ino", "Mode", "Uid", "Gid", "Nlink", "Size", "Mtim", "Mtimespec", "Ctim", "Ctimespec"} {
		u, v := x.FieldByName(name), y.FieldByName(name)
		if u.IsValid() != v.IsValid() {
			return false
		}
		if u.IsValid() && !reflect.DeepEqual(u.Interface(), v.Interface()) {
			return false
		}
	}
	return true
}

// rejectDuplicateJSON also rejects trailing JSON values before struct decoding.
func rejectDuplicateJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func() error
	value = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok || seen[key] {
					return errors.New("duplicate JSON key")
				}
				seen[key] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for d.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = d.Token()
		return err
	}
	if err := value(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func (e *EdenSaves) binding() (edenBinding, error) {
	var b edenBinding
	if e == nil || e.settings == "" {
		return b, errors.New("Eden save binding is not configured")
	}
	data, err := readEdenFile(e.home, filepath.Join(e.settings, "eden-saves.json"), 64<<10)
	if err != nil {
		return b, fmt.Errorf("Eden save binding: %w", err)
	}
	configInfo, err := os.Lstat(filepath.Join(e.settings, "eden-saves.json"))
	if err != nil {
		return b, err
	}
	if configInfo.Mode().Perm() != 0o600 {
		return b, errors.New("Eden binding must be private (0600)")
	}
	if err = rejectDuplicateJSON(data); err != nil {
		return b, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&b); err != nil {
		return b, err
	}
	u, err := url.Parse(b.ServerURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || b.Schema != 1 || b.Owner != "homelab-romm-eden-v1" || b.DeviceID == "" || len(b.DeviceID) > 128 || !edenProfile.MatchString(b.ProfileID) || len(b.Games) == 0 || len(b.Games) > 500 {
		return b, errors.New("invalid Eden save binding")
	}
	if b.NANDPath != filepath.Join(e.home, "Emulation/storage/eden/nand") {
		return b, errors.New("Eden NAND is outside the qualified location")
	}
	seen := map[string]bool{}
	for id, title := range b.Games {
		n, err := strconv.Atoi(id)
		if err != nil || n <= 0 || strconv.Itoa(n) != id || !edenTitle.MatchString(title) || seen[title] {
			return b, errors.New("invalid or ambiguous Eden title mapping")
		}
		seen[title] = true
	}
	if err = safeEdenPath(e.home, b.NANDPath, false); err != nil {
		return b, err
	}
	config, err := readEdenFile(e.home, filepath.Join(e.home, ".config/eden/qt-config.ini"), 2<<20)
	if err != nil {
		return b, err
	}
	if err = qualifyEdenNAND(config, b.NANDPath); err != nil {
		return b, err
	}
	return b, nil
}

func qualifyEdenNAND(data []byte, expected string) error {
	section := ""
	groups := 0
	systemGroups := 0
	keys := map[string]string{}
	userKeys := map[string]string{}
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 4096), 2<<20)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return errors.New("unsupported Eden INI section")
			}
			var err error
			section, err = url.PathUnescape(line[1 : len(line)-1])
			if err != nil {
				return err
			}
			if section == "Data Storage" {
				groups++
			}
			if section == "System" {
				systemGroups++
			}
			continue
		}
		if section != "Data Storage" && section != "System" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if section == "System" {
			if k != "current_user" && k != `current_user\default` {
				continue
			}
			if !ok {
				return errors.New("invalid Eden user setting")
			}
			if _, exists := userKeys[k]; exists {
				return errors.New("duplicate Eden user setting")
			}
			userKeys[k] = strings.TrimSpace(v)
			continue
		}
		if k != "nand_directory" && k != `nand_directory\default` {
			continue
		}
		if !ok {
			return errors.New("invalid Eden NAND setting")
		}
		if _, exists := keys[k]; exists {
			return errors.New("duplicate Eden NAND setting")
		}
		keys[k] = strings.TrimSpace(v)
	}
	if s.Err() != nil || groups != 1 || keys["nand_directory"] != expected || keys[`nand_directory\default`] != "false" {
		return errors.New("Eden active NAND is not qualified")
	}
	if systemGroups != 1 || userKeys["current_user"] != "0" || userKeys[`current_user\default`] != "true" {
		return errors.New("Eden current user must remain the explicitly qualified index 0")
	}
	return nil
}

func (e *EdenSaves) basePath() (string, error) {
	b, err := e.binding()
	if err != nil {
		return "", err
	}
	return filepath.Join(b.NANDPath, "user/save"), nil
}

func (e *EdenSaves) target(server, device string, id int) (string, string, error) {
	b, err := e.binding()
	if err != nil {
		return "", "", err
	}
	if server != b.ServerURL || device != b.DeviceID {
		return "", "", errors.New("Eden binding does not belong to this signed-in device/server")
	}
	title, ok := b.Games[strconv.Itoa(id)]
	if !ok {
		return "", "", errEdenUnmapped
	}
	dir := filepath.Join(b.NANDPath, "user/save/0000000000000000", b.ProfileID, title)
	if err = safeEdenPath(e.home, dir, true); err != nil {
		return "", "", err
	}
	return dir, title, nil
}

type edenFolder struct {
	root, home string
	guard      func() error
	rename     func(string, string) error
}
type edenEntry struct {
	name string
	data []byte
	info fs.FileInfo
}

func (f edenFolder) entries() ([]edenEntry, time.Time, error) {
	if err := f.guard(); err != nil {
		return nil, time.Time{}, err
	}
	if err := safeEdenPath(f.home, f.root, true); err != nil {
		return nil, time.Time{}, err
	}
	if _, err := os.Lstat(f.root); errors.Is(err, os.ErrNotExist) {
		return nil, time.Time{}, nil
	}
	var entries []edenEntry
	var newest time.Time
	var size int64
	err := filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = safeEdenPath(f.home, p, false); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(f.root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if !safeEdenMember(name) {
			return errors.New("unsafe Eden save filename")
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		size += info.Size()
		if size > edenMaxBytes || len(entries) >= edenMaxFiles {
			return errors.New("Eden save exceeds its bound")
		}
		data, err := readEdenFile(f.home, p, edenMaxBytes)
		if err != nil {
			return err
		}
		entries = append(entries, edenEntry{name, data, info})
		if info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	if err != nil {
		return nil, newest, err
	}
	// A second metadata walk detects additions, removals and replacements mid-snapshot.
	seen := map[string]fs.FileInfo{}
	for _, e := range entries {
		seen[e.name] = e.info
	}
	err = filepath.WalkDir(f.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(f.root, p)
		name := filepath.ToSlash(rel)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		old, ok := seen[name]
		if !ok || !sameEdenFile(old, info) {
			return errors.New("Eden save changed during snapshot")
		}
		delete(seen, name)
		return nil
	})
	if err != nil || len(seen) > 0 {
		return nil, newest, errors.New("Eden save changed during snapshot")
	}
	if err = f.guard(); err != nil {
		return nil, newest, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, newest, nil
}

func safeEdenMember(n string) bool {
	return n != "" && !strings.ContainsAny(n, "\\\x00\r\n:") && !strings.HasPrefix(n, "/") && path.Clean(n) == n && n != "." && n != ".." && !strings.HasPrefix(n, "../") && !strings.HasSuffix(n, "/")
}

func (f edenFolder) read() (localSave, error) {
	es, mod, err := f.entries()
	if err != nil || len(es) == 0 {
		return localSave{}, err
	}
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, e := range es {
		h := &zip.FileHeader{Name: e.name, Method: zip.Store, Modified: time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)}
		h.SetMode(0o600)
		w, err := z.CreateHeader(h)
		if err != nil {
			return localSave{}, err
		}
		if _, err = w.Write(e.data); err != nil {
			return localSave{}, err
		}
	}
	if err = z.Close(); err != nil {
		return localSave{}, err
	}
	if b.Len() > edenMaxBytes {
		return localSave{}, errors.New("Eden archive exceeds its bound")
	}
	data := b.Bytes()
	return localSave{exists: true, data: data, hash: domain.ContentHash(data), mod: mod.UTC()}, nil
}

func (f edenFolder) files() []string {
	es, _, err := f.entries()
	if err != nil {
		return nil
	}
	out := []string{}
	for _, e := range es {
		out = append(out, filepath.Join(f.root, filepath.FromSlash(e.name)))
	}
	return out
}

func parseEdenArchive(data []byte) ([]edenEntry, error) {
	if len(data) > edenMaxBytes {
		return nil, errors.New("Eden archive exceeds its bound")
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	if len(z.File) == 0 || len(z.File) > edenMaxFiles {
		return nil, errors.New("invalid Eden archive entry count")
	}
	var out []edenEntry
	seen := map[string]bool{}
	var total uint64
	for _, v := range z.File {
		if !safeEdenMember(v.Name) || seen[v.Name] || v.Mode()&os.ModeType != 0 || v.Flags&1 != 0 {
			return nil, errors.New("unsafe Eden archive entry")
		}
		seen[v.Name] = true
		total += v.UncompressedSize64
		if v.UncompressedSize64 > edenMaxBytes || total > edenMaxBytes {
			return nil, errors.New("Eden archive exceeds its bound")
		}
		r, err := v.Open()
		if err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(r, edenMaxBytes+1))
		closeErr := r.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if uint64(len(b)) != v.UncompressedSize64 {
			return nil, errors.New("invalid Eden archive member size")
		}
		out = append(out, edenEntry{name: v.Name, data: b})
	}
	for n := range seen {
		for p := path.Dir(n); p != "."; p = path.Dir(p) {
			if seen[p] {
				return nil, errors.New("Eden archive file/directory collision")
			}
		}
	}
	return out, nil
}

func edenSyncDir(p string) error {
	d, err := os.Open(p)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func edenWriteNew(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func edenRecoveryReady(home, root string) error {
	parent := filepath.Dir(root)
	if err := safeEdenPath(home, parent, false); err != nil {
		return err
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".romm-eden-restore-") {
			continue
		}
		op := filepath.Join(parent, entry.Name())
		if err = safeEdenPath(home, op, false); err != nil {
			return err
		}
		if _, err = os.Lstat(filepath.Join(op, "completed")); err == nil {
			marker, err := readEdenFile(home, filepath.Join(op, "completed"), 64)
			if err != nil || string(marker) != "complete\n" {
				return errors.New("Unknown Eden restore marker; review required")
			}
			intent, err := readEdenFile(home, filepath.Join(op, "intent.json"), 4096)
			if err != nil {
				return err
			}
			var journal struct {
				Schema int    `json:"schema"`
				Target string `json:"target"`
			}
			if json.Unmarshal(intent, &journal) != nil || journal.Schema != 1 || !edenTitle.MatchString(journal.Target) {
				return errors.New("Unknown Eden restore intent; review required")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return errors.New("An incomplete Eden restore needs review; staged and previous trees are preserved")
	}
	return nil
}

// write leaves a durable intent plus the previous tree beside the new tree.
// Failures preserve the operation directory for review; no automatic deletion.
func (f edenFolder) write(data []byte) error {
	homeInfo, err := os.Stat(f.home)
	if err != nil {
		return err
	}
	owner, ok := homeInfo.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() == 0 || uint32(os.Geteuid()) != owner.Uid {
		return errors.New("Eden restores must run as the non-root home owner")
	}
	es, err := parseEdenArchive(data)
	if err != nil {
		return err
	}
	if err = edenRecoveryReady(f.home, f.root); err != nil {
		return err
	}
	before, err := f.read()
	if err != nil {
		return err
	}
	parent := filepath.Dir(f.root)
	if err = safeEdenPath(f.home, parent, false); err != nil {
		return err
	}
	operation, err := os.MkdirTemp(parent, ".romm-eden-restore-")
	if err != nil {
		return err
	}
	stage := filepath.Join(operation, "staged")
	if err = os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	for _, e := range es {
		p := filepath.Join(stage, filepath.FromSlash(e.name))
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err = edenWriteNew(p, e.data); err != nil {
			return err
		}
	}
	if err = filepath.WalkDir(stage, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return edenSyncDir(p)
		}
		return nil
	}); err != nil {
		return err
	}
	if err = edenSyncDir(stage); err != nil {
		return err
	}
	intent, _ := json.Marshal(map[string]any{"schema": 1, "target": filepath.Base(f.root), "previous_hash": before.hash, "new_hash": domain.ContentHash(data), "previous_exists": before.exists})
	if err = edenWriteNew(filepath.Join(operation, "intent.json"), intent); err != nil {
		return err
	}
	if err = edenSyncDir(operation); err != nil {
		return err
	}
	if err = edenSyncDir(parent); err != nil {
		return err
	}
	now, err := f.read()
	if err != nil {
		return err
	}
	if now.exists != before.exists || now.hash != before.hash {
		return errors.New("Eden save changed before replacement; staging preserved")
	}
	if err = f.guard(); err != nil {
		return err
	}
	rename := f.rename
	if rename == nil {
		rename = os.Rename
	}
	if _, err = os.Lstat(f.root); err == nil {
		if err = rename(f.root, filepath.Join(operation, "previous")); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = edenSyncDir(operation); err != nil {
		return err
	}
	if err = edenSyncDir(parent); err != nil {
		return err
	}
	if err = rename(stage, f.root); err != nil {
		return fmt.Errorf("Eden restore needs review; previous and staged trees preserved at %s: %w", operation, err)
	}
	if err = edenSyncDir(operation); err != nil {
		return err
	}
	if err = edenSyncDir(parent); err != nil {
		return err
	}
	written, err := f.read()
	if err != nil {
		return err
	}
	if !written.exists || written.hash != domain.ContentHash(data) {
		return errors.New("Eden restore verification failed; previous tree preserved")
	}
	if err = edenWriteNew(filepath.Join(operation, "completed"), []byte("complete\n")); err != nil {
		return err
	}
	return edenSyncDir(operation)
}
