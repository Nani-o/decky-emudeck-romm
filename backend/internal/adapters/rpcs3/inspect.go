package rpcs3

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

const (
	pkgHeaderSize         = 0xC0
	maxMetadataSize       = 1 << 20
	maxBundleEntries      = 32
	maxPackageSize        = int64(64 << 30)
	maxBundleSize         = int64(64 << 30)
	maxSFOSize            = 256 << 10
	ps3PlatformType       = 0x0001
	pkgReleaseRetail      = 0x8000
	pkgContentGameExec    = 0x05
	pkgFlagEBOOT          = 0x02
	pkgFlagRequireLicense = 0x04
	pkgFlagPatch          = 0x10
	paramSFOString        = 0x0204
	paramSFOHeaderSize    = 20
	paramSFOEntrySize     = 16
)

var (
	ErrInvalidBundle   = errors.New("invalid PS3 PKG/RAP bundle")
	ErrUnsupportedPKG  = errors.New("unsupported PS3 package type")
	ErrInvalidRPCS3VFS = errors.New("RPCS3 dev_hdd0 mapping is unavailable or invalid")
)

// Bundle is a validated, one-title PSN base-game bundle in a private staging
// directory. Paths are returned only to the installer and are never exposed to
// the UI or written into plugin-owned ROM paths.
type Bundle struct {
	Directory     string
	PackagePath   string
	RAPPath       string
	Identity      domain.PS3Identity
	PackageSHA256 string
	RAPSHA256     string
	PackageSize   int64
	EntryCount    uint32
}

// InspectBundle accepts exactly one retail PS3 base-game PKG and the matching
// 16-byte RAP. It rejects arbitrary extras, split PKGs, links and oversized
// trees before RPCS3 is invoked.
func InspectBundle(directory string) (Bundle, error) {
	root, err := filepath.Abs(directory)
	if err != nil {
		return Bundle{}, fmt.Errorf("resolve PS3 staging directory: %w", err)
	}
	if strings.ContainsAny(root, "\x00\r\n") {
		return Bundle{}, fmt.Errorf("%w: control character in staging path", ErrInvalidBundle)
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return Bundle{}, fmt.Errorf("inspect PS3 staging directory: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&fs.ModeSymlink != 0 || rootInfo.Mode().Perm()&0o077 != 0 {
		return Bundle{}, fmt.Errorf("%w: staging root must be a private real directory", ErrInvalidBundle)
	}

	var pkgPath, rapPath string
	var entries int
	var total int64
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		entries++
		if entries > maxBundleEntries {
			return fmt.Errorf("%w: staging tree has too many entries", ErrInvalidBundle)
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: staging tree contains a symbolic link", ErrInvalidBundle)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() < 0 {
			return fmt.Errorf("%w: staging tree contains a non-regular file", ErrInvalidBundle)
		}
		if info.Size() > maxBundleSize-total {
			return fmt.Errorf("%w: staging tree exceeds the size limit", ErrInvalidBundle)
		}
		total += info.Size()
		switch strings.ToLower(filepath.Ext(path)) {
		case ".pkg":
			if pkgPath != "" {
				return fmt.Errorf("%w: expected one PKG", ErrInvalidBundle)
			}
			pkgPath = path
		case ".rap":
			if rapPath != "" {
				return fmt.Errorf("%w: expected one RAP", ErrInvalidBundle)
			}
			rapPath = path
		default:
			return fmt.Errorf("%w: unexpected file in staging tree", ErrInvalidBundle)
		}
		return nil
	})
	if err != nil {
		return Bundle{}, err
	}
	if pkgPath == "" || rapPath == "" || total < 16 {
		return Bundle{}, fmt.Errorf("%w: expected one PKG and one RAP", ErrInvalidBundle)
	}

	if strings.ContainsAny(pkgPath+rapPath, "\x00\r\n") {
		return Bundle{}, fmt.Errorf("%w: control character in package path", ErrInvalidBundle)
	}
	identity, pkgSize, err := inspectPKG(pkgPath)
	if err != nil {
		return Bundle{}, err
	}
	header, err := os.Open(pkgPath)
	if err != nil {
		return Bundle{}, err
	}
	var countBytes [4]byte
	_, countErr := header.ReadAt(countBytes[:], 20)
	_ = header.Close()
	if countErr != nil {
		return Bundle{}, countErr
	}
	entryCount := binary.BigEndian.Uint32(countBytes[:])
	if filepath.Base(rapPath) != identity.ContentID+filepath.Ext(rapPath) || !strings.EqualFold(filepath.Ext(rapPath), ".rap") {
		return Bundle{}, fmt.Errorf("%w: RAP filename does not match the PKG ContentID", ErrInvalidBundle)
	}
	rapInfo, err := os.Lstat(rapPath)
	if err != nil {
		return Bundle{}, fmt.Errorf("inspect RAP: %w", err)
	}
	if !rapInfo.Mode().IsRegular() || rapInfo.Size() != 16 {
		return Bundle{}, fmt.Errorf("%w: RAP must be a regular 16-byte file", ErrInvalidBundle)
	}
	pkgHash, err := sha256File(pkgPath, pkgSize)
	if err != nil {
		return Bundle{}, fmt.Errorf("hash PKG: %w", err)
	}
	rapHash, err := sha256File(rapPath, 16)
	if err != nil {
		return Bundle{}, fmt.Errorf("hash RAP: %w", err)
	}
	return Bundle{
		Directory:     root,
		PackagePath:   pkgPath,
		RAPPath:       rapPath,
		Identity:      identity,
		PackageSHA256: pkgHash,
		RAPSHA256:     rapHash,
		PackageSize:   pkgSize,
		EntryCount:    entryCount,
	}, nil
}

