// Package fileversion defines Waldo's read-only SFTP validation extensions.
// All path access is performed by the existing A sandbox, not its trusted host.
package fileversion

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
)

const Open = "open-version@waldo"
const Close = "close-version@waldo"
const Reply byte = 201
const HandlePrefix = "waldo-read:"

// Version is a freshness hint, not a content hash. A writer that preserves the
// timestamp and size can escape detection. Identity distinguishes atomic swaps.
type Version struct {
	ModifiedNS int64  `json:"mtime_ns"`
	Size       int64  `json:"size"`
	Device     uint64 `json:"dev"`
	Inode      uint64 `json:"ino"`
}

type Info struct {
	Version
	Mode uint32 `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
}

func FromStat(st os.FileInfo) Info {
	i := Info{Version: Version{ModifiedNS: st.ModTime().UnixNano(), Size: st.Size()}, Mode: uint32(st.Mode().Perm()) | 0100000, UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	if s, ok := st.Sys().(*syscall.Stat_t); ok {
		i.Device, i.Inode, i.Mode, i.UID, i.GID = uint64(s.Dev), uint64(s.Ino), s.Mode, s.Uid, s.Gid
	}
	return i
}

func (i Info) Regular() bool  { return i.Mode&0170000 == 0100000 }
func (i Info) Encode() string { b, _ := json.Marshal(i); return string(b) }
func Decode(s string) (Info, error) {
	var i Info
	if len(s) > 4096 || json.Unmarshal([]byte(s), &i) != nil || i.Size < 0 {
		return i, fmt.Errorf("invalid file version")
	}
	return i, nil
}

// Attributes is an ordinary SFTP v3 ATTRS body. Only the private version carries
// nanoseconds; no modification of SSHFS or the agent is required.
func (i Info) Attributes() []byte {
	b := binary.BigEndian.AppendUint32(nil, 15)
	b = binary.BigEndian.AppendUint64(b, uint64(i.Size))
	b = binary.BigEndian.AppendUint32(b, i.UID)
	b = binary.BigEndian.AppendUint32(b, i.GID)
	b = binary.BigEndian.AppendUint32(b, i.Mode)
	b = binary.BigEndian.AppendUint32(b, uint32(i.ModifiedNS/1e9))
	return binary.BigEndian.AppendUint32(b, uint32(i.ModifiedNS/1e9))
}
