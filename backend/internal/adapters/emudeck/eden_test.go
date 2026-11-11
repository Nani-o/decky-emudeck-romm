package emudeck

import "testing"

func TestProbeFindsCanonicalEdenAppImage(t *testing.T) {
	d := newDeck(t)
	d.mkdir(d.h("Emulation/roms"))
	d.write(d.h("Applications/Eden.AppImage"), "existing native binary")
	state := d.probe("amd64")
	found := false
	for _, e := range state.Emulators {
		if e.ID == "eden" {
			found = true
			if !e.Installed {
				t.Fatal("canonical Eden.AppImage was not detected")
			}
		}
	}
	if !found {
		t.Fatal("Eden missing from setup probe")
	}
}
