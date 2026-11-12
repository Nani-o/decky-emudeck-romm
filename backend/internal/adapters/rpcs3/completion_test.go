package rpcs3

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const teardownSignature = "RPCS3: Verification failed (object: 0x0)\n~manual_typemap()\nEmulation object is unavailable (process teardown)"

func TestKnownTeardownRequiresExactExitSignatureAndLiveContext(t *testing.T) {
	exit143 := exec.Command("sh", "-c", "exit 143").Run()
	exit1 := exec.Command("sh", "-c", "exit 1").Run()
	if !knownHeadlessTeardown(exit143, nil, teardownSignature) {
		t.Fatal("qualified teardown not recognized")
	}
	for _, test := range []struct {
		err, ctx error
		text     string
	}{{exit1, nil, teardownSignature}, {exit143, context.Canceled, teardownSignature}, {exit143, nil, "SIGTERM"}, {exit143, nil, strings.ReplaceAll(teardownSignature, "~manual_typemap()", "other")}} {
		if knownHeadlessTeardown(test.err, test.ctx, test.text) {
			t.Fatal("unqualified process failure accepted")
		}
	}
	b := &boundedTail{limit: 16 << 10}
	_, _ = b.Write([]byte(strings.Repeat("x", 32<<10)))
	_, _ = b.Write([]byte(teardownSignature))
	if len(b.data) != 16<<10 || !strings.HasSuffix(string(b.data), teardownSignature) {
		t.Fatal("stderr capture is not bounded or loses its tail")
	}
}
func nativeSuccess(pkg string) []byte {
	return []byte("·S 0:00:00.523762 GUI: Successfully installed " + pkg + " (title_id=" + testTitleID + ", title=Burn Zombie Burn!, version=01.00).\n")
}
func TestKnownTeardownRequiresFreshCompleteNativeInstallation(t *testing.T) {
	for _, kind := range []string{"fresh success", "stale success", "no success", "wrong package", "partial count", "cancelled", "unsigned143", "zero partial"} {
		t.Run(kind, func(t *testing.T) {
			staging, rap := makeTestBundle(t)
			pkg := filepath.Join(staging, "opaque-download.pkg")
			if kind == "partial count" || kind == "zero partial" {
				data, err := os.ReadFile(pkg)
				if err != nil {
					t.Fatal(err)
				}
				data = append(data, make([]byte, 32)...)
				binary.BigEndian.PutUint32(data[20:24], 4)
				binary.BigEndian.PutUint64(data[24:32], uint64(len(data)))
				binary.BigEndian.PutUint64(data[40:48], 128)
				if err := os.WriteFile(pkg, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var root, config string
			installer, r, c := makeInstaller(t, runnerFunc(func(context.Context, string, ...string) error {
				if err := writeTestRPCS3Install(root, rap); err != nil {
					return err
				}
				if kind != "stale success" {
					line := nativeSuccess(pkg)
					if kind == "no success" {
						line = []byte("·E 0:00:00.523762 GUI: Could not install package\n")
					}
					if kind == "wrong package" {
						line = nativeSuccess(pkg + ".different.pkg")
					}
					if err := os.WriteFile(filepath.Join(config, "RPCS3.log"), line, 0o600); err != nil {
						return err
					}
				}
				if kind == "cancelled" {
					cancel()
				}
				if kind == "unsigned143" {
					return exec.Command("sh", "-c", "exit 143").Run()
				}
				if kind == "zero partial" {
					return nil
				}
				return &headlessTeardownError{}
			}), nil, nil)
			root, config = r, c
			if kind == "stale success" {
				log := filepath.Join(config, "RPCS3.log")
				if err := os.WriteFile(log, nativeSuccess(pkg), 0o600); err != nil {
					t.Fatal(err)
				}
				old := time.Now().Add(-time.Hour)
				if err := os.Chtimes(log, old, old); err != nil {
					t.Fatal(err)
				}
			}
			result, err := installer.Install(ctx, 77, staging)
			if kind == "fresh success" {
				if err != nil || result.Identity.TitleID != testTitleID {
					t.Fatalf("fresh native completion rejected: %+v %v", result, err)
				}
			} else {
				if !errors.Is(err, ErrInstallUnverified) {
					t.Fatalf("unsafe completion accepted: %+v %v", result, err)
				}
				intent, ok, readErr := readIntent(installer.config.IntentDir, testTitleID)
				if readErr != nil || !ok || intent.Phase != intentNeedsReview {
					t.Fatalf("missing review fence %+v %v", intent, readErr)
				}
			}
		})
	}
}
func TestPKGStagingRejectsLogRecordInjectionPaths(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bad\n·S 0:00:00.000000 GUI: Successfully installed x")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBundle(root); err == nil {
		t.Fatal("log-injecting path accepted")
	}
}
