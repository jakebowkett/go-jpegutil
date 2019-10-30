/*
Package jpegutil provides a simple way to handle some common
jpeg tasks such as stripping/changing metadata.
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
Assert returns an error if r doesn't represent
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

type MetaData map[tag]string

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
SetMeta takes a JPEG file represented by rs and returns
a reader r which is the same file with its EXIF data
replaced with the supplied metadata tags and their values.
The resulting image represented by r is not re-compressed.

A zero-length md will result in r having no metadata at all.

The APP1 container wrapping the tags must not exceed 64kb.
SetMeta calls Assert and will error under the same conditions.
It is unnecessary for callers to call Assert if they intend
to immediately follow with SetMeta.

Since r is a wrapper around the new metadata and rs, altering
rs will affect r. Therefore callers are recommended to drain
r before altering rs.
*/
func SetMeta(rs io.ReadSeeker, md MetaData) (r io.Reader, err error) {

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
		We need ifdOffset to create pointers to the data
		below. We also need APP1's segment length so we
		finish calculating while also ensuring a canonical
		ordering of the tags.
	*/
	ifdOffset := 0
	ifdOffset += len(tiff)      //  8  bytes - Tiff header
	ifdOffset += 2              //  2  bytes - Number of IFD0 directory entries
	ifdOffset += (len(md) * 12) // 12+ bytes - Entries
	ifdOffset += len(ifd0Next)  //  4  bytes - Pointer to next IFD directory

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
	buf.Write(p.bytes(app1Len, 2)) // APP1 length.
	buf.Write(exif)
	buf.Write(tiff)
	buf.Write(p.bytes(len(md), 2)) // Number of IFD0 directory entries.

	// Begin appending exif tags.
	var data []byte
	for _, t := range sorted {

		// Write tag and its type.
		buf.Write(tagMarker[tag(t)])

		// Value associated with tag - we add terminating NULL byte for ascii strings.
		newData := append([]byte(md[tag(t)]), 0x00)

		// Collect new data - we can't write it yet.
		data = append(data, newData...)

		// Convert integer length of payload into a byte array.
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
		Set rs to the start of DQT segment so it
		transitions to that after our metadata.
	*/
	if err = p.seekToDQT(rs); err != nil {
		return nil, err
	}

	return io.MultiReader(bytes.NewReader(buf.Bytes()), rs), nil
}

/*
scratch is for writing bytes to when converting integers
to bytes as well as for reading segment markers and their
lengths to.
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
	panic("jpegutil: unexpected byteCount value")
}

/*
seekToDQT seeks a JPEG file represented by rs to
the start of the first DQT marker.
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

		// Break on hitting DQT marker
		if bytes.Equal(p[0:2], dqt) {
			break
		}

		// We subtract two because we've already read beyond the 2 byte length field.
		segLen := int64(binary.BigEndian.Uint16(p[2:4])) - 2

		/*
			Segment length at a minimum could be:

				2  bytes - header
				2  bytes - length
				1+ bytes - payload
				1  byte  - terminator
				1  byte  - following header prefix

				7+ bytes - total

			We assume a payload of at least 2 bytes
			below (for a total segment length of 8).
		*/
		if segLen < 8 {
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
at name, returning the bytes it wrote and an
error, if any.
*/
func WriteFile(name string, r io.Reader) (n int64, err error) {

	name, err = filepath.Abs(name)
	if err != nil {
		return n, err
	}

	f, err := os.Create(name)
	if err != nil {
		return n, err
	}
	defer closeFile(f, &err)

	tr := io.TeeReader(r, f)
	p := make([]byte, 64, 64)

	for {
		count, err := tr.Read(p)
		if errors.Is(err, io.EOF) {
			n += int64(count)
			break
		}
		if err != nil {
			return n, err
		}
	}

	return n, nil
}
