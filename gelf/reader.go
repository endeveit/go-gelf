// Copyright 2012 SocialCode. All rights reserved.
// Use of this source code is governed by the MIT
// license that can be found in the LICENSE file.

package gelf

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

type Reader struct {
	mu   sync.Mutex
	conn net.Conn
}

func NewReader(addr string) (*Reader, error) {
	var err error
	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, fmt.Errorf("ResolveUDPAddr('%s'): %s", addr, err)
	}

	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, fmt.Errorf("ListenUDP: %s", err)
	}

	r := new(Reader)
	r.conn = conn

	return r, nil
}

func (r *Reader) Addr() string {
	return r.conn.LocalAddr().String()
}

func (r *Reader) GetConnection() net.Conn {
	return r.conn
}

// FIXME: this will discard data if p isn't big enough to hold the
// full message.
func (r *Reader) Read(p []byte) (int, error) {
	msg, err := r.ReadMessage()
	if err != nil {
		return -1, err
	}

	var data string

	if msg.Full == "" {
		data = msg.Short
	} else {
		data = msg.Full
	}

	return strings.NewReader(data).Read(p)
}

func (r *Reader) ReadMessage() (msg *Message, err error) {
	var (
		mapped map[string]interface{}
		extra  map[string]interface{} = make(map[string]interface{})
	)

	mapped, err = r.readToMap()

	if err != nil {
		return nil, err
	}

	msg = new(Message)

	if val, ok := mapped["version"]; ok && val != nil {
		msg.Version = val.(string)
	}

	if val, ok := mapped["host"]; ok && val != nil {
		msg.Host = val.(string)
	}

	if val, ok := mapped["short_message"]; ok && val != nil {
		msg.Short = val.(string)
	}

	if val, ok := mapped["full_message"]; ok && val != nil {
		v := val.(string)

		if len(v) > 0 {
			msg.Full = v
		}
	}

	if val, ok := mapped["timestamp"]; ok && val != nil {
		switch val.(type) {
		case string:
			v, err := strconv.ParseFloat(val.(string), 64)
			if err == nil {
				msg.TimeUnix = v
			}
		case float64:
			msg.TimeUnix = val.(float64)
		}
	}

	if val, ok := mapped["level"]; ok && val != nil {
		switch val.(type) {
		case float64:
			msg.Level = int32(val.(float64))
		case int32:
			msg.Level = val.(int32)
		}
	}

	if val, ok := mapped["facility"]; ok && val != nil {
		v := val.(string)

		if len(v) > 0 {
			msg.Facility = v
		}
	}

	if val, ok := mapped["file"]; ok && val != nil {
		v := val.(string)

		if len(v) > 0 {
			msg.File = v
		}
	}

	if val, ok := mapped["line"]; ok && val != nil {
		switch val.(type) {
		case float64:
			msg.Line = int32(val.(float64))
		case int32:
			msg.Line = val.(int32)
		}
	}

	// Move fields started with underscore into "Extra"
	for k, v := range mapped {
		if strings.HasPrefix(k, "_") && v != nil {
			extra[k[1:len(k)]] = v
		}
	}

	if len(extra) > 0 {
		msg.Extra = extra
	}

	return msg, nil
}

func (r *Reader) readToMap() (msg map[string]interface{}, err error) {
	cBuf := make([]byte, ChunkSize)
	var (
		n, length  int
		cid, ocid  []byte
		seq, total uint8
		cHead      []byte
		cReader    io.Reader
		chunks     [][]byte
	)

	for got := 0; got < 128 && (total == 0 || got < int(total)); got++ {
		if n, err = r.conn.Read(cBuf); err != nil {
			return nil, err
		}
		cHead, cBuf = cBuf[:2], cBuf[:n]

		if bytes.Equal(cHead, magicChunked) {
			//fmt.Printf("chunked %v\n", cBuf[:14])
			cid, seq, total = cBuf[2:2+8], cBuf[2+8], cBuf[2+8+1]
			if ocid != nil && !bytes.Equal(cid, ocid) {
				return nil, fmt.Errorf("out-of-band message %v (awaited %v)", cid, ocid)
			} else if ocid == nil {
				ocid = cid
				chunks = make([][]byte, total)
			}
			n = len(cBuf) - chunkedHeaderLen
			//fmt.Printf("setting chunks[%d]: %d\n", seq, n)
			chunks[seq] = append(make([]byte, 0, n), cBuf[chunkedHeaderLen:]...)
			length += n
		} else { //not chunked
			if total > 0 {
				return nil, fmt.Errorf("out-of-band message (not chunked)")
			}
			break
		}
	}
	//fmt.Printf("\nchunks: %v\n", chunks)

	if length > 0 {
		if cap(cBuf) < length {
			cBuf = append(cBuf, make([]byte, 0, length-cap(cBuf))...)
		}
		cBuf = cBuf[:0]
		for i := range chunks {
			//fmt.Printf("appending %d %v\n", i, chunks[i])
			cBuf = append(cBuf, chunks[i]...)
		}
		cHead = cBuf[:2]
	}

	// the data we get from the wire is compressed
	if bytes.Equal(cHead, magicGzip) {
		cReader, err = gzip.NewReader(bytes.NewReader(cBuf))
	} else if cHead[0] == magicZlib[0] &&
		(int(cHead[0])*256+int(cHead[1]))%31 == 0 {
		// zlib is slightly more complicated, but correct
		cReader, err = zlib.NewReader(bytes.NewReader(cBuf))
	} else {
		// compliance with https://github.com/Graylog2/graylog2-server
		// treating all messages as uncompressed if  they are not gzip, zlib or
		// chunked
		cReader = bytes.NewReader(cBuf)
	}

	if err != nil {
		return nil, fmt.Errorf("NewReader: %s", err)
	}

	if err := json.NewDecoder(cReader).Decode(&msg); err != nil {
		return nil, fmt.Errorf("json.Unmarshal: %s", err)
	}

	return msg, nil
}
