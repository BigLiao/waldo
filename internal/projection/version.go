package projection

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"waldo/internal/fileversion"
)

type versionReader interface {
	io.ReaderAt
	Stat() (os.FileInfo, error)
	Close() error
}
type virtualReader struct {
	*bytes.Reader
	info *file
}

func (r *virtualReader) Stat() (os.FileInfo, error) { return r.info, nil }
func (*virtualReader) Close() error                 { return nil }

// Serve adds two read-only extensions around the unchanged OS/virtual SFTP
// servers. OPEN+descriptor-stat is one round trip and never reads file data.
// The small read-only handle table is scoped to this SFTP connection.
func (v *View) Serve(st io.ReadWriteCloser) error {
	defer st.Close()
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() { defer server.Close(); v.serveNative(server) }()
	var output sync.Mutex
	send := func(p []byte) error { output.Lock(); defer output.Unlock(); return writePacket(st, p) }
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer st.Close()
		for {
			p, err := readPacket(client)
			if err != nil {
				return
			}
			if send(p) != nil {
				return
			}
		}
	}()
	defer func() { client.Close(); server.Close(); st.Close(); <-done }()
	files := make(map[string]versionReader)
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	var serial uint64
	for {
		p, err := readPacket(st)
		if err != nil {
			return err
		}
		if len(p) < 5 && p[0] != 1 {
			return io.ErrUnexpectedEOF
		}
		if p[0] == 200 {
			ext, rest, err := packetString(p[5:])
			if err != nil {
				return err
			}
			if ext == fileversion.Open {
				path, tail, err := packetString(rest)
				if err != nil || len(tail) != 0 {
					return io.ErrUnexpectedEOF
				}
				id := binary.BigEndian.Uint32(p[1:5])
				var f versionReader
				if len(files) >= 1024 {
					err = fmt.Errorf("too many read-only files")
				} else {
					f, err = v.openVersioned(path)
				}
				var info os.FileInfo
				if err == nil {
					info, err = f.Stat()
					if err == nil && !info.Mode().IsRegular() {
						err = syscall.ENOTSUP
					}
				}
				if err != nil {
					if f != nil {
						f.Close()
					}
					if err = send(versionStatus(id, err)); err != nil {
						return err
					}
					continue
				}
				serial++
				key := fileversion.HandlePrefix + strconv.FormatUint(serial, 10)
				files[key] = f
				body := replaceString(nil, key, nil)
				body = replaceString(body, fileversion.FromStat(info).Encode(), nil)
				if err = send(versionReply(fileversion.Reply, id, body)); err != nil {
					return err
				}
				continue
			}
			if ext == fileversion.Close {
				key, tail, err := packetString(rest)
				if err != nil || len(tail) != 0 {
					return io.ErrUnexpectedEOF
				}
				id := binary.BigEndian.Uint32(p[1:5])
				f := files[key]
				if f == nil {
					if err = send(versionStatus(id, os.ErrInvalid)); err != nil {
						return err
					}
					continue
				}
				delete(files, key)
				info, err := f.Stat()
				closeErr := f.Close()
				if err == nil {
					err = closeErr
				}
				response := versionStatus(id, err)
				if err == nil {
					response = versionReply(fileversion.Reply, id, replaceString(nil, fileversion.FromStat(info).Encode(), nil))
				}
				if err = send(response); err != nil {
					return err
				}
				continue
			}
		}
		if p[0] == 4 || p[0] == 5 || p[0] == 6 || p[0] == 8 || p[0] == 10 || p[0] == 12 {
			key, rest, err := packetString(p[5:])
			if err != nil {
				return err
			}
			if strings.HasPrefix(key, fileversion.HandlePrefix) {
				id := binary.BigEndian.Uint32(p[1:5])
				f := files[key]
				response := versionStatus(id, os.ErrInvalid)
				if f != nil {
					switch p[0] {
					case 4:
						delete(files, key)
						response = versionStatus(id, f.Close())
					case 8:
						info, err := f.Stat()
						response = versionStatus(id, err)
						if err == nil {
							response = versionReply(105, id, fileversion.FromStat(info).Attributes())
						}
					case 5:
						if len(rest) != 12 {
							return io.ErrUnexpectedEOF
						}
						offset := binary.BigEndian.Uint64(rest)
						length := min(binary.BigEndian.Uint32(rest[8:]), 32<<10)
						if offset > 1<<63-1 {
							response = versionStatus(id, os.ErrInvalid)
						} else {
							data := make([]byte, int(length))
							n, err := f.ReadAt(data, int64(offset))
							response = versionStatus(id, err)
							if n > 0 || err == nil {
								response = versionReply(103, id, replaceString(nil, string(data[:n]), nil))
							}
						}
					default:
						response = versionStatus(id, os.ErrPermission)
					}
				}
				if err = send(response); err != nil {
					return err
				}
				continue
			}
		}
		if err = writePacket(client, p); err != nil {
			return err
		}
	}
}

func (v *View) openVersioned(path string) (versionReader, error) {
	if v != nil {
		if f := v.virtual(path); f != nil {
			return &virtualReader{bytes.NewReader(f.data), f}, nil
		}
	}
	// Do not turn an untrusted named pipe/device into a blocked OPEN in this
	// router. Unsupported file types fall back to the existing SFTP server.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func versionReply(kind byte, id uint32, body []byte) []byte {
	return append(binary.BigEndian.AppendUint32([]byte{kind}, id), body...)
}
func versionStatus(id uint32, err error) []byte {
	code := uint32(4)
	switch {
	case err == nil:
		code = 0
	case errors.Is(err, io.EOF):
		code = 1
	case errors.Is(err, os.ErrNotExist):
		code = 2
	case errors.Is(err, os.ErrPermission):
		code = 3
	case errors.Is(err, syscall.ENOTSUP):
		code = 8
	}
	body := binary.BigEndian.AppendUint32(nil, code)
	body = replaceString(body, "", nil)
	body = replaceString(body, "", nil)
	return versionReply(101, id, body)
}
