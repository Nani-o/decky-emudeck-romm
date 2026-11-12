package rpcs3

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

const maxConfigFile = 1 << 20

// Config points the adapter at the existing RPCS3 AppImage and configuration.
// ConfigDir is the directory containing vfs.yml and GuiConfigs; HomeDir is
// used only to expand the configured VFS root and set the child process HOME.
type Config struct {
	AppImagePath string
	ConfigDir    string
	HomeDir      string
	IntentDir    string
	ProcRoot     string
	Timeout      time.Duration
}

type rpcs3Profile struct {
	DevHDD0 string
	UserID  string
}

func (c Config) normalized() (Config, error) {
	if c.HomeDir == "" {
		var err error
		c.HomeDir, err = os.UserHomeDir()
		if err != nil {
			return Config{}, fmt.Errorf("resolve Deck home directory: %w", err)
		}
	}
	home, err := filepath.Abs(c.HomeDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve Deck home directory: %w", err)
	}
	c.HomeDir = filepath.Clean(home)
	if c.ConfigDir == "" {
		c.ConfigDir = filepath.Join(c.HomeDir, ".config", "rpcs3")
	}
	c.ConfigDir, err = filepath.Abs(c.ConfigDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve RPCS3 config directory: %w", err)
	}
	c.ConfigDir = filepath.Clean(c.ConfigDir)
	if c.AppImagePath == "" {
		c.AppImagePath = filepath.Join(c.HomeDir, "Applications", "rpcs3.AppImage")
	}
	c.AppImagePath, err = filepath.Abs(c.AppImagePath)
	if err != nil {
		return Config{}, fmt.Errorf("resolve RPCS3 AppImage: %w", err)
	}
	c.AppImagePath = filepath.Clean(c.AppImagePath)
	if c.IntentDir == "" {
		return Config{}, fmt.Errorf("PS3 install intent directory is required")
	}
	c.IntentDir, err = filepath.Abs(c.IntentDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve PS3 install intent directory: %w", err)
	}
	c.IntentDir = filepath.Clean(c.IntentDir)
	if c.ProcRoot == "" {
		c.ProcRoot = "/proc"
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Minute
	}
	return c, nil
}

func (c Config) profile() (rpcs3Profile, error) {
	devHDD0, err := readDevHDD0(filepath.Join(c.ConfigDir, "vfs.yml"), c.HomeDir)
	if err != nil {
		return rpcs3Profile{}, err
	}
	userID, err := readActiveUser(filepath.Join(c.ConfigDir, "GuiConfigs", "persistent_settings.dat"))
	if err != nil {
		return rpcs3Profile{}, err
	}
	userRoot := filepath.Join(devHDD0, "home", userID)
	if err := requireRealDirectory(userRoot); err != nil {
		return rpcs3Profile{}, fmt.Errorf("RPCS3 active user directory is unavailable: %w", err)
	}
	exdata := filepath.Join(userRoot, "exdata")
	if err := requireOptionalRealDirectory(exdata); err != nil {
		return rpcs3Profile{}, fmt.Errorf("RPCS3 active user exdata directory is invalid: %w", err)
	}
	return rpcs3Profile{DevHDD0: devHDD0, UserID: userID}, nil
}

func requireOptionalRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", filepath.Base(path))
	}
	return nil
}

