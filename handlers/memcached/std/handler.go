// Copyright 2015 Netflix, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package std

import (
	"bufio"
	"io"

	"github.com/netflix/rend/common"
	"github.com/netflix/rend/metrics"
	"github.com/netflix/rend/protocol/binprot"
)

func readResponseHeader(r *bufio.Reader) (binprot.ResponseHeader, error) {
	resHeader, err := binprot.ReadResponseHeader(r)
	if err != nil {
		return binprot.ResponseHeader{}, err
	}

	if err := binprot.DecodeError(resHeader); err != nil {
		binprot.PutResponseHeader(resHeader)
		return resHeader, err
	}

	binprot.PutResponseHeader(resHeader)
	return resHeader, nil
}

// Handler implements a backend for Rend that communicates to a remote memcached server
type Handler struct {
	rw   *bufio.ReadWriter
	conn io.Closer
}

// NewHandler returns an implementation of handlers.Handler that implements a straightforward
// request-response like normal memcached usage.
func NewHandler(conn io.ReadWriteCloser) Handler {
	rw := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
	return Handler{
		rw:   rw,
		conn: conn,
	}
}

// Close closes the Handler's underlying io.ReadWriteCloser.
// Any calls to the handler after Close is called are invalid.
func (h Handler) Close() error {
	return h.conn.Close()
}

// Set performs a set request on the remote backend
func (h Handler) Set(cmd common.SetRequest) error {
	if err := binprot.WriteSetCmd(h.rw.Writer, cmd.Key, cmd.Flags, cmd.Exptime, uint32(len(cmd.Data)), 0); err != nil {
		return err
	}
	return h.handleSetCommon(cmd)
}

// Add performs an add request on the remote backend
func (h Handler) Add(cmd common.SetRequest) error {
	if err := binprot.WriteAddCmd(h.rw.Writer, cmd.Key, cmd.Flags, cmd.Exptime, uint32(len(cmd.Data)), 0); err != nil {
		return err
	}
	return h.handleSetCommon(cmd)
}

// Replace performs a replace request on the remote backend
func (h Handler) Replace(cmd common.SetRequest) error {
	if err := binprot.WriteReplaceCmd(h.rw.Writer, cmd.Key, cmd.Flags, cmd.Exptime, uint32(len(cmd.Data)), 0); err != nil {
		return err
	}
	return h.handleSetCommon(cmd)
}

// Append performs an append request on the remote backend
func (h Handler) Append(cmd common.SetRequest) error {
	if err := binprot.WriteAppendCmd(h.rw.Writer, cmd.Key, cmd.Flags, cmd.Exptime, uint32(len(cmd.Data)), 0); err != nil {
		return err
	}
	return h.handleSetCommon(cmd)
}

// Prepend performs a prepend request on the remote backend
func (h Handler) Prepend(cmd common.SetRequest) error {
	if err := binprot.WritePrependCmd(h.rw.Writer, cmd.Key, cmd.Flags, cmd.Exptime, uint32(len(cmd.Data)), 0); err != nil {
		return err
	}
	return h.handleSetCommon(cmd)
}

func (h Handler) handleSetCommon(cmd common.SetRequest) error {
	// TODO: should there be a unique flags value for regular data?

	// Write value
	h.rw.Write(cmd.Data)
	metrics.IncCounterBy(common.MetricBytesWrittenLocal, uint64(len(cmd.Data)))

	if err := h.rw.Flush(); err != nil {
		return err
	}

	// Read server's response
	resHeader, err := readResponseHeader(h.rw.Reader)
	if err != nil {
		// Discard response body
		n, ioerr := h.rw.Discard(int(resHeader.TotalBodyLength))
		metrics.IncCounterBy(common.MetricBytesReadLocal, uint64(n))
		if ioerr != nil {
			return ioerr
		}

		// For Add and Replace, the error here will be common.ErrKeyExists or common.ErrKeyNotFound
		// respectively. For each, this is the right response to send to the requestor. The error
		// here is overloaded because it would signal a true error for sets, but a normal "error"
		// response for Add and Replace.
		return err
	}

	return nil
}

