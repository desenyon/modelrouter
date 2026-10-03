// Package sse is a minimal, allocation-conscious Server-Sent Events reader.
package sse

import (
	"bufio"
	"bytes"
	"io"
)

// Event is one SSE message.
type Event struct {
	Name string
	Data []byte // valid until the next call to Next
}

// Reader parses an SSE stream.
type Reader struct {
	br   *bufio.Reader
	data bytes.Buffer
	name string
}

// NewReader wraps r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64<<10)}
}

// Next returns the next event, or io.EOF at end of stream.
func (r *Reader) Next() (Event, error) {
	r.data.Reset()
	r.name = ""
	have := false
	for {
		line, err := r.br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Very long line: accumulate the remainder.
			buf := append([]byte(nil), line...)
			for err == bufio.ErrBufferFull {
				line, err = r.br.ReadSlice('\n')
				buf = append(buf, line...)
			}
			line = buf
		}
		if len(line) == 0 && err != nil {
			if have {
				return Event{Name: r.name, Data: r.data.Bytes()}, nil
			}
			return Event{}, err
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			if have {
				return Event{Name: r.name, Data: r.data.Bytes()}, nil
			}
			if err != nil {
				return Event{}, err
			}
			continue
		}
		if line[0] == ':' {
			continue // comment / keep-alive
		}
		field, value := line, []byte(nil)
		if i := bytes.IndexByte(line, ':'); i >= 0 {
			field, value = line[:i], line[i+1:]
			if len(value) > 0 && value[0] == ' ' {
				value = value[1:]
			}
		}
		switch string(field) {
		case "event":
			r.name = string(value)
			have = true
		case "data":
			if r.data.Len() > 0 {
				r.data.WriteByte('\n')
			}
			r.data.Write(value)
			have = true
		}
		if err != nil {
			if have {
				return Event{Name: r.name, Data: r.data.Bytes()}, nil
			}
			return Event{}, err
		}
	}
}
