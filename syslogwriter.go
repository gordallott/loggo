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
	"unicode/utf8"
)

const rfc5424 = "2006-01-02T15:04:05.000Z07:00"

var utf8bom = []byte{0xef, 0xbb, 0xbf}

// Taken from the go stdlib implimentation of syslog
// Modified to not export
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

// Facility.
// From /usr/include/sys/syslog.h.
// These are the same up to LOG_FTP on Linux, BSD, and OS X.
const (
	FacilityKern int = iota << 3
	FacilityUser
	FacilityMail
	FacilityDaemon
	FacilityAuth
	FacilitySyslog
	FacilityLpr
	FacilityNews
	FacilityUucp
	FacilityCron
	FacilityAuthpriv
	FacilityFtp
	_ // unused
	_ // unused
	_ // unused
	_ // unused
	FacilityLocal0
	FacilityLocal1
	FacilityLocal2
	FacilityLocal3
	FacilityLocal4
	FacilityLocal5
	FacilityLocal6
	FacilityLocal7
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

// SyslogWriter is a struct that impliments Writer and has functions for dealing with sending logs
// to local or remote syslogs
type SyslogWriter struct {
	isClosed uint32 // keep this as the first entry to ensure alignment, needed for ARM/x86-32

	m         sync.Mutex
	conn      connwriter
	localConn bool
	hostname  string
	appname   string
	network   string
	raddr     string
	facility  int

	bufferedMessagesCond *sync.Cond
	bufferedMessages     []string
}

// NewSyslogWriter returns a writer that will send log messages to a syslog server
// configured with the provied network, raddr strings.
// Passing in "" as the network will default to using default unix sockets
// Passing in "" as the appname will default to os.Args[0]
func NewSyslogWriter(appname, network, raddr string) (*SyslogWriter, error) {
	syslog := SyslogWriter{
		appname:  sanatizeString(appname, 48),
		network:  network,
		raddr:    raddr,
		facility: FacilityLocal1,
	}
	err := syslog.init()
	return &syslog, err
}

func (syslog *SyslogWriter) init() (err error) {
	syslog.bufferedMessagesCond = sync.NewCond(&sync.Mutex{})

	if syslog.appname == "" {
		if len(os.Args[0]) > 0 {
			syslog.appname = sanatizeString(os.Args[0], 48)
		} else {
			syslog.appname = "-"
		}
	}
	err = syslog.connect()
	if err == nil {
		go syslog.sendloop()
	}

	return
}

// syslog rfc5424 has certain must be ASCII/max length requirements
// this should resolve them
// ASCII is defined as 'PRINTUSASCII' which is %d33-126
func sanatizeString(str string, maxlen int) string {
	var ret []byte
	for i := 0; i < len(str) && len(ret) < maxlen; {
		_, size := utf8.DecodeRuneInString(str[i:])
		if size < 2 {
			char := str[i]
			if char >= 33 && char <= 126 {
				ret = append(ret, char)
			}
		}
		i += size
	}

	return string(ret)
}

func connectNet(network string, raddr string) (conn connwriter, err error) {
	if network == "" {
		return unixSyslog()
	}

	return net.Dial(network, raddr)
}

// connect specified this way so we can subsitute it out in our tests
var connect = connectNet

func (syslog *SyslogWriter) connect() (err error) {
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
			var hn string
			if hn, err = os.Hostname(); err != nil {
				hn = conn.LocalAddr().String()
			}

			syslog.hostname = sanatizeString(hn, 255)
		}
	}
	return
}

func (syslog *SyslogWriter) sendloop() {
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
				// connection closed requested, flush messages, close conn
				syslog.bufferedMessagesCond.L.Unlock()
				syslog.sendBufferedMessages()
				syslog.conn.Close()
				return
			}

			syslog.bufferedMessagesCond.Wait()
		}
		syslog.bufferedMessagesCond.L.Unlock()
	}
}

func (syslog *SyslogWriter) sendBufferedMessages() (ok bool) {
	// swap out the current buffer of messages so we can unblock asap
	syslog.bufferedMessagesCond.L.Lock()
	msgbuffer := syslog.bufferedMessages
	syslog.bufferedMessages = nil
	syslog.bufferedMessagesCond.L.Unlock()
	ok = true

	if len(msgbuffer) < 1 {
		return
	}

	for _, message := range msgbuffer {
		err := syslog.sendMessage(message)
		if err != nil {
			ok = false
			defer syslog.connect()
			break
		}

		msgbuffer = msgbuffer[1:]
	}

	syslog.bufferedMessagesCond.L.Lock()
	if ok == false {
		syslog.bufferedMessages = append(syslog.bufferedMessages, msgbuffer...)
	}
	syslog.bufferedMessagesCond.L.Unlock()
	return
}

func (syslog *SyslogWriter) sendMessage(message string) (err error) {
	_, err = syslog.conn.Write([]byte(message))
	return
}

func (syslog *SyslogWriter) Write(level Level, module, filename string, line int, timestamp time.Time, message string) {
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
	syslog.m.Lock()
	if syslog.localConn {
		hostname = "-"
	} else {
		hostname = syslog.hostname
	}

	priority = syslog.facility + priority
	syslog.m.Unlock()
	header := fmt.Sprintf("<%d>1 %s %s %s %d - -", priority, timestamp.Format(rfc5424), hostname, syslog.appname, os.Getpid())
	msg := fmt.Sprintf("%s %s%s %s:%d %s\n", header, utf8bom, module, filename, line, message)

	syslog.bufferedMessagesCond.L.Lock()
	syslog.bufferedMessages = append(syslog.bufferedMessages, msg)
	syslog.bufferedMessagesCond.Broadcast()
	syslog.bufferedMessagesCond.L.Unlock()
}

// Close will mark the syslog writers connection as closed
// this may not immediately close - an attempt will be made to flush any buffered messages once
// then the connection is closed.
func (syslog *SyslogWriter) Close() {
	syslog.m.Lock()
	defer syslog.m.Unlock()

	if syslog.conn != nil {
		atomic.StoreUint32(&syslog.isClosed, 1)
		syslog.bufferedMessagesCond.L.Lock()
		syslog.bufferedMessages = nil
		syslog.bufferedMessagesCond.Broadcast()
		syslog.bufferedMessagesCond.L.Unlock()
		return
	}

	return
}

// SetFacility lets you set a facility from one of the given
func (syslog *SyslogWriter) SetFacility(facility int) {
	syslog.m.Lock()
	defer syslog.m.Unlock()
	syslog.facility = facility
}
