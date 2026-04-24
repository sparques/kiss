// Package kiss provides helpers for reading and writing KISS-framed packet
// radio data over a shared byte stream.
//
// A TNC created with NewTNC multiplexes one io.ReadWriter into eight logical
// KISS ports. Incoming frames are decoded in a background goroutine and routed
// to per-port receive queues.
//
// Use Port to exchange ordinary data frames. Reads from a port return the frame
// payload without the leading KISS header byte, and writes automatically encode
// the selected port number with FrameTypeData.
//
// Use CommandPort to exchange non-data KISS command frames such as TX delay or
// slot time settings. CommandPort.Read returns the full decoded frame including
// the leading port-and-command byte. For CommandPort.Write, the first byte of
// the supplied slice becomes the command nibble in the outbound KISS header.
//
// FrameEncode and Split are also available when you need direct control over
// frame encoding or scanner-based decoding outside of TNC.
package kiss

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

const (
	// QueueDepth is the number of frames buffered per receive queue before the
	// oldest queued frame is discarded.
	QueueDepth = 512
)

const (
	// FEND marks the beginning and end of a KISS frame.
	FEND = 0xC0
	// FESC introduces an escaped control byte inside a frame payload.
	FESC = 0xDB
	// TFEND is the escaped representation of FEND after FESC.
	TFEND = 0xDC
	// TFESC is the escaped representation of FESC after FESC.
	TFESC = 0xDD

	// FrameTypeData identifies a data frame for normal payload traffic.
	FrameTypeData = Command(0x00)
	// FrameTypeTXDelay configures the key-up delay in 10 ms units.
	FrameTypeTXDelay = Command(0x01)
	// FrameTypeP configures the persistence parameter used for CSMA.
	FrameTypeP = Command(0x02)
	// FrameTypeSlotTime configures the slot time in 10 ms units for CSMA.
	FrameTypeSlotTime = Command(0x03)
	// FrameTypeTXTail configures the transmit tail time in 10 ms units.
	FrameTypeTXTail = Command(0x04)
	// FrameTypeFullDuplex toggles half- versus full-duplex operation.
	FrameTypeFullDuplex = Command(0x05)
	// FrameTypeSetHardware carries device-specific hardware configuration.
	FrameTypeSetHardware = Command(0x06)
	// FrameTypeReturn exits KISS mode when sent as a command frame.
	FrameTypeReturn = Command(0xFF)
)

// Command is the low four-bit KISS command value stored in the frame header.
type Command uint8

// Is reports whether two command bytes refer to the same KISS command, ignoring
// the port bits stored in the high nibble.
func Is(a, b Command) bool {
	return a&0x0F == b&0x0F
}

var (
	// ErrInvalidPort reports an out-of-range KISS port number.
	ErrInvalidPort = errors.New("invalid port: must be 0-7")
)

// TODO: Add a slog logger that defaults to io.Discard;
// Package function can change the destination.

// TNC multiplexes a single KISS byte stream into eight logical ports.
type TNC struct {
	// ports holds the logical KISS ports exposed by the shared transport.
	ports [8]port
	// closed reports whether the background router has stopped consuming frames.
	closed bool
}

// port provides framed data I/O for one logical KISS port.
type port struct {
	// id is the KISS port number encoded into transmitted frame headers.
	id uint8
	// rw is the shared transport used to send encoded KISS frames.
	rw io.ReadWriter
	// queue buffers decoded data frames destined for this port.
	queue chan []byte
	// cmdQueue buffers decoded command frames destined for this port.
	cmdQueue chan []byte
}

// commandPort exposes command-frame I/O for a logical KISS port.
type commandPort struct {
	*port
}

// NewTNC constructs a TNC backed by rw and starts a router goroutine that
// demultiplexes incoming frames onto per-port queues.
func NewTNC(rw io.ReadWriter) *TNC {
	t := &TNC{}
	for i := range t.ports {
		t.ports[i] = port{
			id:       uint8(i),
			rw:       rw,
			queue:    make(chan []byte, QueueDepth),
			cmdQueue: make(chan []byte, QueueDepth),
		}
	}

	go t.router(rw)

	return t
}

// router scans rd for KISS frames and dispatches them to the appropriate port
// queue until the underlying stream ends or returns an unrecoverable error.
func (t *TNC) router(rd io.Reader) {
	scanner := bufio.NewScanner(rd)
	scanner.Split(Split)
	for scanner.Scan() {
		// scanner.Bytes contains one decoded KISS frame without FEND markers.
		frame := scanner.Bytes()
		if len(frame) == 0 {
			continue
		}
		port := frame[0] >> 4 // will definitely be < 8
		if Is(Command(frame[0]), FrameTypeData) {
			t.enqueue(port, frame[1:])
		} else {
			t.enqueueCommand(port, frame)
		}
	}

	// Scanner terminated and this implementation does not recover. Close the
	// data queues so port readers can observe EOF.
	for i := range t.ports {
		close(t.ports[i].queue)
	}

	t.closed = true
}

// enqueue appends a decoded data frame to the target port queue, dropping the
// oldest queued frame when the queue is full.
func (t *TNC) enqueue(port uint8, data []byte) {
	// Drop the oldest pending frame to preserve a bounded queue size.
	if t.ports[port].free() == 0 {
		<-t.ports[port].queue
	}

	t.ports[port].queue <- data[:]
}

