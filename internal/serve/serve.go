// Package serve implements `waldo sandbox-serve`, the process inside the A
// sandbox that answers every file and exec request coming from B.
//
// It is on the untrusted side of the boundary: whatever it does is confined
// by the sandbox mounts and network namespace, not by checks in this code.
// Its only channel is stdin/stdout to the host waldo process.
package serve

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"github.com/hashicorp/yamux"

	"waldo/internal/projection"
	"waldo/internal/proto"
	"waldo/internal/util"
)

type server struct {
	sess *yamux.Session
	cfg  proto.SandboxConfig
	own  map[int]bool // ports this process listens on itself
	mu   sync.Mutex
	view *projection.View
}

// Main runs sandbox-serve on stdin/stdout.
func Main() int {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	log.SetPrefix("sandbox-serve: ")
	rwc := util.RWC{R: os.Stdin, W: os.Stdout, C: []io.Closer{os.Stdin, os.Stdout}}
	sess, err := yamux.Server(rwc, util.YamuxConfig())
	if err != nil {
		log.Print(err)
		return 1
	}
	s := &server{sess: sess, own: map[int]bool{}}
	ctl, err := sess.AcceptStream()
	if err != nil {
		log.Print(err)
		return 1
	}
	var h proto.Header
	if err := proto.ReadMsg(ctl, &h); err != nil || h.Kind != proto.KindSandbox {
		log.Printf("bad first stream: %v %q", err, h.Kind)
		return 1
	}
	if err := proto.ReadMsg(ctl, &s.cfg); err != nil {
		log.Print(err)
		return 1
	}
	if s.cfg.ContextProject != "" {
		s.view, err = projection.New(s.cfg.ContextProject, s.cfg.InstructionFiles)
		if err != nil {
			log.Printf("session instructions: %v", err)
			return 1
		}
	}
	for _, d := range s.cfg.Mkdirs {
		os.MkdirAll(d, 0o700)
	}
	if err := s.startProxy(); err != nil {
		log.Printf("proxy: %v", err)
	}
	s.startHostPorts()
	if err := proto.WriteMsg(ctl, s.facts()); err != nil {
		log.Print(err)
		return 1
	}
	go s.scanPorts(ctl)
	for {
		st, err := sess.AcceptStream()
		if err != nil {
			return 0
		}
		go s.handle(st)
	}
}

func (s *server) handle(st *yamux.Stream) {
	var h proto.Header
	if err := proto.ReadMsg(st, &h); err != nil {
		st.Close()
		return
	}
	switch h.Kind {
	case proto.KindFS:
		s.view.Serve(st)
	case proto.KindExec:
		if h.Exec == nil {
			st.Close()
			return
		}
		runExec(st, h.Exec, s.cfg.ExecEnv)
	case proto.KindDial:
		s.dial(st, h.Port)
	default:
		st.Close()
	}
}

func (s *server) dial(st *yamux.Stream, port int) {
	var c net.Conn
	var err error
	for _, addr := range []string{"127.0.0.1", "::1"} {
		c, err = net.Dial("tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
		if err == nil {
			break
		}
	}
	if err != nil {
		st.Close()
		return
	}
	util.Splice(st, c)
}

func (s *server) facts() proto.Facts {
	env := append([]string(nil), s.cfg.BaseEnv...)
	home, _ := util.EnvGet(env, "HOME")
	path, _ := util.EnvGet(env, "PATH")
	var dirs []proto.PathDir
	var visible []string
	seen := map[string]bool{}
	total := 0
	add := func(d string, onPath bool) {
		if d == "" || !filepath.IsAbs(d) || seen[d] {
			return
		}
		if !util.IsDir(d) {
			return
		}
		seen[d] = true
		if onPath {
			visible = append(visible, d)
		}
		real, err := filepath.EvalSymlinks(d)
		if err != nil {
			real = d
		}
		ents, _ := os.ReadDir(d)
		var names []string
		for _, e := range ents {
			n := e.Name()
			if strings.ContainsAny(n, "/\x00") || n == "." || n == ".." || total > 100000 {
				continue
			}
			names = append(names, n)
			total++
		}
		dirs = append(dirs, proto.PathDir{Dir: d, Real: real, Names: names, OnPath: onPath})
	}
	for _, d := range filepath.SplitList(path) {
		add(d, true)
	}
	// Well-known directories so that hard-coded /bin/sh, /usr/bin/env work
	// even when they are not on PATH.
	for _, d := range []string{"/bin", "/usr/bin", "/sbin", "/usr/sbin", "/usr/local/bin"} {
		add(d, false)
	}
	env = util.EnvSet(env, "PATH", strings.Join(visible, ":"))
	if sh, ok := util.EnvGet(env, "SHELL"); !ok || !util.Exists(sh) {
		env = util.EnvSet(env, "SHELL", "/bin/sh")
	}
	hn, _ := os.Hostname()
	return proto.Facts{Home: home, Hostname: hn, OS: runtime.GOOS, Arch: runtime.GOARCH, UID: os.Getuid(), GID: os.Getgid(), Env: env, PathDirs: dirs}
}

func (s *server) markOwn(port int) {
	s.mu.Lock()
	s.own[port] = true
	s.mu.Unlock()
}

func (s *server) isOwn(port int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.own[port]
}

// startHostPorts makes the configured host services reachable inside the
// sandbox at 127.0.0.1:<port>. The host decides whether to connect.
func (s *server) startHostPorts() {
	for _, p := range s.cfg.HostPorts {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			log.Printf("host port %d: %v", p, err)
			continue
		}
		s.markOwn(p)
		go func(port int, ln net.Listener) {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				go func() {
					st, err := s.open(proto.Header{Kind: proto.KindHostPort, Port: port})
					if err != nil {
						c.Close()
						return
					}
					util.Splice(st, c)
				}()
			}
		}(p, ln)
	}
}

// open opens a stream to the host and waits for its Reply.
func (s *server) open(h proto.Header) (*yamux.Stream, error) {
	st, err := s.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	if err := proto.WriteMsg(st, h); err != nil {
		st.Close()
		return nil, err
	}
	var r proto.Reply
	if err := proto.ReadMsg(st, &r); err != nil {
		st.Close()
		return nil, err
	}
	if !r.OK {
		st.Close()
		return nil, &deniedError{r.Msg}
	}
	return st, nil
}

type deniedError struct{ msg string }

func (e *deniedError) Error() string { return e.msg }
