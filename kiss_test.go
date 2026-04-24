package kiss

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestFrameEncodeDoesNotPrependZeros(t *testing.T) {
	frame := FrameEncode(0x10, []byte("hello"))

	if len(frame) < 3 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}
	if frame[0] != FEND {
		t.Fatalf("frame starts with %#x, want FEND", frame[0])
	}
	if frame[1] != 0x10 {
		t.Fatalf("port command is %#x, want 0x10", frame[1])
	}
	if !bytes.Equal(frame[2:7], []byte("hello")) {
		t.Fatalf("frame payload prefix is %q, want hello", frame[2:7])
	}
	if frame[len(frame)-1] != FEND {
		t.Fatalf("frame ends with %#x, want FEND", frame[len(frame)-1])
	}
}

func TestFrameEncodeEscapesFrameBytes(t *testing.T) {
	frame := FrameEncode(0x00, []byte{FEND, FESC})
	want := []byte{FEND, 0x00, FESC, TFEND, FESC, TFESC}

	if !bytes.Equal(frame[:len(want)], want) {
		t.Fatalf("escaped frame prefix = %#v, want %#v", frame[:len(want)], want)
	}
}

func TestSplitReadsEscapedFrame(t *testing.T) {
	payload := []byte("0123456789ABCDE")
	payload[3] = FEND
	payload[9] = FESC

	scanner := bufio.NewScanner(bytes.NewReader(FrameEncode(0x20, payload)))
	scanner.Split(Split)

	if !scanner.Scan() {
		t.Fatalf("scanner did not return a frame: %v", scanner.Err())
	}
	want := append([]byte{0x20}, payload...)
	if !bytes.Equal(scanner.Bytes(), want) {
		t.Fatalf("decoded frame = %#v, want %#v", scanner.Bytes(), want)
	}
	if scanner.Scan() {
		t.Fatalf("scanner returned unexpected second frame: %#v", scanner.Bytes())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}
}

func TestPortWriteReportsShortWrite(t *testing.T) {
	p := port{id: 1, rw: shortWriter{}}

	n, err := p.Write([]byte("hello"))
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want io.ErrShortWrite", err)
	}
	if n != 0 {
		t.Fatalf("n = %d, want 0", n)
	}
}

func TestSetHardwareFrameRoundTrip(t *testing.T) {
	left, right := net.Pipe()
	t.Cleanup(func() { _ = left.Close() })
	t.Cleanup(func() { _ = right.Close() })

	sender := NewTNC(left)
	receiver := NewTNC(right)

	payload := []byte("TNC:KISS")
	cmdPayload := WithCommand(FrameTypeSetHardware, payload)

	writeErr := make(chan error, 1)
	go func() {
		n, err := sender.CommandPort(3).Write(cmdPayload)
		if err != nil {
			writeErr <- err
			return
		}
		if n != len(cmdPayload) {
			writeErr <- errors.New("short logical write")
			return
		}
		writeErr <- nil
	}()

	readResult := make(chan []byte, 1)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := receiver.CommandPort(3).Read(buf)
		if err != nil {
			readErr <- err
			return
		}
		readResult <- append([]byte(nil), buf[:n]...)
	}()

	select {
	case err := <-writeErr:
		if err != nil {
			t.Fatalf("write failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out writing SetHardware frame")
	}

	select {
	case err := <-readErr:
		t.Fatalf("read failed: %v", err)
	case got := <-readResult:
		if len(got) < 1+len(cmdPayload) {
			t.Fatalf("decoded command frame too short: got %d bytes", len(got))
		}
		if got[0] != 0x36 {
			t.Fatalf("decoded header = %#x, want %#x", got[0], byte(0x30|FrameTypeSetHardware))
		}
		if !bytes.Equal(got[1:1+len(cmdPayload)], cmdPayload) {
			t.Fatalf("decoded payload = %#v, want %#v", got[1:1+len(cmdPayload)], cmdPayload)
		}
		for i, b := range got[1+len(cmdPayload):] {
			if b != 0 {
				t.Fatalf("padding byte %d = %#x, want 0", i, b)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out reading SetHardware frame")
	}
}

type shortWriter struct{}

func (shortWriter) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (shortWriter) Write(data []byte) (int, error) {
	return len(data) - 1, nil
}
