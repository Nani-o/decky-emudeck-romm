package app

import (
	"archive/zip"
	"crypto/sha1" //nolint:gosec // records what was placed, not for security
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/bodgit/sevenzip"

	"github.com/chenasraf/decky-emudeck-romm/backend/internal/domain"
)

// archiveMember is a file in an archive.
type archiveMember struct {
	// Name is its path inside the archive, with / separators.
	Name string
	Size int64
	open func() (io.ReadCloser, error)
}

// unpackedFile is an archive member written to disk.
type unpackedFile struct {
	// Rel is its path under the folder it was extracted to.
	Rel  string
	SHA1 string
}

// openArchive lists the files in the .zip or .7z at path, folders aside. close releases the
// archive once its members are read.
func openArchive(path string, rarVolumes ...string) (members []archiveMember, close func() error, err error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".rar":
		return openRAR(path, rarVolumes)
	case ".zip":
		r, err := zip.OpenReader(path)
		if err != nil {
			return nil, nil, badArchive(err)
		}
		for _, f := range r.File {
			if !f.FileInfo().IsDir() {
				members = append(members, archiveMember{Name: f.Name, Size: int64(f.UncompressedSize64), open: f.Open}) //nolint:gosec // sizes fit int64
			}
		}
		return members, r.Close, nil
	case ".7z":
		r, err := sevenzip.OpenReader(path)
		if err != nil {
			return nil, nil, badArchive(err)
		}
		for _, f := range r.File {
			if !f.FileInfo().IsDir() {
				members = append(members, archiveMember{Name: f.Name, Size: int64(f.UncompressedSize), open: f.Open}) //nolint:gosec // sizes fit int64
			}
		}
		return members, r.Close, nil
	}
	return nil, nil, fmt.Errorf("%s isn't an archive", filepath.Base(path))
}

func badArchive(err error) error {
	return &domain.Error{Kind: domain.KindCantDownload, Message: "The archive from RomM couldn't be opened.", Err: err}
}

// memberPath is where a member goes under the folder it is extracted to. It refuses a name that
// would land outside that folder.
func memberPath(name string) (string, error) {
	rel := filepath.FromSlash(strings.ReplaceAll(name, `\`, "/"))
	if !filepath.IsLocal(rel) {
		return "", &domain.Error{Kind: domain.KindCantDownload, Message: "The archive from RomM has a file that would land outside the ROM folder: " + name}
	}
	return filepath.Clean(rel), nil
}

// extract writes members under dest, hashing each as it goes.
func extract(members []archiveMember, dest string) ([]unpackedFile, error) {
	rels := make([]string, len(members))
	for i, m := range members {
		rel, err := memberPath(m.Name)
		if err != nil {
			return nil, err
		}
		rels[i] = rel
	}
	if err := validateRelativeFileTargets(rels); err != nil {
		return nil, err
	}
	out := make([]unpackedFile, 0, len(members))
	for i, m := range members {
		rel := rels[i]
		sum, err := extractMember(m, filepath.Join(dest, rel))
		if err != nil {
			return nil, err
		}
		out = append(out, unpackedFile{Rel: rel, SHA1: sum})
	}
	return out, nil
}

func validateRelativeFileTargets(paths []string) error {
	type node struct {
		file     bool
		children map[string]*node
	}
	root := &node{children: map[string]*node{}}
	for _, path := range paths {
		rel := filepath.Clean(path)
		if !filepath.IsLocal(rel) || rel == "." {
			return &domain.Error{Kind: domain.KindCantDownload, Message: "RomM lists an invalid game file path."}
		}
		current := root
		parts := strings.Split(filepath.ToSlash(rel), "/")
		for i, part := range parts {
			if current.file {
				return overlappingFileTargets()
			}
			child := current.children[part]
			if child == nil {
				child = &node{children: map[string]*node{}}
				current.children[part] = child
			}
			last := i == len(parts)-1
			if last {
				if child.file || len(child.children) > 0 {
					return overlappingFileTargets()
				}
				child.file = true
			}
			current = child
		}
	}
	return nil
}

func overlappingFileTargets() error {
	return &domain.Error{Kind: domain.KindCantDownload, Message: "RomM lists game files that overlap at the same path."}
}

func extractMember(m archiveMember, path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	in, err := m.open()
	if err != nil {
		return "", badArchive(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(path)
	if err != nil {
		return "", err
	}
	h := sha1.New() //nolint:gosec // see import
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("extract %s: %w", m.Name, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
