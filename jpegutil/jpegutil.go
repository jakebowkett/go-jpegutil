/*
Package jpegutil provides a simple way to handle some common
tasks with JPEGs such as replacing metadata and checking magic
bytes.
*/
package jpegutil

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Various JPEG header markers.
var (
	soi      = []byte{0xFF, 0xD8}
	app1     = []byte{0xFF, 0xE1}
	exif     = []byte{0x45, 0x78, 0x69, 0x66, 0x00, 0x00}
	tiff     = []byte{0x4D, 0x4D, 0x00, 0x2A, 0x00, 0x00, 0x00, 0x08} // Motorola big endian
	ifd0Next = []byte{0x00, 0x00, 0x00, 0x00}
	dqt      = []byte{0xFF, 0xDB}
	segPad   = []byte{0x00, 0x00}
	eoi      = []byte{0xFF, 0xD9}
)

/*
Assert returns an error if rs doesn't represent
a valid JPEG image. It checks for the SOI and
EOI byte markers without reading the entire file.

If the current offset of rs is important to the
caller it should be stored somewhere as Assert
does not attempt to restore the original offset.
*/
func Assert(rs io.ReadSeeker) (err error) {

	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return err
	}

	p := make([]byte, 2, 2)
	if _, err = rs.Read(p); err != nil {
		return err
	}

	if !bytes.Equal(p, soi) {
		return errors.New("jpegutil: missing SOI marker")
	}

	if _, err = rs.Seek(-2, io.SeekEnd); err != nil {
		return err
	}

	if _, err = rs.Read(p); err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	if !bytes.Equal(p, eoi) {
		return errors.New("jpegutil: missing EOI marker")
	}

	return nil
}

type Metadata map[tag]string

type tag int

const (
	MetaArtist tag = iota
	MetaTitle
	MetaCopyright
)

// First two bytes are tag, second two are type.
var tagMarker = map[tag][]byte{
	MetaArtist:    []byte{0x01, 0x3B, 0x00, 0x02},
	MetaTitle:     []byte{0x01, 0x0E, 0x00, 0x02},
	MetaCopyright: []byte{0x82, 0x98, 0x00, 0x02},
}

/*
ReplaceMeta takes a JPEG file represented by rs and returns
a reader r which is the same file with its Exif data
replaced with the supplied metadata tags and their values.
The resulting image represented by r is not re-compressed.

A zero-length md will result in r having no metadata at all.

The APP1 container wrapping the tags must not exceed 64kb.
ReplaceMeta calls Assert and will error under the same conditions.
It is unnecessary for callers to call Assert if they intend
to immediately follow with ReplaceMeta.

Since r is a wrapper around the new metadata and rs, altering
rs will affect r. Therefore callers are recommended to drain
r before altering rs.
*/
func ReplaceMeta(rs io.ReadSeeker, md Metadata) (r io.Reader, err error) {

	if err = Assert(rs); err != nil {
		return nil, err
	}

	p := make(scratch, 4, 4)

	// Return early if there's no metadata to write.
	if len(md) == 0 {
		if err = p.seekToDQT(rs); err != nil {
			return nil, err
		}
		return io.MultiReader(bytes.NewReader(soi), rs), nil
	}

	var buf bytes.Buffer
	var sorted []int

	/*
		We need ifdOffset to create pointers to the tag
		data below. We also need APP1's segment length so
		we finish calculating that while also ensuring a
		canonical ordering of the tags. (Ordering is not
		required by spec but hey why not).
	*/
	ifdOffset := 0
	ifdOffset += len(tiff)      //  8  bytes - Tiff header
	ifdOffset += 2              //  2  bytes - Number of IFD0 entries
	ifdOffset += (len(md) * 12) // 12+ bytes - Entries = num of entries * 12 bytes
	ifdOffset += len(ifd0Next)  //  4  bytes - Pointer to next IFD

	app1Len := 0
	app1Len += 4         // APP1 marker + length
	app1Len += len(exif) // Exif header
	app1Len += ifdOffset // Length of IFD0

	for t, v := range md {
		app1Len += len(v) + 1 // Add one for NULL byte terminator
		sorted = append(sorted, int(t))
	}
	sort.Ints(sorted)

	if app1Len > (1024 * 64) {
		return nil, errors.New("jpegutil: APP1 segment is too long")
	}

	buf.Write(soi)
	buf.Write(app1)
	buf.Write(p.bytes(app1Len, 2))
	buf.Write(exif)
	buf.Write(tiff)
	buf.Write(p.bytes(len(md), 2)) // Number of IFD0 entries.

	/*
		Begin appending Exif entries. All entries are 12 bytes
		and contain pointers to their data, therefore we must
		collect that data and write it after the entries and
		the pointer to the next IFD.
	*/
	var data []byte
	for _, t := range sorted {

		// Write tag and its type.
		buf.Write(tagMarker[tag(t)])

		// Data associated with tag - we add terminating NULL byte for ascii strings.
		newData := append([]byte(md[tag(t)]), 0x00)

		// Collect new data - we can't write it yet.
		data = append(data, newData...)

		// Convert integer length of payload into a byte slice.
		buf.Write(p.bytes(len(newData), 4))

		// Write pointer to payload.
		buf.Write(p.bytes(ifdOffset, 4))

		// Update pointer to next data offset.
		ifdOffset += len(newData)
	}

	// Declare there are no more IFDs.
	buf.Write(ifd0Next)

	// Write IFD0 data here.
	buf.Write(data)

	// Write segment padding between APP1 and DQT
	buf.Write(segPad)

	/*
		Set rs to the start of DQT segment so it transitions
		to that after our metadata with the multireader.
	*/
	if err = p.seekToDQT(rs); err != nil {
		return nil, err
	}

	/*
		Return a concatenation of our new metadata and the
		existing image data from the original JPEG source.
	*/
	return io.MultiReader(bytes.NewReader(buf.Bytes()), rs), nil
}

