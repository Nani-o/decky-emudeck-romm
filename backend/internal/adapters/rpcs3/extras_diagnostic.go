package rpcs3

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Keep only a bounded diagnostic outside the disposable extraction. RPCS3 logs
// package crypto fields, so no raw native log may escape this private filter.
func (i *Installer) recordStageDiagnostic(b ExtraBundle, logPath string, cause error) {
	lines := []string{}
	if logPath != "" {
		if info, err := os.Lstat(logPath); err == nil && info.Mode().IsRegular() {
			if f, err := os.Open(logPath); err == nil {
				offset := info.Size() - (32 << 10)
				if offset < 0 {
					offset = 0
				}
				data, _ := io.ReadAll(io.NewSectionReader(f, offset, 32<<10))
				f.Close()
				secret := regexp.MustCompile(`(?i)key|license|\brap\b|ticket|npdrm|digest`)
				useful := regexp.MustCompile(`(?i)error|failed|successfully installed|path:|title.id|app.ver|category|install.dir|package failed`)
				for _, line := range strings.Split(string(data), "\n") {
					if !secret.MatchString(line) && useful.MatchString(line) {
						if len(line) > 1024 {
							line = line[:1024]
						}
						lines = append(lines, line)
					}
				}
				if len(lines) > 24 {
					lines = lines[len(lines)-24:]
				}
			}
		}
	}
	_ = savePrivateJSON(filepath.Join(i.config.IntentDir, b.Identity.TitleID+".last-stage-error.json"), map[string]any{"source_file_id": b.SourceFileID, "kind": b.Kind, "app_version": b.AppVersion, "reason": cause.Error(), "filtered_native_log": lines}, false)
}
