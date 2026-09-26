package server

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
)

// Phones substitute values such as $display_remote ("John Doe") into the
// Action URL verbatim, which can put raw spaces in the HTTP request line:
//
//	GET /actionurl/incoming_call?display_remote=John Doe&mac=... HTTP/1.1
//
// net/http rejects that with 400. lenientListener wraps each connection so
// that, in every request line, bytes between the method and the trailing
// " HTTP/x.y" that are not valid in a URI are percent-encoded before
// net/http parses them. Header and body bytes pass through untouched.
type lenientListener struct{ net.Listener }

func (l lenientListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &lenientConn{Conn: c, br: bufio.NewReader(c)}, nil
}

type lenientConn struct {
	net.Conn
	br  *bufio.Reader
	out bytes.Buffer // rewritten bytes waiting to be read

	inHeaders   bool
	contentLen  int64 // body bytes still to pass through
	passthrough bool  // stop rewriting (e.g. chunked body we won't parse)
}

func (c *lenientConn) Read(p []byte) (int, error) {
	for c.out.Len() == 0 {
		if c.passthrough {
			return c.br.Read(p)
		}
		if c.contentLen > 0 {
			n := int64(len(p))
			if n > c.contentLen {
				n = c.contentLen
			}
			m, err := c.br.Read(p[:n])
			c.contentLen -= int64(m)
			return m, err
		}
		line, err := c.br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Absurdly long line: give up rewriting and let net/http decide.
			c.out.Write(line)
			c.passthrough = true
			break
		}
		if len(line) == 0 && err != nil {
			return 0, err
		}
		c.handleLine(line)
		if err != nil {
			break
		}
	}
	return c.out.Read(p)
}

func (c *lenientConn) handleLine(line []byte) {
	if !c.inHeaders {
		if len(bytes.TrimSpace(line)) == 0 {
			c.out.Write(line) // stray CRLF between requests
			return
		}
		c.out.WriteString(fixRequestLine(string(line)))
		c.inHeaders = true
		c.contentLen = 0
		return
	}
	c.out.Write(line)
	trimmed := bytes.TrimRight(line, "\r\n")
	if len(trimmed) == 0 {
		c.inHeaders = false // end of headers; body (if any) follows
		return
	}
	name, value, ok := strings.Cut(string(trimmed), ":")
	if !ok {
		return
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "content-length":
		if n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && n > 0 {
			c.contentLen = n
		}
	case "transfer-encoding":
		c.passthrough = true
	}
}

// fixRequestLine percent-encodes spaces and control characters inside the
// request-target of an HTTP/1.x request line.
func fixRequestLine(line string) string {
	body := strings.TrimRight(line, "\r\n")
	eol := line[len(body):]
	method, rest, ok := strings.Cut(body, " ")
	if !ok {
		return line
	}
	i := strings.LastIndex(rest, " ")
	if i < 0 || !strings.HasPrefix(rest[i+1:], "HTTP/") {
		return line
	}
	target, proto := rest[:i], rest[i+1:]
	var b strings.Builder
	for j := 0; j < len(target); j++ {
		ch := target[j]
		if ch <= ' ' || ch == 0x7f || ch == '"' || ch == '<' || ch == '>' ||
			ch == '\\' || ch == '^' || ch == '`' || ch == '{' || ch == '|' || ch == '}' {
			b.WriteString("%" + strings.ToUpper(strconv.FormatUint(uint64(ch)|0x100, 16)[1:]))
			continue
		}
		b.WriteByte(ch)
	}
	return method + " " + b.String() + " " + proto + eol
}

var _ io.Reader = (*lenientConn)(nil)
