package domain

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

var switchTitleTag = regexp.MustCompile(`(?i)\[([0-9a-f]{16})\]`)
var switchVersionTag = regexp.MustCompile(`(?i)\[v([0-9]+)\]`)
var switchRARPart = regexp.MustCompile(`(?i)^(.*)\.part([0-9]+)\.rar$`)

// SelectContentFiles chooses the complete base game, every DLC and the newest
// compatible Switch patch. Raw package versions order patches; readable version
// strings are never treated as arithmetic encodings of those integers.
// Ambiguous metadata is an error before a download modifies any installed file.
func SelectContentFiles(files []ROMFile) ([]ROMFile, error) {
	all := allContentFiles(files)
	var updates []ROMFile
	baseIDs := map[uint64]bool{}
	for _, f := range all {
		if f.IsGame() {
			id, _, err := switchPackageIdentity(f)
			if err != nil {
				return nil, err
			}
			if id != 0 && id&0xfff == 0 {
				baseIDs[id] = true
			}
		} else if f.Category == "update" {
			updates = append(updates, f)
		}
	}
	if len(updates) == 0 {
		return all, nil
	}
	type patch struct {
		file    ROMFile
		id      uint64
		version uint64
		known   bool
	}
	patches := make([]patch, 0, len(updates))
	var expected, newest uint64
	for _, f := range updates {
		id, version, err := switchPackageIdentity(f)
		if err != nil {
			return nil, err
		}
		if id != 0 {
			if id&0xfff != 0x800 || len(baseIDs) > 0 && !baseIDs[id-0x800] {
				return nil, badSwitchUpdate("An update's title ID does not match this Switch base game.")
			}
			if expected != 0 && expected != id {
				return nil, badSwitchUpdate("The Switch updates refer to different title IDs.")
			}
			expected = id
		}
		// A single legacy update needs no ordering. Multiple candidates must have
		// trustworthy identities, rather than silently fetching all historical ones.
		known := id != 0 && version != nil
		patches = append(patches, patch{file: f, id: id, known: known})
		if version != nil {
			patches[len(patches)-1].version = *version
			if *version > newest {
				newest = *version
			}
		}
	}
	if len(patches) == 1 {
		return all, nil
	}
	for _, p := range patches {
		if !p.known {
			return nil, badSwitchUpdate("The latest Switch update cannot be determined. Add its title ID and raw package version in RomM or [TITLEID][vNUMBER] filename tags.")
		}
	}
	// Identical versions may be packaged both as an NSP and an archive. Prefer
	// the ready-to-use NSP/XCI, then a single archive, then a complete RAR set.
	// Two equally preferred packages are ambiguous and are never guessed from size.
	type cohort struct {
		files []ROMFile
		rank  int
		parts map[int]bool
	}
	cohorts := map[string]*cohort{}
	for _, p := range patches {
		if p.version != newest {
			continue
		}
		f := p.file
		key, rank, part := switchUpdateCohort(f)
		c := cohorts[key]
		if c == nil {
			c = &cohort{rank: rank, parts: map[int]bool{}}
			cohorts[key] = c
		}
		if part > 0 && c.parts[part] {
			return nil, badSwitchUpdate("A Switch update archive has duplicate RAR volumes.")
		}
		if part > 0 {
			c.parts[part] = true
		}
		c.files = append(c.files, f)
	}
	bestRank := 100
	var chosen *cohort
	ambiguous := false
	for _, c := range cohorts {
		if c.rank < bestRank {
			chosen, bestRank, ambiguous = c, c.rank, false
		} else if c.rank == bestRank {
			ambiguous = true
		}
	}
	if chosen == nil || ambiguous {
		return nil, badSwitchUpdate("The newest Switch update has several equivalent packages. Keep one package in RomM before downloading.")
	}
	if len(chosen.parts) > 0 {
		for part := 1; part <= len(chosen.files); part++ {
			if !chosen.parts[part] {
				return nil, badSwitchUpdate("The newest Switch update is missing a RAR volume.")
			}
		}
	}
	selected := map[int]bool{}
	for _, f := range chosen.files {
		selected[f.ID] = true
	}
	out := make([]ROMFile, 0, len(all))
	for _, f := range all {
		if f.Category != "update" || selected[f.ID] {
			out = append(out, f)
		}
	}
	return out, nil
}

func badSwitchUpdate(message string) error { return &Error{Kind: KindCantDownload, Message: message} }

func switchPackageIdentity(f ROMFile) (uint64, *uint64, error) {
	tags := switchTitleTag.FindAllStringSubmatch(path.Base(f.Name), -1)
	versions := switchVersionTag.FindAllStringSubmatch(path.Base(f.Name), -1)
	if len(tags) > 1 || len(versions) > 1 {
		return 0, nil, badSwitchUpdate("A Switch filename has ambiguous package identity tags.")
	}
	// Complete canonical tags are authoritative. Some RomM scanners expose an
	// encrypted NCA's ID/version instead of the outer patch package's metadata.
	if len(tags) == 1 && len(versions) == 1 {
		id, idErr := strconv.ParseUint(tags[0][1], 16, 64)
		n, versionErr := strconv.ParseUint(versions[0][1], 10, 32)
		if idErr != nil || versionErr != nil {
			return 0, nil, badSwitchUpdate("A Switch filename has invalid package identity tags.")
		}
		return id, &n, nil
	}
	var id uint64
	if f.TitleID != "" {
		raw := strings.TrimSpace(f.TitleID)
		n, err := strconv.ParseUint(raw, 16, 64)
		if err != nil || len(raw) != 16 {
			return 0, nil, badSwitchUpdate("A Switch package has an invalid RomM title ID.")
		}
		id = n
	} else if len(tags) == 1 {
		id, _ = strconv.ParseUint(tags[0][1], 16, 64)
	}
	var version *uint64
	if f.TitleVersion != "" {
		raw := strings.TrimPrefix(strings.TrimSpace(f.TitleVersion), "v")
		n, err := strconv.ParseUint(raw, 10, 32)
		if err != nil {
			return 0, nil, badSwitchUpdate("A Switch package has an invalid raw RomM version.")
		}
		version = &n
	} else if len(versions) == 1 {
		n, err := strconv.ParseUint(versions[0][1], 10, 32)
		if err != nil {
			return 0, nil, badSwitchUpdate("A Switch filename has an invalid raw version tag.")
		}
		version = &n
	}
	return id, version, nil
}

func switchUpdateCohort(f ROMFile) (string, int, int) {
	name := path.Base(f.Name)
	parent := path.Dir(f.Path)
	if match := switchRARPart.FindStringSubmatch(name); match != nil {
		part, err := strconv.Atoi(match[2])
		if err != nil || part < 1 {
			return parent + "/" + name, 100, 0
		}
		return parent + "/" + match[1], 3, part
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".nsp", ".xci":
		return parent + "/" + name, 1, 0
	default:
		return parent + "/" + name, 2, 0
	}
}
