package rpcs3

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

const testContentID = "EP2007-NPEB00059_00-BZBDSIX000000001"
const testTitleID = "NPEB00059"

func resolvedTestTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func makeTestBundle(t *testing.T) (string, []byte) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	pkg := makeQualifiedBurnZombiePKG()
	pkgPath := filepath.Join(directory, "opaque-download.pkg")
	if err := os.WriteFile(pkgPath, pkg, 0o600); err != nil {
		t.Fatal(err)
	}
	rap := []byte("0123456789abcdef")
	if err := os.WriteFile(filepath.Join(directory, testContentID+".rap"), rap, 0o600); err != nil {
		t.Fatal(err)
	}
	return directory, rap
}

func makeTestPKG(contentID, titleID string, contentType, packageType uint32) []byte {
	return makePKGWithFields(contentID, titleID, []testPKGMetadata{
		{id: 2, value: be32(contentType)},
		{id: 3, value: be32(packageType)},
		{id: 6, value: paddedTitleID(titleID)},
	}, 0)
}

func makeQualifiedBurnZombiePKG() []byte {
	return makePKGWithFields(testContentID, testTitleID, []testPKGMetadata{
		{id: 1, value: be32(1)},
		{id: 2, value: be32(pkgContentGameExec)},
		{id: 3, value: be32(0x4e)},
		{id: 4, value: make([]byte, 8)},
		{id: 5, value: make([]byte, 4)},
		{id: 7, value: make([]byte, 24)},
		{id: 8, value: make([]byte, 4)},
	}, 160)
}

type testPKGMetadata struct {
	id    uint32
	value []byte
}

func makePKGWithFields(contentID, titleID string, fields []testPKGMetadata, metadataSize int) []byte {
	metadata := make([]byte, 0, 160)
	for _, field := range fields {
		var entry [8]byte
		binary.BigEndian.PutUint32(entry[:4], field.id)
		binary.BigEndian.PutUint32(entry[4:], uint32(len(field.value)))
		metadata = append(metadata, entry[:]...)
		metadata = append(metadata, field.value...)
	}
	if metadataSize > len(metadata) {
		metadata = append(metadata, make([]byte, metadataSize-len(metadata))...)
	}
	dataOffset := pkgHeaderSize + len(metadata)
	const dataSize = 96
	pkg := make([]byte, dataOffset+dataSize)
	copy(pkg[:4], []byte{0x7f, 'P', 'K', 'G'})
	binary.BigEndian.PutUint16(pkg[4:6], pkgReleaseRetail)
	binary.BigEndian.PutUint16(pkg[6:8], ps3PlatformType)
	binary.BigEndian.PutUint32(pkg[8:12], pkgHeaderSize)
	binary.BigEndian.PutUint32(pkg[12:16], uint32(len(fields)))
	binary.BigEndian.PutUint32(pkg[16:20], uint32(len(metadata)))
	binary.BigEndian.PutUint32(pkg[20:24], 3)
	binary.BigEndian.PutUint64(pkg[24:32], uint64(len(pkg)))
	binary.BigEndian.PutUint64(pkg[32:40], uint64(dataOffset))
	binary.BigEndian.PutUint64(pkg[40:48], dataSize)
	copy(pkg[0x30:0x60], contentID)
	copy(pkg[pkgHeaderSize:dataOffset], metadata)
	return pkg
}

func be32(value uint32) []byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], value)
	return out[:]
}

func paddedTitleID(titleID string) []byte {
	out := make([]byte, 12)
	copy(out, titleID)
	return out
}

func makeTestSFO(titleID string) []byte {
	const keyOffset = paramSFOHeaderSize + paramSFOEntrySize
	key := append([]byte("TITLE_ID"), 0)
	value := append([]byte(titleID), 0)
	valueOffset := keyOffset + len(key)
	data := make([]byte, valueOffset+len(value))
	copy(data[:4], []byte{0, 'P', 'S', 'F'})
	binary.LittleEndian.PutUint32(data[4:8], 0x00000101)
	binary.LittleEndian.PutUint32(data[8:12], keyOffset)
	binary.LittleEndian.PutUint32(data[12:16], uint32(valueOffset))
	binary.LittleEndian.PutUint32(data[16:20], 1)
	binary.LittleEndian.PutUint16(data[20:22], 0)
	binary.LittleEndian.PutUint16(data[22:24], paramSFOString)
	binary.LittleEndian.PutUint32(data[24:28], uint32(len(value)))
	binary.LittleEndian.PutUint32(data[28:32], uint32(len(value)))
	binary.LittleEndian.PutUint32(data[32:36], 0)
	copy(data[keyOffset:valueOffset], key)
	copy(data[valueOffset:], value)
	return data
}
