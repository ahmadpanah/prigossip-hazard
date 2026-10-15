package gossip

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Wire format (Section "Gossip Transport Daemon"):
//
//	[Header: 4B][Timestamp: 8B]{[InstanceID: 4B][LaplaceVariance: 4B]} x M
//
// Header layout: magic uint16 | version uint8 | count uint8.
const (
	headerSize        = 12
	recordSize        = 8
	magic      uint16 = 0x5047 // "PG"
	version    byte   = 1
)

// Datagram is one gossip message carrying the perturbed variance sketch.
type Datagram struct {
	Timestamp uint64    // unix nanoseconds of the transmitting proxy
	Variances []float64 // \tilde{v}_k(t), one entry per instance
}

// Size returns the encoded byte length: 12 + 8*M bytes, always far below the
// 1500-byte path MTU for realistic instance counts.
func (d Datagram) Size() int {
	return headerSize + recordSize*len(d.Variances)
}

// Encode serializes the datagram.
func (d Datagram) Encode() ([]byte, error) {
	if len(d.Variances) > 255 {
		return nil, fmt.Errorf("too many instances for wire format: %d", len(d.Variances))
	}
	buf := make([]byte, d.Size())
	binary.LittleEndian.PutUint16(buf[0:2], magic)
	buf[2] = version
	buf[3] = byte(len(d.Variances))
	binary.LittleEndian.PutUint64(buf[4:12], d.Timestamp)
	off := headerSize
	for i, v := range d.Variances {
		binary.LittleEndian.PutUint32(buf[off:off+4], uint32(i))
		binary.LittleEndian.PutUint32(buf[off+4:off+8], math.Float32bits(float32(v)))
		off += recordSize
	}
	return buf, nil
}

// Decode parses a received datagram.
func Decode(b []byte) (Datagram, error) {
	var d Datagram
	if len(b) < headerSize {
		return d, fmt.Errorf("datagram too short: %d bytes", len(b))
	}
	if binary.LittleEndian.Uint16(b[0:2]) != magic {
		return d, fmt.Errorf("bad magic")
	}
	if b[2] != version {
		return d, fmt.Errorf("unsupported version %d", b[2])
	}
	count := int(b[3])
	if len(b) < headerSize+count*recordSize {
		return d, fmt.Errorf("truncated payload: %d bytes for %d records", len(b), count)
	}
	d.Timestamp = binary.LittleEndian.Uint64(b[4:12])
	d.Variances = make([]float64, count)
	off := headerSize
	for i := 0; i < count; i++ {
		idx := int(binary.LittleEndian.Uint32(b[off : off+4]))
		val := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[off+4 : off+8])))
		if idx >= 0 && idx < count {
			d.Variances[idx] = val
		}
		off += recordSize
	}
	return d, nil
}