/*
scratch is for writing bytes to when converting integers
to bytes as well as for reading segment markers and their
lengths to. Methods of scratch assume it is 4 bytes long.
*/
type scratch []byte

func (p scratch) bytes(n, byteCount int) []byte {
	switch byteCount {
	case 2:
		binary.BigEndian.PutUint16(p, uint16(n))
		return p[0:2]
	case 4:
		binary.BigEndian.PutUint32(p, uint32(n))
		return p
	}
	panic("jpegutil: unexpected byteCount")
}

/*
seekToDQT seeks a JPEG file represented by rs to
the start of the first DQT marker. It assumes it
is being passed a JPEG file and does not check
to verify.
*/
func (p scratch) seekToDQT(rs io.ReadSeeker) (err error) {

	// Ensure we're reading from the file start.
	if _, err = rs.Seek(2, io.SeekStart); err != nil {
		return err
	}

	for {
		// Read next 4 bytes.
		n, err := rs.Read(p)
		if err != nil {
			return err
		}
		if n != 4 {
			return errors.New("jpegutil: couldn't read next segment marker and length")
		}

		// Break if we've hit the DQT marker
		if bytes.Equal(p[0:2], dqt) {
			break
		}

		// We subtract two because we've already read beyond the 2 byte length field.
		segLen := int64(binary.BigEndian.Uint16(p[2:4])) - 2

		/*
			Segment length at a minimum could be:

				2  bytes - length
				1+ bytes - payload
				1  byte  - terminator
				1  byte  - following header prefix

				5+ bytes - total

			We assume a payload of at least 2 bytes
			below (for a total segment length of 6).
		*/
		if segLen < 6 {
			return errors.New("jpegutil: reported segment length too small")
		}
		if _, err = rs.Seek(segLen, io.SeekCurrent); err != nil {
			return err
		}
	}

	// Seek back to the start of the DQT marker.
	if _, err = rs.Seek(-4, io.SeekCurrent); err != nil {
		return err
	}

	return nil
}

func closeFile(c io.Closer, err *error) {
	cErr := c.Close()
	if err == nil {
		*err = cErr
		return
	}
	if cErr == nil {
		return
	}
	*err = fmt.Errorf("%w: %v", *err, cErr)
}

/*
WriteFile drains r and writes it to a new file
at name, returning the number of bytes it wrote
and an error, if any.

If name doesn't already end in ".jpg" or ".jpeg"
WriteFile will add ".jpg" to the end.
*/
func WriteFile(name string, r io.Reader) (n int64, err error) {

	if ext := filepath.Ext(name); !(ext == ".jpg" || ext == ".jpeg") {
		name = strings.TrimRight(name, ".")
		name += ".jpg"
	}

	name, err = filepath.Abs(name)
	if err != nil {
		return n, fmt.Errorf("jpegutil: %w", err)
	}

	f, err := os.Create(name)
	if err != nil {
		return n, fmt.Errorf("jpegutil: %w", err)
	}
	defer closeFile(f, &err)

	tr := io.TeeReader(r, f)
	p := make([]byte, 64, 64)

	for {
		count, err := tr.Read(p)
		n += int64(count)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, fmt.Errorf("jpegutil: %w", err)
		}
	}

	return n, nil
}
