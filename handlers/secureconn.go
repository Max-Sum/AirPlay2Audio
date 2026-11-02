package handlers

import (
	"bufio"
	"io"
	"net"

	"github.com/brutella/hc/crypto"
	"github.com/brutella/hc/hap"
)

// secureConn replaces the read side of hap.Connection, whose DecryptedRead
// has two faults that drop AirPlay connections:
//
//   - When a caller's read consumes exactly the rest of a decrypted message,
//     the empty buffer is kept, so the next read returns io.EOF and the RTSP
//     server closes the connection. Large bodies such as cover artwork hit
//     this every time.
//   - Each read wraps the socket in a new bufio.Reader, so encrypted bytes
//     read ahead of the current message are lost.
//
// Writes, session handling and Close still use hap.Connection.
type secureConn struct {
	*hap.Connection
	raw     net.Conn
	context hap.Context
	reader  *bufio.Reader // persists across reads, so nothing read ahead is lost
	pending []byte        // decrypted bytes not yet returned
}

func newSecureConn(raw net.Conn, context hap.Context) *secureConn {
	return &secureConn{
		Connection: hap.NewConnection(raw, context),
		raw:        raw,
		context:    context,
		reader:     bufio.NewReaderSize(raw, 16*1024),
	}
}

func (c *secureConn) Read(b []byte) (int, error) {
	if len(c.pending) == 0 {
		decrypter := c.decrypter()
		if decrypter == nil {
			return c.reader.Read(b)
		}
		decrypted, err := decrypter.Decrypt(c.reader)
		if err != nil {
			if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
				_ = c.Connection.Close()
			}
			return 0, err
		}
		data, err := io.ReadAll(decrypted)
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			// Decrypt returns no data only when the socket reached EOF.
			return 0, io.EOF
		}
		c.pending = data
	}
	n := copy(b, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// decrypter is looked up before every message, like hap.Connection does: a
// session switches to its new keys when this is first called after pairing.
func (c *secureConn) decrypter() crypto.Decrypter {
	if session := c.context.GetSessionForConnection(c.raw); session != nil {
		return session.Decrypter()
	}
	return nil
}