// Get performs a batched get request on the remote backend. The channels returned
// are expected to be read from until either a single error is received or the
// response channel is exhausted.
func (h Handler) Get(cmd common.GetRequest) (<-chan common.GetResponse, <-chan error) {
	dataOut := make(chan common.GetResponse)
	errorOut := make(chan error)
	go realHandleGet(cmd, dataOut, errorOut, h.rw)
	return dataOut, errorOut
}

func realHandleGet(cmd common.GetRequest, dataOut chan common.GetResponse, errorOut chan error, rw *bufio.ReadWriter) {
	defer close(errorOut)
	defer close(dataOut)

	for idx, key := range cmd.Keys {
		if err := binprot.WriteGetCmd(rw.Writer, key, 0); err != nil {
			errorOut <- err
			return
		}

		data, flags, _, err := getLocal(rw, false)
		if err != nil {
			if err == common.ErrKeyNotFound {
				dataOut <- common.GetResponse{
					Miss:   true,
					Quiet:  cmd.Quiet[idx],
					Opaque: cmd.Opaques[idx],
					Flags:  flags,
					Key:    key,
					Data:   nil,
				}

				continue
			}

			errorOut <- err
			return
		}

		dataOut <- common.GetResponse{
			Miss:   false,
			Quiet:  cmd.Quiet[idx],
			Opaque: cmd.Opaques[idx],
			Flags:  flags,
			Key:    key,
			Data:   data,
		}
	}
}

// GetE performs a batched gete request on the remote backend. The channels returned
// are expected to be read from until either a single error is received or the
// response channel is exhausted.
func (h Handler) GetE(cmd common.GetRequest) (<-chan common.GetEResponse, <-chan error) {
	dataOut := make(chan common.GetEResponse)
	errorOut := make(chan error)
	go realHandleGetE(cmd, dataOut, errorOut, h.rw)
	return dataOut, errorOut
}

func realHandleGetE(cmd common.GetRequest, dataOut chan common.GetEResponse, errorOut chan error, rw *bufio.ReadWriter) {
	defer close(errorOut)
	defer close(dataOut)

	for idx, key := range cmd.Keys {
		if err := binprot.WriteGetECmd(rw.Writer, key, 0); err != nil {
			errorOut <- err
			return
		}

		data, flags, exp, err := getLocal(rw, true)
		if err != nil {
			if err == common.ErrKeyNotFound {
				dataOut <- common.GetEResponse{
					Miss:    true,
					Quiet:   cmd.Quiet[idx],
					Opaque:  cmd.Opaques[idx],
					Flags:   flags,
					Exptime: exp,
					Key:     key,
					Data:    nil,
				}

				continue
			}

			errorOut <- err
			return
		}

		dataOut <- common.GetEResponse{
			Miss:    false,
			Quiet:   cmd.Quiet[idx],
			Opaque:  cmd.Opaques[idx],
			Flags:   flags,
			Exptime: exp,
			Key:     key,
			Data:    data,
		}
	}
}

// GAT performs a get-and-touch request on the remote backend
func (h Handler) GAT(cmd common.GATRequest) (common.GetResponse, error) {
	if err := binprot.WriteGATCmd(h.rw.Writer, cmd.Key, cmd.Exptime, 0); err != nil {
		return common.GetResponse{}, err
	}

	data, flags, _, err := getLocal(h.rw, false)
	if err != nil {
		if err == common.ErrKeyNotFound {
			return common.GetResponse{
				Miss:   true,
				Quiet:  false,
				Opaque: cmd.Opaque,
				Flags:  flags,
				Key:    cmd.Key,
				Data:   nil,
			}, nil
		}

		return common.GetResponse{}, err
	}

	return common.GetResponse{
		Miss:   false,
		Quiet:  false,
		Opaque: cmd.Opaque,
		Flags:  flags,
		Key:    cmd.Key,
		Data:   data,
	}, nil
}

// Delete performs a delete request on the remote backend
func (h Handler) Delete(cmd common.DeleteRequest) error {
	if err := binprot.WriteDeleteCmd(h.rw.Writer, cmd.Key, 0); err != nil {
		return err
	}
	return simpleCmdLocal(h.rw)
}

// Touch performs a touch request on the remote backend
func (h Handler) Touch(cmd common.TouchRequest) error {
	if err := binprot.WriteTouchCmd(h.rw.Writer, cmd.Key, cmd.Exptime, 0); err != nil {
		return err
	}
	return simpleCmdLocal(h.rw)
}
