// Copyright 2016 Canonical Ltd.
// Licensed under the LGPLv3, see LICENCE file for details.

package loggo

import (
	"errors"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	syslogParser "github.com/jeromer/syslogparser/rfc5424"
	gc "gopkg.in/check.v1"
)

type SyslogWriterSuite struct{}

var _ = gc.Suite(&SyslogWriterSuite{})

type mockAddr struct {
}

func (m mockAddr) Network() string {
	return "mocked"
}
func (m mockAddr) String() string {
	return "mocked"
}

type mockConn struct {
	sync.Mutex

	isClosed  bool
	localAddr net.Addr
	messages  chan string

	writeShouldReturnError bool
}

func (conn *mockConn) Close() error {
	conn.Lock()
	defer conn.Unlock()
	conn.isClosed = true
	return nil
}

func (conn mockConn) LocalAddr() net.Addr {
	return conn.localAddr
}

func (conn *mockConn) Write(b []byte) (int, error) {
	conn.Lock()
	defer conn.Unlock()

	if conn.writeShouldReturnError {
		return 0, errors.New("conn.writeShouldReturnError is true")
	}

	conn.messages <- string(b)
	return len(b), nil
}

func (s *SyslogWriterSuite) TestSyslogWriterConnection(c *gc.C) {
	// makes sure the internal network connection logic inside
	// SyslogWriter is solid
	// if a connection drops it should try and re-establish and buffer messages until it has
	// when connections are re-established.
	// if the connection is closed, it should ignore all Writes
	conn := mockConn{
		localAddr: &mockAddr{},
		messages:  make(chan string, 10),
	}

	// swap out global connect function for one that mocks it out
	connect = func(_ string, _ string) (connwriter, error) {
		return &conn, nil
	}

	writer := SyslogWriter{}
	err := writer.init()
	c.Assert(err, gc.Equals, nil)

	// ensure writing data sends the data to the connection and clears the buffered message
	writer.Write(CRITICAL, "testmodule", "syslogwriter_test.go", 80, time.Now(), "test message")
	run := true
	for run {
		select {
		case <-conn.messages:
			run = false
		case <-time.After(time.Millisecond * 5000):
			run = false
			c.Log("Write() failed, no message recieved after 50ms")
			c.FailNow()
		}
	}
	c.Assert(len(writer.bufferedMessages), gc.Equals, 0)

	// ensure that if the connection is closed
	conn.writeShouldReturnError = true
	writer.Write(CRITICAL, "testmodule", "syslogwriter_test.go", 80, time.Now(), "test message")
	writer.Write(CRITICAL, "testmodule", "syslogwriter_test.go", 80, time.Now(), "test message")
	writer.Write(CRITICAL, "testmodule", "syslogwriter_test.go", 80, time.Now(), "test message")
	writer.bufferedMessagesCond.L.Lock()
	c.Assert(len(writer.bufferedMessages), gc.Equals, 3)
	writer.bufferedMessagesCond.L.Unlock()

	run = true
	for run {
		select {
		case <-conn.messages:
			run = false
			c.Log("Write() succeeded when connection was closed")
			c.FailNow()
		case <-time.After(time.Millisecond * 20):
			run = false
		}
	}
	writer.bufferedMessagesCond.L.Lock()
	c.Assert(len(writer.bufferedMessages), gc.Equals, 3)
	writer.bufferedMessagesCond.L.Unlock()

	conn.Lock()
	conn.writeShouldReturnError = false
	conn.Unlock()
	run = true
	foundMessages := 0
	for run {
		select {
		case <-conn.messages:
			foundMessages++
			if foundMessages > 2 {
				run = false
			}
		case <-time.After(time.Millisecond * 400):
			run = false
			c.Log("Not enough writes after connection restored, timed out after 400ms")
			c.FailNow()
		}
	}
	c.Assert(len(writer.bufferedMessages), gc.Equals, 0)

	writer.Close()
	writer.Write(CRITICAL, "testmodule", "syslogwriter_test.go", 80, time.Now(), "test message")
	writer.bufferedMessagesCond.L.Lock()
	c.Assert(len(writer.bufferedMessages), gc.Equals, 0)
	writer.bufferedMessagesCond.L.Unlock()
}

