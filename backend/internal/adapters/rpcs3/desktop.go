package rpcs3

import (
	"fmt"
	"path/filepath"
	"strings"
)

// desktopEntry contains only the configured executable and the verified native
// EBOOT path. ES-DE's desktop parser expands %% back to %, then executes the
// resulting command through a shell, so both formats must be escaped.
func desktopEntry(executable, eboot, title string) (string, error) {
	if !filepath.IsAbs(executable) || filepath.Base(executable) != "rpcs3.AppImage" ||
		!filepath.IsAbs(eboot) || filepath.Base(eboot) != "EBOOT.BIN" || strings.ContainsAny(executable+eboot, "\x00\r\n\t") {
		return "", fmt.Errorf("unsafe RPCS3 desktop launch path")
	}
	if len(title) != 9 || strings.ContainsAny(title, "\x00\r\n") {
		return "", fmt.Errorf("invalid RPCS3 launcher identity")
	}
	quote := func(path string) string {
		return strings.ReplaceAll("'"+strings.ReplaceAll(path, "'", "'\\''")+"'", "%", "%%")
	}
	return "[Desktop Entry]\nType=Application\nName=RPCS3 " + title + "\nExec=" + quote(executable) + " " + quote(eboot) + "\nTerminal=false\n", nil
}
