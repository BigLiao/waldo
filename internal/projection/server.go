package projection

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/pkg/sftp"
)

// Serve routes only virtual files and project-root directory listings to a
// read-only RequestServer. All other packets go unchanged to the original OS
// server, preserving its creation permissions, handle semantics and extensions.
// Both protocol parsers remain inside the existing A sandbox.
func (v *View) Serve(st io.ReadWriteCloser) error {
	defer st.Close()
	if v == nil || len(v.files) == 0 {
		s, err := sftp.NewServer(st)
		if err != nil {
			return err
		}
		defer s.Close()
		return s.Serve()
	}
	realClient, realServer := net.Pipe()
	viewClient, viewServer := net.Pipe()
	backends := []net.Conn{realClient, viewClient}
	defer realClient.Close()
	defer viewClient.Close()
	defer realServer.Close()
	defer viewServer.Close()
	real, err := sftp.NewServer(realServer)
	if err != nil {
		return err
	}
	virtual := sftp.NewRequestServer(viewServer, sftp.Handlers{
		FileGet: v, FilePut: v, FileCmd: v, FileList: v,
	}, sftp.WithStartDirectory(v.project))
	go func() { defer real.Close(); real.Serve() }()
	go func() { defer virtual.Close(); virtual.Serve() }()

	var output sync.Mutex
	var readers sync.WaitGroup
	// Closing the client ends unblocks either the protocol reader or writer.
	defer func() {
		realClient.Close()
		viewClient.Close()
		st.Close()
		readers.Wait()
	}()
	for i, c := range backends {
		readers.Add(1)
		go func(i int, c net.Conn) {
			defer readers.Done()
			for {
				p, err := readPacket(c)
				if err != nil {
					st.Close()
					return
				}
				if p[0] == 2 && i == 1 {
					continue
				} // one VERSION reply to the client
				if p[0] == 102 { // HANDLE: namespace the two servers' handle values
					if len(p) < 5 {
						st.Close()
						return
					}
					h, rest, err := packetString(p[5:])
					if err != nil {
						st.Close()
						return
					}
					p = replaceString(p[:5], string([]byte{byte('0' + i), ':'})+h, rest)
				}
				output.Lock()
				err = writePacket(st, p)
				output.Unlock()
				if err != nil {
					st.Close()
					return
				}
			}
		}(i, c)
	}
	for {
		p, err := readPacket(st)
		if err != nil {
			return err
		}
		if p[0] == 1 { // INIT both backends before forwarding later requests
			if err := writePacket(viewClient, p); err != nil {
				return err
			}
			if err := writePacket(realClient, p); err != nil {
				return err
			}
			continue
		}
		dst, p, err := v.route(p)
		if err != nil {
			return err
		}
		if err := writePacket(backends[dst], p); err != nil {
			return err
		}
	}
}

func (v *View) route(p []byte) (int, []byte, error) {
	if len(p) < 5 {
		return 0, nil, io.ErrUnexpectedEOF
	}
	switch p[0] {
	case 4, 5, 6, 8, 10, 12: // CLOSE, READ, WRITE, FSTAT, FSETSTAT, READDIR
		h, rest, err := packetString(p[5:])
		if err != nil {
			return 0, nil, err
		}
		if len(h) >= 2 && h[1] == ':' && (h[0] == '0' || h[0] == '1') {
			return int(h[0] - '0'), replaceString(p[:5], h[2:], rest), nil
		}
		return 0, p, nil // the OS server returns its normal invalid-handle error
	}
	pathData := p[5:]
	twoPaths := false
	switch p[0] {
	case 3, 7, 9, 11, 13, 14, 15, 16, 17, 19: // single-path operations
	case 18, 20: // RENAME, SYMLINK
		twoPaths = true
	case 200: // extensions supported by the existing server
		ext, rest, err := packetString(pathData)
		if err != nil {
			return 0, nil, err
		}
		switch ext {
		case "posix-rename@openssh.com", "hardlink@openssh.com":
			twoPaths = true
		case "statvfs@openssh.com":
		default:
			return 0, p, nil
		}
		pathData = rest
	default:
		return 0, p, nil
	}
	path, rest, err := packetString(pathData)
	if err != nil {
		return 0, nil, err
	}
	if v.virtual(path) != nil || (p[0] == 11 && v.root(path)) {
		return 1, p, nil
	}
	if twoPaths {
		target, _, err := packetString(rest)
		if err != nil {
			return 0, nil, err
		}
		if v.virtual(target) != nil {
			return 1, p, nil
		}
	}
	return 0, p, nil
}

const maxPacket = 16 << 20

func readPacket(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n == 0 || n > maxPacket {
		return nil, fmt.Errorf("invalid SFTP packet length %d", n)
	}
	p := make([]byte, n)
	_, err := io.ReadFull(r, p)
	return p, err
}

func writePacket(w io.Writer, p []byte) error {
	b := binary.BigEndian.AppendUint32(nil, uint32(len(p)))
	b = append(b, p...)
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func packetString(p []byte) (string, []byte, error) {
	if len(p) < 4 {
		return "", nil, io.ErrUnexpectedEOF
	}
	n := binary.BigEndian.Uint32(p[:4])
	if uint64(n) > uint64(len(p)-4) {
		return "", nil, io.ErrUnexpectedEOF
	}
	return string(p[4 : 4+int(n)]), p[4+int(n):], nil
}

func replaceString(prefix []byte, s string, rest []byte) []byte {
	p := append([]byte{}, prefix...)
	p = binary.BigEndian.AppendUint32(p, uint32(len(s)))
	p = append(p, s...)
	return append(p, rest...)
}