func (s *SyslogWriterSuite) TestSyslogWriterWrite(c *gc.C) {
	now := time.Now()

	localWriter := SyslogWriter{
		appname:              "localWriter",
		bufferedMessagesCond: sync.NewCond(&sync.Mutex{}),
		facility:             FacilityLocal1,
		hostname:             "shouldnotexist",
		localConn:            true,
	}

	localWriter.Write(CRITICAL, "testmodule", "syslogwriter_test.go", 32, now, "my local test message")

	localWriter.bufferedMessagesCond.L.Lock()
	p := syslogParser.NewParser([]byte(localWriter.bufferedMessages[0]))
	localWriter.bufferedMessagesCond.L.Unlock()

	err := p.Parse()
	c.Assert(err, gc.IsNil)
	parsed := p.Dump()

	parsedTime, ok := parsed["timestamp"].(time.Time)
	c.Assert(ok, gc.Equals, true)

	c.Assert(parsedTime.Format(rfc5424), gc.Equals, now.Format(rfc5424))
	c.Assert(parsed["hostname"], gc.Not(gc.Equals), localWriter.hostname)
	c.Assert(parsed["message"], gc.Equals, "\ufefftestmodule syslogwriter_test.go:32 my local test message\n")
	c.Assert(parsed["priority"], gc.Equals, 138)
	c.Assert(parsed["proc_id"], gc.Equals, strconv.Itoa(os.Getpid()))

	remoteWriter := SyslogWriter{
		appname:              "remoteWriter",
		bufferedMessagesCond: sync.NewCond(&sync.Mutex{}),
		facility:             FacilityLocal1,
		hostname:             "shouldexist",
		localConn:            false,
	}

	// ensure that hostnames are added for non local messages
	remoteWriter.Write(CRITICAL, "testmodule", "syslogwriter_test.go", 32, now, "my local test message")
	remoteWriter.bufferedMessagesCond.L.Lock()
	p = syslogParser.NewParser([]byte(remoteWriter.bufferedMessages[0]))
	remoteWriter.bufferedMessagesCond.L.Unlock()

	err = p.Parse()
	c.Assert(err, gc.IsNil)
	parsed = p.Dump()

	parsedTime, ok = parsed["timestamp"].(time.Time)
	c.Assert(ok, gc.Equals, true)

	c.Assert(parsedTime.Format(rfc5424), gc.Equals, now.Format(rfc5424))
	c.Assert(parsed["hostname"], gc.Equals, remoteWriter.hostname)
	c.Assert(parsed["message"], gc.Equals, "\ufefftestmodule syslogwriter_test.go:32 my local test message\n")
	c.Assert(parsed["priority"], gc.Equals, 138)
	c.Assert(parsed["proc_id"], gc.Equals, strconv.Itoa(os.Getpid()))

	// make sure trace messages do not get added
	localWriter.Write(TRACE, "testmodule", "syslogwriter_test.go", 32, now, "my local test message")
	localWriter.bufferedMessagesCond.L.Lock()
	c.Assert(len(localWriter.bufferedMessages), gc.Equals, 1)
	localWriter.bufferedMessagesCond.L.Unlock()
}

func (s *SyslogWriterSuite) TestsanatizeString(c *gc.C) {
	c.Assert(sanatizeString("this is valid ascii", 10), gc.Equals, "thisisvali")
	c.Assert(sanatizeString("this🙈🙉🙊is valid ascii", 10), gc.Equals, "thisisvali")
}

func (s *SyslogWriterSuite) TestRealSyslogFLush(c *gc.C) {
	// this test will *only* work on unix and depends on the system syslog daemon
	writer, err := NewSyslogWriter("testprocess", "tcp4", "localhost:601")
	c.Assert(err, gc.Equals, nil)
	writer.Write(DEBUG, "arealmodule", "myfavouritefile.go", 32, time.Now(), "yet another test message!")
	<-time.After(50 * time.Millisecond)

	writer.bufferedMessagesCond.L.Lock()
	c.Assert(len(writer.bufferedMessages), gc.Equals, 0)
	writer.bufferedMessagesCond.L.Unlock()
}
