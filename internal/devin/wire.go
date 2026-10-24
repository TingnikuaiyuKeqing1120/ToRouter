package devin

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

const maxWire = 16 << 20

type field struct {
	number int
	kind   int
	n      uint64
	data   []byte
}

func fields(data []byte) ([]field, error) {
	out := []field{}
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 || tag>>3 == 0 || tag>>3 > (1<<29)-1 {
			return nil, errors.New("invalid RPC field tag")
		}
		data = data[n:]
		f := field{number: int(tag >> 3), kind: int(tag & 7)}
		switch f.kind {
		case 0:
			f.n, n = binary.Uvarint(data)
			if n <= 0 {
				return nil, errors.New("invalid RPC integer")
			}
			data = data[n:]
		case 1:
			if len(data) < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			f.data, data = data[:8], data[8:]
		case 2:
			length, size := binary.Uvarint(data)
			if size <= 0 || length > maxWire || length > uint64(len(data)-size) {
				return nil, errors.New("invalid RPC field size")
			}
			f.data, data = data[size:size+int(length)], data[size+int(length):]
		case 5:
			if len(data) < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			f.data, data = data[:4], data[4:]
		default:
			return nil, errors.New("unsupported RPC wire type")
		}
		out = append(out, f)
	}
	return out, nil
}

type message []byte

func (m message) bytes(number int, value []byte) message {
	m = binary.AppendUvarint(m, uint64(number<<3|2))
	m = binary.AppendUvarint(m, uint64(len(value)))
	return append(m, value...)
}
func (m message) text(number int, value string) message { return m.bytes(number, []byte(value)) }
func (m message) integer(number int, value uint64) message {
	m = binary.AppendUvarint(m, uint64(number<<3))
	return binary.AppendUvarint(m, value)
}
func (m message) decimal(number int, value float64) message {
	m = binary.AppendUvarint(m, uint64(number<<3|1))
	return binary.LittleEndian.AppendUint64(m, math.Float64bits(value))
}
func frame(payload []byte) []byte {
	out := make([]byte, 5, len(payload)+5)
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload)))
	return append(out, payload...)
}
func readFrame(r io.Reader) (byte, []byte, error) {
	var header [5]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return 0, nil, err
	}
	flags, size := header[0], binary.BigEndian.Uint32(header[1:])
	if flags & ^byte(3) != 0 || size > maxWire {
		return 0, nil, errors.New("invalid Connect frame")
	}
	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	if flags&1 != 0 {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return 0, nil, errors.New("invalid compressed Connect frame")
		}
		data, err = io.ReadAll(io.LimitReader(reader, maxWire+1))
		reader.Close()
		if err != nil || len(data) > maxWire {
			return 0, nil, errors.New("invalid expanded Connect frame")
		}
	}
	return flags, data, nil
}