// enqueueCommand appends a decoded command frame to the target command queue,
// dropping the oldest queued command when the queue is full.
func (t *TNC) enqueueCommand(port uint8, data []byte) {
	// Drop the oldest pending command to preserve a bounded queue size.
	if t.ports[port].cmdFree() == 0 {
		<-t.ports[port].cmdQueue
	}

	t.ports[port].cmdQueue <- data[:]
}

// IsClosed reports whether the router goroutine has stopped and closed the data
// queues.
func (t *TNC) IsClosed() bool {
	return t.closed
}

// Port returns the data port for n, clamping values greater than 7 to port 7.
func (t *TNC) Port(n uint8) *port {
	n = min(n, 7)
	return &t.ports[n]
}

// CommandPort returns the command port for n, clamping values greater than 7
// to port 7.
func (t *TNC) CommandPort(n uint8) *commandPort {
	n = min(n, 7)
	return &commandPort{&t.ports[n]}
}

// Read copies the next queued data frame into data and returns the full frame
// length, even when data is too small to hold the entire payload.
func (p *port) Read(data []byte) (n int, err error) {
	frame, ok := <-p.queue // Block until a frame is available.
	if !ok {
		return 0, io.EOF // Channel closed.
	}
	copy(data, frame)
	return len(frame), nil
}

// Write encodes data as a KISS data frame for this port and writes it to the
// shared transport.
func (p *port) Write(data []byte) (n int, err error) {
	frame := FrameEncode(p.id<<4, data)
	written, err := p.rw.Write(frame)
	if err != nil {
		return 0, err
	}
	if written != len(frame) {
		return 0, io.ErrShortWrite
	}
	return len(data), nil
}

// free returns the remaining capacity in the port's data queue.
func (p *port) free() int {
	return cap(p.queue) - len(p.queue)
}

// free returns the remaining capacity in the port's data queue.
func (p *port) cmdFree() int {
	return cap(p.cmdQueue) - len(p.cmdQueue)
}

// Read copies the next queued command frame into data and returns the full
// frame length, even when data is too small to hold the entire payload.
func (cp *commandPort) Read(data []byte) (n int, err error) {
	frame, ok := <-cp.cmdQueue // Block until a frame is available.
	if !ok {
		return 0, io.EOF // Channel closed.
	}
	copy(data, frame)
	return len(frame), nil
}

// Write encodes data as a KISS command frame for this port. The first byte of
// data supplies the command value stored in the frame header. The port nybble
// is masked--you cannot write to a different port by specifying the port in
// the first byte.
func (cp *commandPort) Write(data []byte) (n int, err error) {
	if len(data) == 0 {
		return 0, nil
	}
	frame := FrameEncode(cp.id<<4|(data[0]&0x0f), data)
	written, err := cp.rw.Write(frame)
	if err != nil {
		return 0, err
	}
	if written != len(frame) {
		return 0, io.ErrShortWrite
	}
	return len(data), nil
}

// FrameEncode wraps data in a KISS frame, escaping control bytes and adding
// zero padding so short payloads reach the minimum AX.25 frame size.
func FrameEncode(portCmd byte, data []byte) []byte {
	// If no bytes need escaping, len(data)+3 is the exact encoded size.
	buf := bytes.NewBuffer(make([]byte, 0, len(data)+3))
	buf.WriteByte(FEND)
	buf.WriteByte(portCmd)
	for i := range len(data) {
		switch data[i] {
		case FEND:
			buf.Write([]byte{FESC, TFEND})
		case FESC:
			buf.Write([]byte{FESC, TFESC})
		default:
			buf.WriteByte(data[i])
		}
	}
	// Pad short payloads to the minimum AX.25 information field length.
	if len(data) <= 14 {
		buf.Write(bytes.Repeat([]byte{0}, 14-len(data)))
	}
	buf.WriteByte(FEND)

	return buf.Bytes()
}

// Split is a bufio.SplitFunc that extracts one decoded KISS frame at a time.
// Returned tokens exclude FEND delimiters and have escape sequences resolved.
func Split(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}

	start := bytes.IndexByte(data, FEND)
	if start == -1 {
		// No frame start found; skip to EOF or wait for more data.
		if atEOF {
			return len(data), nil, nil
		}
		return 0, nil, nil
	}

	end := bytes.IndexByte(data[start+1:], FEND)
	if end == -1 {
		// Frame start found but no end; wait for more data.
		if atEOF {
			return len(data), nil, errors.New("incomplete KISS frame")
		}
		return 0, nil, nil
	}

	// Adjust end to be relative to the original data slice.
	end += start + 1

	// Extract the raw frame data (excluding delimiters).
	rawFrame := data[start+1 : end]

	// Process escape sequences in the frame.
	frame := make([]byte, 0, len(rawFrame))
	i := 0
	for i < len(rawFrame) {
		if rawFrame[i] == FESC {
			if i+1 >= len(rawFrame) {
				// Incomplete escape sequence; wait for more data.
				if atEOF {
					return len(data), nil, errors.New("incomplete escape sequence")
				}
				return 0, nil, nil
			}
			switch rawFrame[i+1] {
			case TFEND:
				frame = append(frame, FEND)
			case TFESC:
				frame = append(frame, FESC)
			default:
				return len(data), nil, errors.New("invalid escape sequence")
			}
			i += 2
		} else {
			frame = append(frame, rawFrame[i])
			i++
		}
	}

	// Return the processed frame as the token.
	return end + 1, frame, nil
}
