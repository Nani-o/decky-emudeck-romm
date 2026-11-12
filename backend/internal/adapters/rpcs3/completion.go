package rpcs3

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// This identifies only the observed RPCS3 0.0.43 teardown failure. The installer
// still requires every normal postcondition plus fresh native completion proof.
type headlessTeardownError struct{}

func (*headlessTeardownError) Error() string {
	return "RPCS3 hit its known headless process-teardown verification failure"
}

func knownHeadlessTeardown(err, contextErr error, stderr string) bool {
	var exit *exec.ExitError
	return contextErr == nil && errors.As(err, &exit) && exit.ExitCode() == 143 &&
		strings.Contains(stderr, "RPCS3: Verification failed (object: 0x0)") &&
		strings.Contains(stderr, "~manual_typemap()") &&
		strings.Contains(stderr, "Emulation object is unavailable (process teardown)")
}

type boundedTail struct {
	limit int
	data  []byte
}

func (b *boundedTail) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.limit {
		b.data = append(b.data[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	if excess := len(b.data) + len(p) - b.limit; excess > 0 {
		copy(b.data, b.data[excess:])
		b.data = b.data[:len(b.data)-excess]
	}
	b.data = append(b.data, p...)
	return n, nil
}

type nativeLogSnapshot struct{ info fs.FileInfo }

func snapshotNativeLog(path string) (nativeLogSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nativeLogSnapshot{}, nil
	}
	if err != nil {
		return nativeLogSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return nativeLogSnapshot{}, fmt.Errorf("native log is not a regular file")
	}
	return nativeLogSnapshot{info: info}, nil
}

func freshNativeInstallSuccess(path string, before nativeLogSnapshot, started time.Time, bundle Bundle) error {
	after, err := snapshotNativeLog(path)
	if err != nil {
		return err
	}
	if after.info == nil || after.info.Size() == 0 || after.info.ModTime().Before(started) {
		return fmt.Errorf("native completion log is absent or stale")
	}
	var offset int64
	if before.info != nil && os.SameFile(before.info, after.info) {
		if after.info.Size() == before.info.Size() {
			return fmt.Errorf("native completion log was not replaced or appended")
		}
		if after.info.Size() > before.info.Size() {
			offset = before.info.Size()
		}
	}
	const bound int64 = 64 << 10
	if after.info.Size()-offset > bound {
		offset = after.info.Size() - bound
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, after.info) || opened.Size() != after.info.Size() {
		return fmt.Errorf("native completion log changed while opening")
	}
	data, err := io.ReadAll(io.NewSectionReader(file, offset, after.info.Size()-offset))
	if err != nil {
		return err
	}
	// Match a native success record, not a diagnostic that merely quotes a path.
	prefix := regexp.MustCompile(`^·S [0-9]+:[0-9]{2}:[0-9]{2}\.[0-9]{6} GUI: Successfully installed `)
	expected := bundle.PackagePath + " (title_id=" + bundle.Identity.TitleID + ","
	for _, line := range strings.Split(string(data), "\n") {
		if prefix.MatchString(line) && strings.HasPrefix(prefix.ReplaceAllString(line, ""), expected) {
			return nil
		}
	}
	return fmt.Errorf("no fresh native package completion record")
}

func verifyPackageEntryCount(title string, expected uint32) error {
	if expected == 0 {
		return fmt.Errorf("package entry count is unavailable")
	}
	var count uint64
	err := filepath.WalkDir(title, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == title {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("installed package contains a symbolic link")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("installed package contains a special file")
		}
		count++
		if count > uint64(expected) {
			return fmt.Errorf("installed package has extra entries")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if count != uint64(expected) {
		return fmt.Errorf("installed package is incomplete")
	}
	return nil
}