func readDevHDD0(path, home string) (string, error) {
	data, err := readBoundedConfig(path)
	if err != nil {
		return "", fmt.Errorf("%w: read vfs.yml: %v", ErrInvalidRPCS3VFS, err)
	}
	var value string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, raw, ok := strings.Cut(line, ":")
		if !ok || parseYAMLScalar(key) != "/dev_hdd0/" {
			continue
		}
		if value != "" {
			return "", fmt.Errorf("%w: duplicate dev_hdd0 VFS mapping", ErrInvalidRPCS3VFS)
		}
		value, err = parseYAMLScalarValue(raw)
		if err != nil {
			return "", fmt.Errorf("%w: malformed dev_hdd0 VFS mapping", ErrInvalidRPCS3VFS)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("%w: read vfs.yml: %v", ErrInvalidRPCS3VFS, err)
	}
	if value == "" {
		return "", fmt.Errorf("%w: no dev_hdd0 mapping", ErrInvalidRPCS3VFS)
	}
	if value == "~" {
		value = home
	} else if strings.HasPrefix(value, "~/") {
		value = filepath.Join(home, filepath.FromSlash(value[2:]))
	}
	if strings.ContainsAny(value, "$\x00\r\n\t") || !filepath.IsAbs(value) {
		return "", fmt.Errorf("%w: mapping must be an absolute local path", ErrInvalidRPCS3VFS)
	}
	value = filepath.Clean(value)
	if err := requireRealDirectory(value); err != nil {
		return "", fmt.Errorf("%w: mapped directory is unavailable: %v", ErrInvalidRPCS3VFS, err)
	}
	return value, nil
}

// The Deck's vfs.yml is a small top-level scalar map. This parser intentionally
// accepts only ordinary, quoted, or single-quoted scalar values for the one
// mapping we use; aliases, block values and substitutions fail closed.
func parseYAMLScalar(raw string) string {
	value, err := parseYAMLScalarValue(raw)
	if err != nil {
		return ""
	}
	return value
}

func parseYAMLScalarValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	value = stripYAMLComment(value)
	if value == "" || strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") ||
		strings.HasPrefix(value, "&") || strings.HasPrefix(value, "*") || strings.HasPrefix(value, "[") ||
		strings.HasPrefix(value, "{") {
		return "", errors.New("unsupported YAML scalar")
	}
	if value[0] == '"' {
		decoded, err := strconv.Unquote(value)
		if err != nil {
			return "", err
		}
		return decoded, nil
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", errors.New("unterminated YAML string")
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	if strings.ContainsAny(value, "[]{}&*|>\"'") {
		return "", errors.New("unsupported YAML scalar")
	}
	return strings.TrimSpace(value), nil
}

func stripYAMLComment(value string) string {
	var quote rune
	for index, char := range value {
		if quote == '\'' {
			if char == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if char == '"' && (index == 0 || value[index-1] != '\\') {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == '#' && index > 0 && (value[index-1] == ' ' || value[index-1] == '\t') {
			return strings.TrimSpace(value[:index])
		}
	}
	return value
}

func readActiveUser(path string) (string, error) {
	data, err := readBoundedConfig(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "00000001", nil
	}
	if err != nil {
		return "", fmt.Errorf("read RPCS3 persistent user settings: %w", err)
	}
	section := ""
	user := ""
	seen := false
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 1024), 64<<10)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		if section != "Users" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "active_user" {
			continue
		}
		if seen {
			return "", fmt.Errorf("RPCS3 persistent settings contain duplicate active_user entries")
		}
		seen = true
		user, err = parseINIValue(value)
		if err != nil {
			return "", fmt.Errorf("RPCS3 active_user is malformed")
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if !seen || user == "" {
		user = "00000001"
	}
	if len(user) != 8 || !asciiNumeric(user) {
		return "", fmt.Errorf("RPCS3 active user ID is invalid")
	}
	return user, nil
}

func asciiNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func parseINIValue(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if value[0] == '"' {
		return strconv.Unquote(value)
	}
	if value[0] == '\'' {
		if len(value) < 2 || value[len(value)-1] != '\'' {
			return "", errors.New("unterminated value")
		}
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'"), nil
	}
	return value, nil
}

func readBoundedConfig(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 || info.Size() > maxConfigFile {
		return nil, fmt.Errorf("configuration file is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxConfigFile+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxConfigFile {
		return nil, fmt.Errorf("configuration file exceeds the size limit")
	}
	return data, nil
}

func requireRealDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a real directory", filepath.Base(path))
	}
	return nil
}

func configXDGHome(configDir string) string {
	return filepath.Dir(configDir)
}

func processIsRPCS3(process domain.Process) bool {
	if len(process.Args) == 0 {
		return false
	}
	base := strings.ToLower(filepath.Base(process.Args[0]))
	return base == "rpcs3" || base == "rpcs3.appimage"
}
