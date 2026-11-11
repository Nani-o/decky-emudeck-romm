package romm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

type edenTransport func(*http.Request) (*http.Response, error)

func (f edenTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEdenNegotiationSerializesOnlyItsEmulatorAndNativeScope(t *testing.T) {
	c := New(domain.Server{URL: "https://romm.nani"}, "synthetic-token", ver)
	c.bulk = &http.Client{Transport: edenTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.URL.String() != "https://romm.nani/api/sync/negotiate" {
			t.Fatalf("request %s %s", r.Method, r.URL)
		}
		var got struct {
			Device    string   `json:"device_id"`
			Emulators []string `json:"emulators"`
			ROMIDs    []int    `json:"rom_ids"`
			Saves     []struct {
				Slot     string `json:"slot"`
				Emulator string `json:"emulator"`
			} `json:"saves"`
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got.Device != "deck-1" || len(got.Emulators) != 1 || got.Emulators[0] != "eden" || len(got.ROMIDs) != 1 || got.ROMIDs[0] != 319 || got.Saves[0].Slot != "eden" || got.Saves[0].Emulator != "eden" {
			t.Fatalf("payload %+v", got)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"session_id":1,"operations":[]}`))}, nil
	})}
	_, err := c.NegotiateSync(context.Background(), domain.SyncRequest{DeviceID: "deck-1", Emulators: []string{"eden"}, ROMIDs: []int{319}, Saves: []domain.ClientSave{{ROMID: 319, FileName: "0100EA80032EA000.zip", Slot: "eden", Emulator: "eden"}}})
	if err != nil {
		t.Fatal(err)
	}
}