func inspectPKG(path string) (domain.PS3Identity, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return domain.PS3Identity{}, 0, fmt.Errorf("open PS3 PKG: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return domain.PS3Identity{}, 0, err
	}
	if !info.Mode().IsRegular() || info.Size() < pkgHeaderSize || info.Size() > maxPackageSize {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: PKG size is outside the supported range", ErrInvalidBundle)
	}
	header := make([]byte, pkgHeaderSize)
	if _, err := io.ReadFull(io.NewSectionReader(file, 0, pkgHeaderSize), header); err != nil {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: truncated PKG header", ErrInvalidBundle)
	}
	if !bytes.Equal(header[:4], []byte{0x7f, 'P', 'K', 'G'}) {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: PKG magic is invalid", ErrInvalidBundle)
	}
	if binary.BigEndian.Uint16(header[4:6]) != pkgReleaseRetail || binary.BigEndian.Uint16(header[6:8]) != ps3PlatformType {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: only retail PS3 packages are accepted", ErrUnsupportedPKG)
	}
	metadataOffset := uint64(binary.BigEndian.Uint32(header[8:12]))
	metadataCount := uint64(binary.BigEndian.Uint32(header[12:16]))
	metadataSize := uint64(binary.BigEndian.Uint32(header[16:20]))
	fileCount := uint64(binary.BigEndian.Uint32(header[20:24]))
	pkgSize := binary.BigEndian.Uint64(header[24:32])
	dataOffset := binary.BigEndian.Uint64(header[32:40])
	dataSize := binary.BigEndian.Uint64(header[40:48])
	if pkgSize != uint64(info.Size()) || metadataOffset < pkgHeaderSize || metadataCount == 0 || metadataCount > 1024 ||
		metadataSize < metadataCount*8 || metadataSize > maxMetadataSize ||
		metadataOffset > pkgSize || metadataSize > pkgSize-metadataOffset ||
		dataOffset < metadataOffset+metadataSize || dataOffset > pkgSize || dataSize > pkgSize-dataOffset ||
		fileCount == 0 || fileCount > dataSize/32 {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: inconsistent PKG size or metadata bounds", ErrInvalidBundle)
	}

	contentBytes := header[0x30:0x60]
	contentEnd := bytes.IndexByte(contentBytes, 0)
	if contentEnd < 0 {
		contentEnd = len(contentBytes)
	} else if !allZero(contentBytes[contentEnd:]) {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: malformed PKG ContentID padding", ErrInvalidBundle)
	}
	contentID := string(contentBytes[:contentEnd])
	headerTitleID := strings.TrimRight(string(header[0x37:0x40]), "\x00 ")

	metadata := make([]byte, int(metadataSize))
	if _, err := file.ReadAt(metadata, int64(metadataOffset)); err != nil {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: truncated PKG metadata", ErrInvalidBundle)
	}
	var contentType, packageType uint32
	var metadataTitleID string
	seen := map[uint32]bool{}
	cursor := uint64(0)
	for range metadataCount {
		if cursor > metadataSize || metadataSize-cursor < 8 {
			return domain.PS3Identity{}, 0, fmt.Errorf("%w: truncated PKG metadata entry", ErrInvalidBundle)
		}
		id := binary.BigEndian.Uint32(metadata[cursor : cursor+4])
		length := uint64(binary.BigEndian.Uint32(metadata[cursor+4 : cursor+8]))
		cursor += 8
		if length > metadataSize-cursor {
			return domain.PS3Identity{}, 0, fmt.Errorf("%w: PKG metadata entry exceeds its bounds", ErrInvalidBundle)
		}
		value := metadata[cursor : cursor+length]
		if id == 2 || id == 3 || id == 6 {
			if seen[id] {
				return domain.PS3Identity{}, 0, fmt.Errorf("%w: duplicate identity metadata", ErrInvalidBundle)
			}
			seen[id] = true
			switch id {
			case 2:
				if len(value) != 4 {
					return domain.PS3Identity{}, 0, fmt.Errorf("%w: invalid package content type", ErrInvalidBundle)
				}
				contentType = binary.BigEndian.Uint32(value)
			case 3:
				if len(value) != 4 {
					return domain.PS3Identity{}, 0, fmt.Errorf("%w: invalid package flags", ErrInvalidBundle)
				}
				packageType = binary.BigEndian.Uint32(value)
			case 6:
				if len(value) != 12 {
					return domain.PS3Identity{}, 0, fmt.Errorf("%w: invalid package title ID", ErrInvalidBundle)
				}
				metadataTitleID = strings.TrimRight(string(value), "\x00 ")
			}
		}
		cursor += length
	}
	if !seen[2] || !seen[3] {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: PKG identity metadata is incomplete", ErrInvalidBundle)
	}
	if contentType != pkgContentGameExec || packageType&pkgFlagEBOOT == 0 || packageType&pkgFlagRequireLicense == 0 || packageType&pkgFlagPatch != 0 {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: PKG is not a licensed PSN game executable", ErrUnsupportedPKG)
	}
	identity, err := domain.ParsePS3ContentID(contentID)
	if err != nil {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: %v", ErrInvalidBundle, err)
	}
	if headerTitleID != identity.TitleID {
		return domain.PS3Identity{}, 0, fmt.Errorf("%w: PKG header title ID does not match its ContentID", ErrInvalidBundle)
	}
	if seen[6] {
		metadataIdentity, err := domain.ParsePS3Identity(contentID, metadataTitleID)
		if err != nil || metadataIdentity != identity {
			return domain.PS3Identity{}, 0, fmt.Errorf("%w: optional PKG metadata title ID does not match its ContentID", ErrInvalidBundle)
		}
	}
	return identity, info.Size(), nil
}

