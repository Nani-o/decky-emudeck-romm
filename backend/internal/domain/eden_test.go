package domain

import "testing"

func TestEdenRuleRecognizesInjectedESDECommandAndProcesses(t *testing.T) {
	e, ok := EmulatorByID("eden")
	if !ok || e.Location != "Eden.AppImage" || !e.SaveSync() {
		t.Fatalf("emulator %+v", e)
	}
	l := GameLauncher{ESDECommand: ESDECommand{Command: "%INJECT%=%BASENAME%.esprefix %EMULATOR_EDEN% -f -g %ROM%", Label: "Eden (Standalone)"}, Known: true}
	r, ok := l.SaveRule()
	if !ok || r.Kind != SaveSwitchFolder || r.Tag != "eden" {
		t.Fatalf("rule %+v", r)
	}
	for _, exe := range []string{"/home/deck/Applications/Eden.AppImage", "/tmp/.mount_eden/usr/bin/eden", "/tmp/.mount_eden/usr/bin/eden-qt"} {
		if !e.Runs(Process{Args: []string{exe}}) {
			t.Fatalf("missed %s", exe)
		}
	}
	if e.Runs(Process{Args: []string{"/usr/bin/something-else"}}) {
		t.Fatal("unrelated process matched")
	}
}
