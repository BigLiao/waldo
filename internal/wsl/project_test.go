package wsl

import (
	"strings"
	"testing"
)

func TestWindowsProjectMount(t *testing.T) {
	const mounts = `20 10 8:1 / / rw - ext4 /dev/sda rw
21 20 0:7 / /mnt/c rw - 9p C:\134 rw,aname=drvfs;path=C:\134;uid=1000
22 20 0:8 /work /home/me/Drive\040Alias rw - 9p C:\134 rw,aname=drvfs;path=C:\134;uid=1000
23 20 0:9 / /home/me/virtio rw - virtiofs tag rw
24 20 0:10 / /mnt/custom rw - 9p shared rw,aname=linux
25 21 8:2 / /mnt/c/native rw - ext4 /dev/sdb rw
`
	for _, tc := range []struct {
		project string
		want    string
	}{
		{"/home/me/project", ""},
		{"/mnt/copy", ""},
		{"/mnt/custom/project", ""},
		{"/mnt/c", "/mnt/c"},
		{"/mnt/c/project", "/mnt/c"},
		{"/mnt/c/native/project", ""},
		{"/home/me/Drive Alias/project", "/home/me/Drive Alias"},
		{"/home/me/virtio/project", "/home/me/virtio"},
		{"/home/me", "/home/me/virtio"},
	} {
		got, err := windowsProjectMount(strings.NewReader(mounts), tc.project)
		if err != nil || got != tc.want {
			t.Errorf("project %q: Windows mount %q, want %q (%v)", tc.project, got, tc.want, err)
		}
	}
}
