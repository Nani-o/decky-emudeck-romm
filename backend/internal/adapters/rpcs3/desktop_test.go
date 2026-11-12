package rpcs3

import (
	"os/exec"
	"strings"
	"testing"
)

func TestDesktopExecEscapesShellAndDesktopFields(t *testing.T) {
	executable := "/home/deck/test ' $`x` %/rpcs3.AppImage"
	eboot := "/home/deck/a ' $`touch bad` %/EBOOT.BIN"
	entry, err := desktopEntry(executable, eboot, testTitleID)
	if err != nil {
		t.Fatal(err)
	}
	var command string
	for _, line := range strings.Split(entry, "\n") {
		if strings.HasPrefix(line, "Exec=") {
			command = strings.TrimPrefix(line, "Exec=")
		}
	}
	if !strings.Contains(command, "%%") {
		t.Fatal("desktop field escaping absent")
	}
	// Model ES-DE's documented %% decoding, then inspect the shell arguments
	// without executing either the emulator or anything named by the paths.
	command = strings.ReplaceAll(command, "%%", "%")
	out, err := exec.Command("sh", "-c", "set -- "+command+"; printf '%s\\n' \"$@\"").Output()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(out), executable+"\n"+eboot+"\n"; got != want {
		t.Fatalf("args %q want %q", got, want)
	}
	if strings.Contains(command, "--no-gui") {
		t.Fatal("ES-DE provides --no-gui; the shortcut must not duplicate it")
	}
}

func TestDesktopRefusesNonExecutableOrControlPaths(t *testing.T) {
	for _, paths := range [][2]string{{"relative/rpcs3.AppImage", "/h/EBOOT.BIN"}, {"/h/not-rpcs3", "/h/EBOOT.BIN"}, {"/h/rpcs3.AppImage", "/h/a\n/EBOOT.BIN"}, {"/h/rpcs3.AppImage", "relative/EBOOT.BIN"}} {
		if _, err := desktopEntry(paths[0], paths[1], testTitleID); err == nil {
			t.Fatalf("accepted %q", paths)
		}
	}
}
