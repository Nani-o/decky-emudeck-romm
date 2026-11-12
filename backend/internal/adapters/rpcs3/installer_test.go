package rpcs3

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

type runnerFunc func(context.Context, string, ...string) error

func (fn runnerFunc) Run(ctx context.Context, executable string, args ...string) error {
	return fn(ctx, executable, args...)
}

func makeInstaller(t *testing.T, runner Runner, processes []domain.Process, processErr error) (*Installer, string, string) {
	t.Helper()
	root := t.TempDir()
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(physicalRoot, "home", "deck")
	configDir := filepath.Join(home, ".config", "rpcs3")
	devHDD0 := filepath.Join(home, "Emulation", "storage", "rpcs3", "dev_hdd0")
	if err := os.MkdirAll(filepath.Join(configDir, "GuiConfigs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(devHDD0, "home", "00000001"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "vfs.yml"), []byte("/dev_hdd0/: "+devHDD0+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "GuiConfigs", "persistent_settings.dat"), []byte("[Users]\nactive_user=00000001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	appImage := filepath.Join(home, "Applications", "rpcs3.AppImage")
	if err := os.MkdirAll(filepath.Dir(appImage), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(appImage, []byte("injected validation means no real AppImage is needed"), 0o700); err != nil {
		t.Fatal(err)
	}
	intentDir := filepath.Join(physicalRoot, "private", "ps3-install-intents")
	config, err := (Config{
		AppImagePath: appImage,
		ConfigDir:    configDir,
		HomeDir:      home,
		IntentDir:    intentDir,
		ProcRoot:     filepath.Join(root, "proc"),
		Timeout:      time.Minute,
	}).normalized()
	if err != nil {
		t.Fatal(err)
	}
	installer := &Installer{
		config: config,
		runner: runner,
		processes: func() ([]domain.Process, error) {
			return processes, processErr
		},
		validateExecutable: func(string) error { return nil },
		now:                func() time.Time { return time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC) },
	}
	return installer, devHDD0, configDir
}

func TestInstallUsesQualifiedHeadlessCommandAndVerifiesWithoutTouchingSaves(t *testing.T) {
	staging, rap := makeTestBundle(t)
	var invocations int
	var installer *Installer
	var devHDD0, configDir string
	installer, devHDD0, configDir = makeInstaller(t, runnerFunc(func(ctx context.Context, executable string, args ...string) error {
		invocations++
		if got, want := args, []string{"--headless", "--installpkg", staging}; !reflect.DeepEqual(got, want) {
			t.Fatalf("RPCS3 arguments = %#v, want %#v", got, want)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// RPCS3 creates the active user's exdata directory while installing the RAP.
		if err := os.MkdirAll(filepath.Join(devHDD0, "home", "00000001", "exdata"), 0o700); err != nil {
			return err
		}
		titleDir := filepath.Join(devHDD0, "game", testTitleID)
		if err := os.MkdirAll(filepath.Join(titleDir, "USRDIR"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(titleDir, "PARAM.SFO"), makeTestSFO(testTitleID), 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(titleDir, "USRDIR", "EBOOT.BIN"), []byte("boot executable"), 0o600); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(devHDD0, "home", "00000001", "exdata", testContentID+".rap"), rap, 0o600)
	}), nil, nil)

	// These files model data the installer is explicitly forbidden to modify.
	savePath := filepath.Join(devHDD0, "home", "00000001", "savedata", "preserve.bin")
	firmwarePath := filepath.Join(devHDD0, "dev_flash", "sys", "preserve.bin")
	if err := os.MkdirAll(filepath.Dir(savePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(firmwarePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(savePath, []byte("user save"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firmwarePath, []byte("firmware"), 0o600); err != nil {
		t.Fatal(err)
	}
	vfsBefore, err := os.ReadFile(filepath.Join(configDir, "vfs.yml"))
	if err != nil {
		t.Fatal(err)
	}

	result, err := installer.Install(context.Background(), 77, staging)
	if err != nil {
		t.Fatal(err)
	}
	if invocations != 1 {
		t.Fatalf("RPCS3 invocation count = %d", invocations)
	}
	if result.ROMID != 77 || result.Identity.TitleID != testTitleID || result.RPCS3User != "00000001" || result.VerifiedAt.IsZero() {
		t.Fatalf("unexpected install result: %+v", result)
	}
	intent, found, err := readIntent(installer.config.IntentDir, testTitleID)
	if err != nil || !found || intent.Phase != intentVerified {
		t.Fatalf("intent = %+v, found = %t, error = %v", intent, found, err)
	}
	if got, err := os.ReadFile(savePath); err != nil || string(got) != "user save" {
		t.Fatalf("save data changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(firmwarePath); err != nil || string(got) != "firmware" {
		t.Fatalf("firmware changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(configDir, "vfs.yml")); err != nil || string(got) != string(vfsBefore) {
		t.Fatalf("RPCS3 config changed: %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(staging, testContentID+".rap")); err != nil || !reflect.DeepEqual(got, rap) {
		t.Fatalf("staged license changed: %x, %v", got, err)
	}
}

func TestInstallZeroExitWithoutVerifiedOutputLeavesNeedsReviewAndDoesNotRetry(t *testing.T) {
	staging, _ := makeTestBundle(t)
	var invocations int
	installer, _, _ := makeInstaller(t, runnerFunc(func(context.Context, string, ...string) error {
		invocations++
		return nil
	}), nil, nil)
	if _, err := installer.Install(context.Background(), 77, staging); !errors.Is(err, ErrInstallUnverified) {
		t.Fatalf("expected ErrInstallUnverified, got %v", err)
	}
	intent, found, err := readIntent(installer.config.IntentDir, testTitleID)
	if err != nil || !found || intent.Phase != intentNeedsReview || intent.Reason != "installed_files_failed_verification" {
		t.Fatalf("intent = %+v, found = %t, error = %v", intent, found, err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("staging was removed: %v", err)
	}
	if _, err := installer.Install(context.Background(), 77, staging); !errors.Is(err, ErrIntentNeedsReview) {
		t.Fatalf("expected retry refusal, got %v", err)
	}
	if invocations != 1 {
		t.Fatalf("unsafe retry invoked RPCS3 %d times", invocations)
	}
}

func TestInstallRefusesExistingTitleOrLicenseWithoutCallingRPCS3(t *testing.T) {
	tests := []struct {
		name string
		seed func(*testing.T, string)
		want error
	}{
		{name: "title", seed: func(t *testing.T, root string) {
			if err := os.MkdirAll(filepath.Join(root, "game", testTitleID), 0o700); err != nil {
				t.Fatal(err)
			}
		}, want: ErrTitleAlreadyExists},
		{name: "license", seed: func(t *testing.T, root string) {
			path := filepath.Join(root, "home", "00000001", "exdata", testContentID+".rap")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("foreign-license"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, want: ErrLicenseExists},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staging, _ := makeTestBundle(t)
			calls := 0
			installer, root, _ := makeInstaller(t, runnerFunc(func(context.Context, string, ...string) error {
				calls++
				return nil
			}), nil, nil)
			tt.seed(t, root)
			if _, err := installer.Install(context.Background(), 77, staging); !errors.Is(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, err)
			}
			if calls != 0 {
				t.Fatalf("RPCS3 ran %d times despite existing content", calls)
			}
		})
	}
}

func TestInstallFailsClosedWhenRPCS3IsRunningOrProcessListingFails(t *testing.T) {
	tests := []struct {
		name      string
		processes []domain.Process
		procErr   error
		want      error
	}{
		{name: "running", processes: []domain.Process{{PID: 42, Args: []string{"/home/deck/Applications/rpcs3.AppImage"}}}, want: ErrRPCS3Running},
		{name: "listing error", procErr: errors.New("permission denied"), want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			staging, _ := makeTestBundle(t)
			calls := 0
			installer, _, _ := makeInstaller(t, runnerFunc(func(context.Context, string, ...string) error {
				calls++
				return nil
			}), tt.processes, tt.procErr)
			_, err := installer.Install(context.Background(), 77, staging)
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("expected %v, got %v", tt.want, err)
			}
			if tt.procErr != nil && err == nil {
				t.Fatal("expected process listing error to fail closed")
			}
			if calls != 0 {
				t.Fatalf("RPCS3 ran %d times", calls)
			}
		})
	}
}

func TestInstallRunnerErrorIsDurablyMarkedForReview(t *testing.T) {
	staging, _ := makeTestBundle(t)
	installer, _, _ := makeInstaller(t, runnerFunc(func(context.Context, string, ...string) error {
		return errors.New("simulated process interruption")
	}), nil, nil)
	_, err := installer.Install(context.Background(), 77, staging)
	if !errors.Is(err, ErrInstallUnverified) {
		t.Fatalf("expected ErrInstallUnverified, got %v", err)
	}
	intent, found, readErr := readIntent(installer.config.IntentDir, testTitleID)
	if readErr != nil || !found || intent.Phase != intentNeedsReview || intent.Reason != "rpcs3_command_failed" {
		t.Fatalf("intent = %+v, found = %t, error = %v", intent, found, readErr)
	}
}

func TestTwoInstallersCannotRunConcurrentRPCS3InstallationsForOneTitle(t *testing.T) {
	staging, rap := makeTestBundle(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var first *Installer
	var devHDD0 string
	first, devHDD0, _ = makeInstaller(t, runnerFunc(func(context.Context, string, ...string) error {
		close(started)
		<-release
		return writeTestRPCS3Install(devHDD0, rap)
	}), nil, nil)
	secondInvocations := 0
	second := &Installer{
		config:             first.config,
		processes:          first.processes,
		validateExecutable: first.validateExecutable,
		now:                first.now,
		runner: runnerFunc(func(context.Context, string, ...string) error {
			secondInvocations++
			return nil
		}),
	}
	type installOutcome struct {
		result domain.PS3InstallResult
		err    error
	}
	firstDone := make(chan installOutcome, 1)
	go func() {
		result, err := first.Install(context.Background(), 77, staging)
		firstDone <- installOutcome{result: result, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("first installer did not reach the runner")
	}
	if _, err := second.Install(context.Background(), 77, staging); !errors.Is(err, ErrIntentNeedsReview) {
		close(release)
		t.Fatalf("second installer should refuse the active durable intent, got %v", err)
	}
	if secondInvocations != 0 {
		close(release)
		t.Fatalf("second RPCS3 process launched %d times", secondInvocations)
	}
	close(release)
	select {
	case outcome := <-firstDone:
		if outcome.err != nil || outcome.result.Identity.TitleID != testTitleID {
			t.Fatalf("first install outcome = %+v, error = %v", outcome.result, outcome.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first installer did not finish after release")
	}
}

func writeTestRPCS3Install(devHDD0 string, rap []byte) error {
	if err := os.MkdirAll(filepath.Join(devHDD0, "home", "00000001", "exdata"), 0o700); err != nil {
		return err
	}
	titleDir := filepath.Join(devHDD0, "game", testTitleID)
	if err := os.MkdirAll(filepath.Join(titleDir, "USRDIR"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(titleDir, "PARAM.SFO"), makeTestSFO(testTitleID), 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(titleDir, "USRDIR", "EBOOT.BIN"), []byte("boot executable"), 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(devHDD0, "home", "00000001", "exdata", testContentID+".rap"), rap, 0o600)
}
