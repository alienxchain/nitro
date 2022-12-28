// Copyright 2021-2022, Offchain Labs, Inc.
// For license information, see https://github.com/nitro/blob/master/LICENSE

package wsbroadcastserver

import (
	"compress/flate"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsflate"
	"github.com/gobwas/ws/wsutil"
)

type chainedReader struct {
	readers []io.Reader
}

func logError(err error, msg string) {
	if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		log.Error(msg, "err", err)
	}
}

func logWarn(err error, msg string) {
	if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
		log.Warn(msg, "err", err)
	}
}

func (cr *chainedReader) Read(b []byte) (n int, err error) {
	for len(cr.readers) > 0 {
		n, err = cr.readers[0].Read(b)
		if errors.Is(err, io.EOF) {
			cr.readers = cr.readers[1:]
			if n == 0 {
				continue // EOF and empty, skip to next
			} else {
				// The Read interface specifies some data can be returned along with an EOF.
				if len(cr.readers) != 1 {
					// If this isn't the last reader, return the data without the EOF since this
					// may not be the end of all the readers.
					return n, nil
				} else {
					return
				}
			}
		}
		break
	}
	return
}

func (cr *chainedReader) add(r io.Reader) *chainedReader {
	if r != nil {
		cr.readers = append(cr.readers, r)
	}
	return cr
}

const DeflateCompressionLevel = flate.BestCompression

func GetStaticCompressorDictionary() []byte {
	// TODO
	return []byte("whatever{}")
}

func ReadData(ctx context.Context, conn net.Conn, earlyFrameData io.Reader, idleTimeout time.Duration, state ws.State, compression bool, flateReader *wsflate.Reader) ([]byte, ws.OpCode, *wsflate.Reader, error) {
	if compression {
		state |= ws.StateExtended
	}
	controlHandler := wsutil.ControlFrameHandler(conn, state)
	var msg wsflate.MessageState
	reader := wsutil.Reader{
		Source:          (&chainedReader{}).add(earlyFrameData).add(conn),
		State:           state,
		CheckUTF8:       !compression,
		SkipHeaderCheck: false,
		OnIntermediate:  controlHandler,
		Extensions:      []wsutil.RecvExtension{&msg},
	}

	// Remove timeout when leaving this function
	defer func(conn net.Conn) {
		err := conn.SetReadDeadline(time.Time{})
		logError(err, "error removing read deadline")
	}(conn)

	for {
		select {
		case <-ctx.Done():
			return nil, 0, flateReader, nil
		default:
		}

		err := conn.SetReadDeadline(time.Now().Add(idleTimeout))
		if err != nil {
			return nil, 0, flateReader, err
		}

		// Control packet may be returned even if err set
		header, err := reader.NextFrame()
		if header.OpCode.IsControl() {
			// Control packet may be returned even if err set
			if err2 := controlHandler(header, &reader); err2 != nil {
				return nil, 0, flateReader, err2
			}

			// Discard any data after control packet
			if err2 := reader.Discard(); err2 != nil {
				return nil, 0, nil, err2
			}

			return nil, 0, flateReader, nil
		}
		if err != nil {
			return nil, 0, flateReader, err
		}

		if header.OpCode != ws.OpText &&
			header.OpCode != ws.OpBinary {
			if err := reader.Discard(); err != nil {
				return nil, 0, flateReader, err
			}
			continue
		}
		var data []byte
		if msg.IsCompressed() {
			if !compression {
				log.Warn("Received compressed frame even though compression is disabled, discarding the frame")
				if err := reader.Discard(); err != nil {
					return nil, 0, nil, err
				}
				continue
			}
			if flateReader == nil {
				flateReader = wsflate.NewReader(nil, func(r io.Reader) wsflate.Decompressor {
					return flate.NewReaderDict(r, GetStaticCompressorDictionary())
				})
			}
			flateReader.Reset(&reader)
			data, err = io.ReadAll(flateReader)
		} else {
			data, err = io.ReadAll(&reader)
		}

		return data, header.OpCode, flateReader, err
	}
}
