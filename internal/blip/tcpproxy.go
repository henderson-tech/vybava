package blip

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// serveTCP is the raw-byte mode: one goroutine per accepted connection.
func (s *Server) serveTCP(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		go s.handleTCP(c)
	}
}

func (s *Server) handleTCP(client net.Conn) {
	start := time.Now()
	id := s.connections.Add(1)
	s.requests.Add(1)
	s.inFlight.Add(1)
	s.track(client)
	defer func() {
		s.untrack(client)
		_ = client.Close()
		s.inFlight.Add(-1)
	}()

	applied := ""
	fs, hold := s.snapshot(start)
	if fs != nil && fs.decide("", "", start) {
		applied = fs.Kind
		s.faulted.Add(1)
	}
	log := func(in, out int64) {
		fault := applied
		if fault == "" {
			fault = "-"
		}
		s.logf("conn#%d %s fault=%s in=%d out=%d dur=%s", id, client.RemoteAddr(), fault, in, out, time.Since(start).Round(time.Millisecond))
	}

	switch applied {
	case "drop", "flap":
		if tc, ok := client.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		log(0, 0)
		return
	case "timeout":
		done := make(chan struct{})
		go func() { _, _ = io.Copy(io.Discard, client); close(done) }()
		select {
		case <-hold:
		case <-done:
		}
		log(0, 0)
		return
	}

	upstream, err := net.DialTimeout("tcp", s.cfg.Upstream, 5*time.Second)
	if err != nil {
		s.logf("conn#%d dial %s: %v", id, s.cfg.Upstream, err)
		return
	}
	s.track(upstream)
	defer s.untrack(upstream)
	defer upstream.Close()

	var in, out int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); in = s.pipe(upstream, client, fs, applied); _ = closeWrite(upstream) }()
	go func() { defer wg.Done(); out = s.pipe(client, upstream, fs, applied); _ = closeWrite(client) }()
	wg.Wait()
	log(in, out)
}

// pipe copies src → dst applying `delay` (per chunk) or `slow` (throttle).
func (s *Server) pipe(dst io.Writer, src io.Reader, fs *faultState, applied string) int64 {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			switch applied {
			case "delay":
				time.Sleep(fs.sleep())
			case "slow":
				time.Sleep(time.Duration(float64(n) / float64(fs.Bytes) * float64(time.Second)))
			}
			m, werr := dst.Write(buf[:n])
			total += int64(m)
			if werr != nil {
				return total
			}
		}
		if err != nil {
			return total
		}
	}
}

func closeWrite(c net.Conn) error {
	if tc, ok := c.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}
	return nil
}
