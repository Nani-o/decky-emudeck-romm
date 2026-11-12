package domain

import (
	"fmt"
	"strings"
	"time"
)

// PS3Identity identifies an installed PlayStation 3 title. ContentID is read
// from the PKG header, and TitleID is its embedded product serial. The PKG
// header serial and installed PARAM.SFO are checked independently against it.
type PS3Identity struct {
	ContentID string
	TitleID   string
}

// PS3InstallResult says which native RPCS3 title was installed and verified.
// DesktopEntry is a generated launcher, not a list of owned NAND files. Native
// removal uses a separate installation receipt and always preserves user saves.
type PS3InstallResult struct {
	ROMID        int
	Identity     PS3Identity
	RPCS3User    string
	VerifiedAt   time.Time
	DesktopEntry string
}

// ParsePS3Identity validates a ContentID and an optional metadata TitleID. It
// never derives either value from a filename.
func ParsePS3Identity(contentID, titleID string) (PS3Identity, error) {
	identity, err := ParsePS3ContentID(contentID)
	if err != nil {
		return PS3Identity{}, err
	}
	if !validPS3TitleID(titleID) || titleID != identity.TitleID {
		return PS3Identity{}, fmt.Errorf("PKG title ID does not match its content ID")
	}
	return identity, nil
}

// ParsePS3ContentID validates an RPCS3 ContentID and extracts its product
// TitleID. The identifier is read from the package header, never its filename.
func ParsePS3ContentID(contentID string) (PS3Identity, error) {
	if len(contentID) != 36 || strings.TrimSpace(contentID) != contentID {
		return PS3Identity{}, fmt.Errorf("invalid PS3 content ID")
	}
	publisher, remainder, ok := strings.Cut(contentID, "-")
	if !ok || len(publisher) != 6 || !asciiUpperAlphaNumeric(publisher) {
		return PS3Identity{}, fmt.Errorf("invalid PS3 content ID publisher")
	}
	product, description, ok := strings.Cut(remainder, "-")
	if !ok || len(description) != 16 || !asciiUpperAlphaNumeric(description) {
		return PS3Identity{}, fmt.Errorf("invalid PS3 content ID description")
	}
	serial, revision, ok := strings.Cut(product, "_")
	if !ok || len(revision) != 2 || !asciiDigits(revision) || !validPS3TitleID(serial) {
		return PS3Identity{}, fmt.Errorf("invalid PS3 content ID product")
	}
	return PS3Identity{ContentID: contentID, TitleID: serial}, nil
}

func validPS3TitleID(value string) bool {
	return len(value) == 9 &&
		asciiUpperAlpha(value[:4]) &&
		asciiDigits(value[4:])
}

func asciiUpperAlpha(value string) bool {
	for _, r := range value {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return value != ""
}

func asciiDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func asciiUpperAlphaNumeric(value string) bool {
	for _, r := range value {
		if (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return value != ""
}
