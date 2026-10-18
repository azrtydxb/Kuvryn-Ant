package rpmrepo

import (
	"bytes"
	"crypto/md5" // RPM's native signature-header digest; verified, never trusted alone.
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Bounds for hostile input. RPM headers are parsed only as data: nothing in a
// package is executed, extracted or followed.
const (
	maxPackage    = 2 << 30
	maxHeaderData = 64 << 20
	maxIndex      = 8192
	maxFiles      = 500000
	maxMetadata   = 48 << 20
	maxChangelog  = 64
	leadSize      = 96
)

var (
	errMalformed = errors.New("malformed RPM")
	errInvalid   = errors.New("invalid RPM request")

	rpmName    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+~^-]{0,127}$`)
	rpmVersion = regexp.MustCompile(`^[A-Za-z0-9._+~^]{1,128}$`)
	rpmArch    = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
)

// Header tag numbers (rpmtag.h) that this adapter reads.
const (
	tagName, tagVersion, tagRelease, tagEpoch  = 1000, 1001, 1002, 1003
	tagSummary, tagDescription, tagBuildTime   = 1004, 1005, 1006
	tagBuildHost, tagSize, tagVendor           = 1007, 1009, 1011
	tagLicense, tagPackager, tagGroup, tagURL  = 1014, 1015, 1016, 1020
	tagArch, tagSourceRPM, tagArchiveSize      = 1022, 1044, 1046
	tagOldFileNames, tagFileSizes, tagFileMode = 1027, 1028, 1030
	tagFileFlags, tagBaseNames, tagDirNames    = 1037, 1117, 1118
	tagDirIndexes                              = 1116
	tagProvideName, tagProvideFlags            = 1047, 1112
	tagProvideVersion                          = 1113
	tagRequireName, tagRequireFlags            = 1049, 1048
	tagRequireVersion                          = 1050
	tagConflictName, tagConflictFlags          = 1054, 1053
	tagConflictVersion                         = 1055
	tagObsoleteName, tagObsoleteFlags          = 1090, 1114
	tagObsoleteVersion                         = 1115
	tagChangeTime, tagChangeName, tagChangeTxt = 1080, 1081, 1082
	tagSourcePackage                           = 1106
	tagLongSize, tagLongArchiveSize            = 5009, 271

	sigSize, sigMD5, sigSHA256, sigLongSize   = 1000, 1004, 273, 270
	sigDSA, sigRSA, sigPGP, sigGPG, sigOpen   = 267, 268, 1002, 1005, 278
	senseLess, senseGreater, senseEqual       = 0x02, 0x04, 0x08
	sensePre                                  = 1<<6 | 1<<9 | 1<<10
	senseRPMLib                               = 1 << 24
	fileGhost                                 = 1 << 6
	modeDir, modeTypeMask                     = 0o040000, 0o170000
	typeInt8, typeInt16, typeInt32, typeInt64 = 2, 3, 4, 5
	typeString, typeBin, typeStringArray      = 6, 7, 8
	typeI18N, typeChar                        = 9, 1
)

type entry struct {
	typ, off, count uint32
}

type header struct {
	raw     []byte // intro + index + data, exactly as stored (digest input)
	entries map[uint32]entry
	data    []byte
}

func (h *header) end() int64 { return int64(len(h.raw)) }

func typeSize(t uint32) int {
	switch t {
	case typeChar, typeInt8:
		return 1
	case typeInt16:
		return 2
	case typeInt32:
		return 4
	case typeInt64:
		return 8
	}
	return 0
}

// readHeader parses one header structure at the reader's current position.
func readHeader(r io.Reader) (*header, error) {
	var intro [16]byte
	if _, err := io.ReadFull(r, intro[:]); err != nil {
		return nil, fmt.Errorf("%w: truncated header", errMalformed)
	}
	if intro[0] != 0x8e || intro[1] != 0xad || intro[2] != 0xe8 || intro[3] != 0x01 {
		return nil, fmt.Errorf("%w: bad header magic", errMalformed)
	}
	n := binary.BigEndian.Uint32(intro[8:12])
	size := binary.BigEndian.Uint32(intro[12:16])
	if n == 0 || n > maxIndex || size > maxHeaderData {
		return nil, fmt.Errorf("%w: header bounds", errMalformed)
	}
	body := make([]byte, int(n)*16+int(size))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("%w: truncated header body", errMalformed)
	}
	h := &header{raw: append(append(make([]byte, 0, 16+len(body)), intro[:]...), body...), entries: make(map[uint32]entry, n), data: body[int(n)*16:]}
	for i := 0; i < int(n); i++ {
		e := body[i*16 : i*16+16]
		tag := binary.BigEndian.Uint32(e[0:4])
		x := entry{typ: binary.BigEndian.Uint32(e[4:8]), off: binary.BigEndian.Uint32(e[8:12]), count: binary.BigEndian.Uint32(e[12:16])}
		if _, dup := h.entries[tag]; dup || x.typ < 1 || x.typ > 9 && x.typ != 0 || x.count == 0 || x.count > 1<<20 || uint64(x.off) > uint64(len(h.data)) {
			return nil, fmt.Errorf("%w: bad index entry", errMalformed)
		}
		if w := typeSize(x.typ); w > 0 && uint64(x.off)+uint64(w)*uint64(x.count) > uint64(len(h.data)) {
			return nil, fmt.Errorf("%w: entry out of range", errMalformed)
		}
		if x.typ == typeBin && uint64(x.off)+uint64(x.count) > uint64(len(h.data)) {
			return nil, fmt.Errorf("%w: entry out of range", errMalformed)
		}
		h.entries[tag] = x
	}
	return h, nil
}

func (h *header) has(tag uint32) bool { _, ok := h.entries[tag]; return ok }

// strs returns every string of a STRING, I18NSTRING or STRING_ARRAY entry.
func (h *header) strs(tag uint32) ([]string, error) {
	e, ok := h.entries[tag]
	if !ok {
		return nil, nil
	}
	if e.typ != typeString && e.typ != typeStringArray && e.typ != typeI18N || e.typ == typeString && e.count != 1 {
		return nil, fmt.Errorf("%w: tag %d type", errMalformed, tag)
	}
	out := make([]string, 0, min(e.count, 1024))
	rest := h.data[e.off:]
	for i := uint32(0); i < e.count; i++ {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			return nil, fmt.Errorf("%w: unterminated string", errMalformed)
		}
		out = append(out, string(rest[:end]))
		rest = rest[end+1:]
	}
	return out, nil
}
func (h *header) str(tag uint32) (string, error) {
	s, err := h.strs(tag)
	if err != nil || len(s) == 0 {
		return "", err
	}
	return s[0], nil
}
func (h *header) ints(tag uint32) ([]uint64, error) {
	e, ok := h.entries[tag]
	if !ok {
		return nil, nil
	}
	w := typeSize(e.typ)
	if w == 0 {
		return nil, fmt.Errorf("%w: tag %d type", errMalformed, tag)
	}
	out := make([]uint64, e.count)
	for i := range out {
		b := h.data[int(e.off)+i*w:]
		switch w {
		case 1:
			out[i] = uint64(b[0])
		case 2:
			out[i] = uint64(binary.BigEndian.Uint16(b))
		case 4:
			out[i] = uint64(binary.BigEndian.Uint32(b))
		case 8:
			out[i] = binary.BigEndian.Uint64(b)
		}
	}
	return out, nil
}
func (h *header) int1(tag uint32) (uint64, bool, error) {
	v, err := h.ints(tag)
	if err != nil || len(v) == 0 {
		return 0, false, err
	}
	return v[0], true, nil
}
func (h *header) bin(tag uint32) ([]byte, bool) {
	e, ok := h.entries[tag]
	if !ok || e.typ != typeBin {
		return nil, false
	}
	return h.data[e.off : e.off+e.count], true
}

type dependency struct {
	Name, Flags, Epoch, Version, Release string
	Pre                                  bool
}

type fileEntry struct {
	Path string
	Kind string // "", "dir" or "ghost"
}

type change struct {
	Author, Text string
	Time         uint64
}

// Package is the validated identity and index metadata of one RPM.
type Package struct {
	Name, Epoch, Version, Release, Arch string
	HasEpoch                            bool
	Summary, Description, Packager, URL string
	License, Vendor, Group, BuildHost   string
	SourceRPM                           string
	BuildTime, Installed, Archive       uint64
	HeaderStart, HeaderEnd              int64
	Signed                              bool
	Provides, Requires                  []dependency
	Conflicts, Obsoletes                []dependency
	Files                               []fileEntry
	Changes                             []change
}

// Filename is the conventional NEVRA file name (epoch is not part of it).
func (p *Package) Filename() string {
	return p.Name + "-" + p.Version + "-" + p.Release + "." + p.Arch + ".rpm"
}

// VersionKey is the immutable core version for one name; epoch is metadata.
func (p *Package) VersionKey() string { return p.Version + "-" + p.Release + "." + p.Arch }

// parseRPM validates lead, signature header and header, verifies every
// integrity digest the signature header carries, and extracts metadata. The
// reader must be positioned at offset 0 with exactly size bytes.
func parseRPM(r io.ReaderAt, size int64) (*Package, error) {
	if size < leadSize+32 || size > maxPackage {
		return nil, fmt.Errorf("%w: size", errMalformed)
	}
	var lead [leadSize]byte
	if _, err := r.ReadAt(lead[:], 0); err != nil || lead[0] != 0xed || lead[1] != 0xab || lead[2] != 0xee || lead[3] != 0xdb || lead[4] < 3 || lead[4] > 4 {
		return nil, fmt.Errorf("%w: bad lead", errMalformed)
	}
	sr := io.NewSectionReader(r, leadSize, size-leadSize)
	sig, err := readHeader(sr)
	if err != nil {
		return nil, err
	}
	// The signature header is padded to an 8-byte boundary.
	pos := int64(leadSize) + sig.end()
	if pad := (8 - pos%8) % 8; pad > 0 {
		pos += pad
	}
	if pos >= size {
		return nil, fmt.Errorf("%w: truncated", errMalformed)
	}
	main, err := readHeader(io.NewSectionReader(r, pos, size-pos))
	if err != nil {
		return nil, err
	}
	hdrStart, payload := pos, pos+main.end()
	if payload > size {
		return nil, fmt.Errorf("%w: truncated", errMalformed)
	}
	if err = verifyDigests(r, sig, main, hdrStart, size); err != nil {
		return nil, err
	}
	p := &Package{HeaderStart: hdrStart, HeaderEnd: payload}
	for _, t := range []uint32{sigDSA, sigRSA, sigPGP, sigGPG, sigOpen} {
		if sig.has(t) {
			p.Signed = true
		}
	}
	if err = extract(main, p); err != nil {
		return nil, err
	}
	return p, nil
}

func verifyDigests(r io.ReaderAt, sig, main *header, start, size int64) error {
	if s, err := sig.str(sigSHA256); err != nil {
		return err
	} else if s != "" {
		sum := sha256.Sum256(main.raw)
		if !strings.EqualFold(s, hex.EncodeToString(sum[:])) {
			return fmt.Errorf("%w: header SHA-256 mismatch", errMalformed)
		}
	}
	rest := size - start
	for _, t := range []uint32{sigSize, sigLongSize} {
		if v, ok, err := sig.int1(t); err != nil {
			return err
		} else if ok && int64(v) != rest {
			return fmt.Errorf("%w: size mismatch", errMalformed)
		}
	}
	if want, ok := sig.bin(sigMD5); ok {
		if len(want) != md5.Size {
			return fmt.Errorf("%w: MD5 length", errMalformed)
		}
		h := md5.New() //nolint:gosec // verifies the package's own legacy digest
		if _, err := io.CopyBuffer(h, io.NewSectionReader(r, start, rest), make([]byte, 64<<10)); err != nil {
			return err
		}
		if !bytes.Equal(h.Sum(nil), want) {
			return fmt.Errorf("%w: MD5 mismatch", errMalformed)
		}
	}
	return nil
}

func extract(h *header, p *Package) error {
	get := func(tag uint32) string {
		s, _ := h.str(tag)
		return s
	}
	for _, tag := range []uint32{tagName, tagVersion, tagRelease, tagArch, tagSummary, tagDescription, tagURL, tagLicense, tagVendor, tagGroup, tagPackager, tagBuildHost, tagSourceRPM} {
		if _, err := h.strs(tag); err != nil {
			return err
		}
	}
	p.Name, p.Version, p.Release, p.Arch = get(tagName), get(tagVersion), get(tagRelease), get(tagArch)
	if h.has(tagSourcePackage) || !h.has(tagSourceRPM) && p.Arch == "" {
		p.Arch = "src"
	}
	if !rpmName.MatchString(p.Name) || !rpmVersion.MatchString(p.Version) || !rpmVersion.MatchString(p.Release) || !rpmArch.MatchString(p.Arch) {
		return fmt.Errorf("%w: invalid name/version/release/arch", errMalformed)
	}
	if v, ok, err := h.int1(tagEpoch); err != nil {
		return err
	} else if ok {
		if v > 1<<31-1 {
			return fmt.Errorf("%w: epoch", errMalformed)
		}
		p.HasEpoch, p.Epoch = true, fmt.Sprint(v)
	} else {
		p.Epoch = "0"
	}
	p.Summary, p.Description, p.URL = get(tagSummary), get(tagDescription), get(tagURL)
	p.License, p.Vendor, p.Group, p.Packager = get(tagLicense), get(tagVendor), get(tagGroup), get(tagPackager)
	p.BuildHost, p.SourceRPM = get(tagBuildHost), get(tagSourceRPM)
	if v, ok, err := h.int1(tagBuildTime); err != nil {
		return err
	} else if ok {
		p.BuildTime = v
	}
	var err error
	if p.Installed, err = firstInt(h, tagLongSize, tagSize); err != nil {
		return err
	}
	if p.Archive, err = firstInt(h, tagLongArchiveSize, tagArchiveSize); err != nil {
		return err
	}
	for _, d := range []struct {
		dst                  *[]dependency
		name, flags, vers    uint32
		skipRPMLib, allowPre bool
	}{
		{&p.Provides, tagProvideName, tagProvideFlags, tagProvideVersion, false, false},
		{&p.Requires, tagRequireName, tagRequireFlags, tagRequireVersion, true, true},
		{&p.Conflicts, tagConflictName, tagConflictFlags, tagConflictVersion, false, false},
		{&p.Obsoletes, tagObsoleteName, tagObsoleteFlags, tagObsoleteVersion, false, false},
	} {
		if *d.dst, err = deps(h, d.name, d.flags, d.vers, d.skipRPMLib, d.allowPre); err != nil {
			return err
		}
	}
	if p.Files, err = files(h); err != nil {
		return err
	}
	p.Changes, err = changelog(h)
	return err
}

func firstInt(h *header, tags ...uint32) (uint64, error) {
	for _, t := range tags {
		if v, ok, err := h.int1(t); err != nil {
			return 0, err
		} else if ok {
			return v, nil
		}
	}
	return 0, nil
}

func deps(h *header, nameTag, flagTag, versTag uint32, skipLib, allowPre bool) ([]dependency, error) {
	names, err := h.strs(nameTag)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	flags, err := h.ints(flagTag)
	if err != nil {
		return nil, err
	}
	vers, err := h.strs(versTag)
	if err != nil {
		return nil, err
	}
	if len(flags) != len(names) || len(vers) != len(names) {
		return nil, fmt.Errorf("%w: dependency arrays disagree", errMalformed)
	}
	seen := map[dependency]bool{}
	var out []dependency
	for i, n := range names {
		if skipLib && flags[i]&senseRPMLib != 0 {
			continue
		}
		d := dependency{Name: n, Pre: allowPre && flags[i]&sensePre != 0}
		switch flags[i] & (senseLess | senseGreater | senseEqual) {
		case senseLess:
			d.Flags = "LT"
		case senseGreater:
			d.Flags = "GT"
		case senseEqual:
			d.Flags = "EQ"
		case senseLess | senseEqual:
			d.Flags = "LE"
		case senseGreater | senseEqual:
			d.Flags = "GE"
		}
		if v := vers[i]; v != "" {
			d.Epoch = "0"
			if e, rest, ok := strings.Cut(v, ":"); ok && e != "" && strings.Trim(e, "0123456789") == "" {
				d.Epoch, v = e, rest
			}
			if ver, rel, ok := strings.Cut(v, "-"); ok {
				d.Version, d.Release = ver, rel
			} else {
				d.Version = v
			}
		}
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	return out, nil
}

func files(h *header) ([]fileEntry, error) {
	var paths []string
	if old, err := h.strs(tagOldFileNames); err != nil {
		return nil, err
	} else if len(old) > 0 {
		paths = old
	} else {
		base, err := h.strs(tagBaseNames)
		if err != nil || len(base) == 0 {
			return nil, err
		}
		dirs, err := h.strs(tagDirNames)
		if err != nil {
			return nil, err
		}
		idx, err := h.ints(tagDirIndexes)
		if err != nil {
			return nil, err
		}
		if len(idx) != len(base) {
			return nil, fmt.Errorf("%w: file arrays disagree", errMalformed)
		}
		paths = make([]string, len(base))
		for i, b := range base {
			if idx[i] >= uint64(len(dirs)) {
				return nil, fmt.Errorf("%w: directory index", errMalformed)
			}
			paths[i] = dirs[idx[i]] + b
		}
	}
	if len(paths) > maxFiles {
		return nil, fmt.Errorf("%w: too many files", errMalformed)
	}
	modes, err := h.ints(tagFileMode)
	if err != nil {
		return nil, err
	}
	flags, err := h.ints(tagFileFlags)
	if err != nil {
		return nil, err
	}
	if len(modes) != 0 && len(modes) != len(paths) || len(flags) != 0 && len(flags) != len(paths) {
		return nil, fmt.Errorf("%w: file arrays disagree", errMalformed)
	}
	out := make([]fileEntry, len(paths))
	for i, path := range paths {
		out[i].Path = path
		switch {
		case len(flags) > 0 && flags[i]&fileGhost != 0:
			out[i].Kind = "ghost"
		case len(modes) > 0 && modes[i]&modeTypeMask == modeDir:
			out[i].Kind = "dir"
		}
	}
	return out, nil
}

func changelog(h *header) ([]change, error) {
	times, err := h.ints(tagChangeTime)
	if err != nil || len(times) == 0 {
		return nil, err
	}
	names, err := h.strs(tagChangeName)
	if err != nil {
		return nil, err
	}
	texts, err := h.strs(tagChangeTxt)
	if err != nil {
		return nil, err
	}
	if len(names) != len(times) || len(texts) != len(times) {
		return nil, fmt.Errorf("%w: changelog arrays disagree", errMalformed)
	}
	out := make([]change, len(times))
	for i := range times {
		out[i] = change{Author: names[i], Text: texts[i], Time: times[i]}
	}
	// Newest entries last, as createrepo_c does, bounded to the most recent.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time < out[j].Time })
	if len(out) > maxChangelog {
		out = out[len(out)-maxChangelog:]
	}
	return out, nil
}