func allZero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func sha256File(path string, expectedSize int64) (string, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || before.Size() != expectedSize {
		return "", fmt.Errorf("file changed while inspecting bundle")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(file, expectedSize+1))
	if err != nil {
		return "", err
	}
	after, err := file.Stat()
	if err != nil {
		return "", err
	}
	if read != expectedSize || !os.SameFile(before, after) || after.Size() != expectedSize || !after.ModTime().Equal(before.ModTime()) {
		return "", fmt.Errorf("file changed while inspecting bundle")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validRegular(path string, maxSize int64) (fs.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxSize {
		return nil, fmt.Errorf("%w: expected a bounded regular file", ErrInvalidBundle)
	}
	return info, nil
}

func inspectParamsfoTitleID(path string) (string, error) {
	info, err := validRegular(path, maxSFOSize)
	if err != nil {
		return "", fmt.Errorf("inspect installed PARAM.SFO: %w", err)
	}
	if info.Size() < paramSFOHeaderSize {
		return "", fmt.Errorf("installed PARAM.SFO is truncated")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(data[:4], []byte{0, 'P', 'S', 'F'}) {
		return "", fmt.Errorf("installed PARAM.SFO has invalid magic")
	}
	keyTable := uint64(binary.LittleEndian.Uint32(data[8:12]))
	valueTable := uint64(binary.LittleEndian.Uint32(data[12:16]))
	count := uint64(binary.LittleEndian.Uint32(data[16:20]))
	entryBytes := count * paramSFOEntrySize
	if count == 0 || count > 4096 || paramSFOHeaderSize+entryBytes > uint64(len(data)) ||
		keyTable < paramSFOHeaderSize+entryBytes || keyTable > uint64(len(data)) ||
		valueTable < keyTable || valueTable > uint64(len(data)) {
		return "", fmt.Errorf("installed PARAM.SFO has invalid table bounds")
	}
	titleID := ""
	for index := uint64(0); index < count; index++ {
		entry := uint64(paramSFOHeaderSize) + index*paramSFOEntrySize
		keyOffset := uint64(binary.LittleEndian.Uint16(data[entry : entry+2]))
		format := binary.LittleEndian.Uint16(data[entry+2 : entry+4])
		dataLength := uint64(binary.LittleEndian.Uint32(data[entry+4 : entry+8]))
		dataMax := uint64(binary.LittleEndian.Uint32(data[entry+8 : entry+12]))
		dataOffset := uint64(binary.LittleEndian.Uint32(data[entry+12 : entry+16]))
		keyStart := keyTable + keyOffset
		if keyStart >= valueTable {
			return "", fmt.Errorf("installed PARAM.SFO has invalid key offset")
		}
		keyEnd := bytes.IndexByte(data[keyStart:valueTable], 0)
		if keyEnd < 0 {
			return "", fmt.Errorf("installed PARAM.SFO has unterminated key")
		}
		key := string(data[keyStart : keyStart+uint64(keyEnd)])
		if key != "TITLE_ID" {
			continue
		}
		if titleID != "" || format != paramSFOString || dataLength == 0 || dataLength > dataMax ||
			valueTable+dataOffset > uint64(len(data)) || dataLength > uint64(len(data))-(valueTable+dataOffset) {
			return "", fmt.Errorf("installed PARAM.SFO TITLE_ID entry is invalid")
		}
		titleID = strings.TrimRight(string(data[valueTable+dataOffset:valueTable+dataOffset+dataLength]), "\x00 ")
	}
	if titleID == "" {
		return "", fmt.Errorf("installed PARAM.SFO has no TITLE_ID")
	}
	return titleID, nil
}
