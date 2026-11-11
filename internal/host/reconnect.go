package host

import (
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"waldo/internal/proto"
	"waldo/internal/reconnect"
	"waldo/internal/util"
)

// The socket is only reachable through SSH as the same user. A private,
// owner-checked directory prevents another local user from replacing it.
func reconnectSocket(token string) (string, error) {
	b, err := hex.DecodeString(token)
	if err != nil || len(b) != 32 {
		return "", fmt.Errorf("invalid reconnect token")
	}
	// Keep this path short enough for Linux's Unix-socket limit, and stable
	// across SSH invocations even when TMPDIR changes.
	dir := filepath.Join("/tmp", fmt.Sprintf("waldo-reconnect-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !os.IsExist(err) {
		return "", err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !st.IsDir() || st.Mode().Perm() != 0o700 || !ok || owner.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("unsafe reconnect socket directory: %s", dir)
	}
	return filepath.Join(dir, token+".sock"), nil
}

// This short-lived SSH child proxies bytes to the independent session owner.
// A retry is forbidden from starting a replacement owner/agent.
func attachReconnect(dataDir, token string, window time.Duration, start bool) int {
	path, err := reconnectSocket(token)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil && start {
		exe, e := os.Executable()
		if e != nil {
			return 1
		}
		if e := os.MkdirAll(filepath.Join(dataDir, "log"), 0o700); e != nil {
			return 1
		}
		lf, e := os.OpenFile(filepath.Join(dataDir, "log", "host.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if e != nil {
			return 1
		}
		cmd := exec.Command(exe, "host", "--data-dir", dataDir, "--reconnect-serve", token, "--reconnect-window", window.String())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		cmd.Stdout, cmd.Stderr = lf, lf
		e = cmd.Start()
		lf.Close()
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			return 1
		}
		go cmd.Wait()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			c, err = net.DialTimeout("unix", path, 100*time.Millisecond)
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "waldo: 原会话不可用:", err)
		return 1
	}
	defer c.Close()
	if _, err := fmt.Fprintln(os.Stdout, proto.BannerPrefix+proto.FullVersion()); err != nil {
		return 1
	}
	go func() { io.Copy(c, os.Stdin); c.Close() }()
	io.Copy(os.Stdout, c)
	return 0
}

func serveReconnect(dataDir, token string, window time.Duration) int {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetPrefix(fmt.Sprintf("[%d] ", os.Getpid()))
	path, err := reconnectSocket(token)
	if err != nil {
		return 1
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		log.Printf("reconnect listen: %v", err)
		return 1
	}
	defer ln.Close()
	defer os.Remove(path)
	link := reconnect.New(window)
	link.State = func(up bool) { log.Printf("reconnect attached=%t", up) }
	defer link.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go link.Serve(c)
		}
	}()
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	defer signal.Stop(sigs)
	go func() {
		select {
		case <-sigs:
			link.Close()
		case <-link.Done():
		}
	}()
	h := &host{dataDir: dataDir}
	code, err := h.runOn(link, util.ResumableYamuxConfig())
	h.cleanup()
	link.Drain()
	if h.sess != nil {
		h.sess.Close()
	}
	if err != nil {
		log.Printf("resumable session: %v", err)
	}
	return code
}
