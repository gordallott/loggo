// Copyright 2016 Canonical Ltd.
// Licenced under the LGPLV3, see LICENSE file for details.

package loggo

import (
	"errors"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const rfc5424 = "2006-01-02T15:04:05.000Z07:00"

var utf8bom = []byte{0xef, 0xbb, 0xbf}

// Most of this code imitates the golang syslog implimentation
// but as a lot of that is hard-coded to use specific (poor) formatting
// we re-impliment

// Taken from the go stdlib implimentation of syslog
// modified to not export values
const (
	// Severity.

	// From /usr/include/sys/syslog.h.
	// These are the same on Linux, BSD, and OS X.
	logEmerg int = iota
	logAlert
	logCrit
	logErr
	logWarning
	logNotice
	logInfo
	logDebug
)

const (
	// Facility.

	// From /usr/include/sys/syslog.h.
	// These are the same up to LOG_FTP on Linux, BSD, and OS X.
	logKern int = iota << 3
	logUser
	logMail
	logDaemon
	logAuth
	logSyslog
	logLpr
	logNews
	logUucp
	logCron
	logAuthpriv
	logFtp
	_ // unused
	_ // unused
	_ // unused
	_ // unused
	logLocal0
	logLocal1
	logLocal2
	logLocal3
	logLocal4
	logLocal5
	logLocal6
	logLocal7
)

// a sub-interface of net.Conn, makes mocking simpler in tests
type connwriter interface {
	Close() error
	LocalAddr() net.Addr
	Write(b []byte) (n int, err error)
}

// unixSyslog opens a connection to the syslog daemon running on the
// local machine using a Unix domain socket.
func unixSyslog() (conn connwriter, err error) {
	logTypes := []string{"unixgram", "unix"}
	logPaths := []string{"/dev/log", "/var/run/syslog", "/var/run/log"}
	for _, network := range logTypes {
		for _, path := range logPaths {
			conn, err := net.Dial(network, path)
			if err != nil {
				continue
			} else {
				return conn, nil
			}
		}
	}
	return nil, errors.New("Unix syslog delivery error")
}

type syslogMessage struct {
	level   Level
	message string
}

type syslogWriter struct {
	m sync.Mutex

	isClosed  uint32
	conn      connwriter
	localConn bool
	hostname  string
	network   string
	raddr     string

	bufferedMessagesCond *sync.Cond
	bufferedMessages     []string
}

// NewSyslogWriter returns a writer that will send log messages to a syslog server
// configured with the provied network, raddr strings.
// Passing in "" as the network will default to using default unix sockets
func NewSyslogWriter(network, raddr string) (Writer, error) {
	syslog := syslogWriter{
		network: network,
		raddr:   raddr,
	}
	err := syslog.init()
	return &syslog, err
}

func (syslog *syslogWriter) init() (err error) {
	syslog.bufferedMessagesCond = sync.NewCond(&sync.Mutex{})
	err = syslog.connect()
	if err == nil {
		go syslog.sendloop()
	}

	return
}

func connectNet(network string, raddr string) (conn connwriter, err error) {
	if network == "" {
		return unixSyslog()
	}

	return net.Dial(network, raddr)
}

// connect specified this way so we can subsitute it out in our tests
var connect = connectNet

func (syslog *syslogWriter) connect() (err error) {
	syslog.m.Lock()
	defer syslog.m.Unlock()
	if syslog.conn != nil {
		syslog.conn.Close()
		syslog.conn = nil
	}

	if syslog.network == "" {
		if syslog.hostname == "" {
			syslog.hostname = "localhost"
		}
		syslog.localConn = true
	}

	conn, err := connect(syslog.network, syslog.raddr)

	if err == nil {
		syslog.conn = conn
		if syslog.hostname == "" {
			syslog.hostname = conn.LocalAddr().String()
		}
	}
	return
}

func (syslog *syslogWriter) sendloop() {
	// intended to be a long running goroutine
	// uses a slice buffer and sync.Cond to syncronise instead of a channel
	// because a channel has a limited capacity which may cause log messages
	// to block at some point (if network goes down, there is likely to be a spike
	// in log messages that can not be sent)

	var backoffCounter int32

	for {
		ok := syslog.sendBufferedMessages()
		if ok == false {
			// network connection problem, backoff for a while to stop any hammering
			if backoffCounter < 7 {
				backoffCounter++
			}

			<-time.After((time.Millisecond * 50) * time.Duration(rand.Int31n(backoffCounter)))
		}

		syslog.bufferedMessagesCond.L.Lock()
		if len(syslog.bufferedMessages) > 0 {
			syslog.bufferedMessagesCond.L.Unlock()
			continue
		}

		for len(syslog.bufferedMessages) < 1 {
			if atomic.LoadUint32(&syslog.isClosed) == 1 {
				// connection closed, we need to exit
				return
			}

			syslog.bufferedMessagesCond.Wait()
		}
		syslog.bufferedMessagesCond.L.Unlock()
	}
}

func (syslog *syslogWriter) sendBufferedMessages() (ok bool) {
	syslog.bufferedMessagesCond.L.Lock()
	defer syslog.bufferedMessagesCond.L.Unlock()
	ok = true

	if len(syslog.bufferedMessages) < 1 {
		return
	}

	var index int
	for index, message := range syslog.bufferedMessages {
		err := syslog.sendMessage(message)
		if err != nil {
			ok = false
			defer syslog.connect()

			index = index - 1
			if index < 0 {
				index = 0
			}
			break
		}
	}

	if ok == false {
		syslog.bufferedMessages = syslog.bufferedMessages[index:]
	} else {
		syslog.bufferedMessages = nil
	}

	return
}

func (syslog *syslogWriter) sendMessage(message string) (err error) {
	_, err = syslog.conn.Write([]byte(message))
	return
}

func (syslog *syslogWriter) Write(level Level, module, filename string, line int, timestamp time.Time, message string) {
	if atomic.LoadUint32(&syslog.isClosed) == 1 {
		return
	}

	var priority int

	switch level {
	case TRACE: // trace does not get sent to syslog
		return
	case DEBUG:
		priority = logDebug
	case INFO:
		priority = logInfo
	case WARNING:
		priority = logWarning
	case ERROR:
		priority = logErr
	case CRITICAL:
		priority = logCrit
	}

	var hostname string
	if syslog.localConn {
		hostname = "-"
	} else {
		hostname = syslog.hostname
	}

	header := fmt.Sprintf("<%d>1 %s %s %s %d - -", priority, timestamp.Format(rfc5424), hostname, module, os.Getpid())
	msg := fmt.Sprintf("%s %s%s:%d %s\n", header, utf8bom, filename, line, message)

	syslog.bufferedMessagesCond.L.Lock()
	syslog.bufferedMessages = append(syslog.bufferedMessages, msg)
	syslog.bufferedMessagesCond.Broadcast()
	syslog.bufferedMessagesCond.L.Unlock()
}

func (syslog *syslogWriter) Close() error {
	syslog.m.Lock()
	defer syslog.m.Unlock()

	if syslog.conn != nil {
		err := syslog.conn.Close()
		syslog.conn = nil
		syslog.isClosed = 1
		syslog.bufferedMessagesCond.L.Lock()
		syslog.bufferedMessages = nil
		syslog.bufferedMessagesCond.Broadcast()
		syslog.bufferedMessagesCond.L.Unlock()
		return err
	}

	return nil
}
