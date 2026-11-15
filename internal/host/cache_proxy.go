package host

import (
	"io"
	"os"

	"waldo/internal/util"
)

func cacheProcessPipe() (stdin, stdout *os.File, proxy io.ReadWriteCloser, err error) {
	stdin, toChild, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	fromChild, stdout, err := os.Pipe()
	if err != nil {
		stdin.Close()
		toChild.Close()
		return nil, nil, nil, err
	}
	proxy = util.RWC{R: fromChild, W: toChild, C: []io.Closer{fromChild, toChild}}
	return stdin, stdout, proxy, nil
}
