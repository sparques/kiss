# kiss

`kiss` is a small Go package for working with KISS-framed packet radio data.
It wraps a shared `io.ReadWriter`, splits inbound frames, escapes outbound
frames, and exposes up to eight logical KISS ports through simple `Read` and
`Write` methods.

## Features

- Encodes outbound KISS frames with the required `FEND` and `FESC` escaping.
- Decodes inbound frames with a `bufio.Scanner` split function.
- Routes incoming frames to per-port queues.
- Separates data traffic from command traffic with `Port` and `CommandPort`.
- Keeps bounded receive queues and drops the oldest queued frame when a queue
  fills.

## Install

```bash
go get github.com/sparques/kiss
```

## Usage

```go
package main

import (
	"log"
	"net"

	"github.com/sparques/kiss"
)

func main() {
	conn, err := net.Dial("tcp", "127.0.0.1:8001")
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	tnc := kiss.NewTNC(conn)

	port0 := tnc.Port(0)
	if _, err := port0.Write([]byte("hello")); err != nil {
		log.Fatal(err)
	}

	buf := make([]byte, 512)
	n, err := port0.Read(buf)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("received %d bytes", n)
}
```

Use `CommandPort` when you need to exchange non-data KISS frames:

```go
cmd := tnc.CommandPort(0)
_, err := cmd.Write([]byte{byte(kiss.FrameTypeTXDelay), 50})
```

The first byte passed to `CommandPort.Write` becomes the command nibble in the
outbound frame header.

## API Notes

- `NewTNC` starts a background router goroutine immediately.
- `Port(n)` and `CommandPort(n)` clamp any value greater than `7` to port `7`.
- `port.Read` and `commandPort.Read` return the full queued frame length even
  if the destination buffer is smaller than the frame contents.
- `FrameEncode` pads short payloads with zero bytes to reach the minimum frame
  size used by this package.
- When the data receive queue for a port is full, the oldest queued frame is
  discarded before the new one is enqueued.

## Testing

```bash
go test ./...
```
