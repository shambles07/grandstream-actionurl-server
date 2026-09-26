package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"time"
)

// expecter is a minimal expect(1)-style driver over an interactive stream:
// it accumulates output and waits until a regular expression matches.
type expecter struct {
	w      io.Writer
	chunks chan []byte
	errc   chan error
	done   chan struct{}
	buf    []byte
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b[()][A-Za-z0-9]|\r`)

func newExpecter(r io.Reader, w io.Writer, transcript io.Writer) *expecter {
	e := &expecter{w: w, chunks: make(chan []byte, 16), errc: make(chan error, 1), done: make(chan struct{})}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := r.Read(b)
			if n > 0 {
				chunk := append([]byte(nil), b[:n]...)
				if transcript != nil {
					transcript.Write(chunk)
				}
				select {
				case e.chunks <- chunk:
				case <-e.done:
					return
				}
			}
			if err != nil {
				e.errc <- err
				close(e.chunks)
				return
			}
		}
	}()
	return e
}

// close stops the reader goroutine once the underlying stream is closed.
func (e *expecter) close() { close(e.done) }

// send writes one line terminated by CR, as a terminal would.
func (e *expecter) send(line string) error {
	_, err := io.WriteString(e.w, line+"\r")
	return err
}

// errExpectTimeout is returned when no pattern matches in time.
var errExpectTimeout = errors.New("timed out waiting for expected output")

// expect waits until one of pats matches the buffered output (with ANSI
// escapes and CRs removed). It returns the index of the matching pattern and
// the output up to and including the match, which is then consumed.
func (e *expecter) expect(ctx context.Context, timeout time.Duration, pats ...*regexp.Regexp) (int, string, error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	for {
		clean := ansiEscape.ReplaceAll(e.buf, nil)
		for i, p := range pats {
			if loc := p.FindIndex(clean); loc != nil {
				out := string(clean[:loc[1]])
				e.buf = append(e.buf[:0], clean[loc[1]:]...)
				return i, out, nil
			}
		}
		select {
		case chunk, ok := <-e.chunks:
			if !ok {
				err := <-e.errc
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				return -1, string(clean), fmt.Errorf("connection closed: %w (last output: %q)", err, tail(clean))
			}
			e.buf = append(e.buf, chunk...)
		case <-t.C:
			return -1, string(clean), fmt.Errorf("%w (last output: %q)", errExpectTimeout, tail(clean))
		case <-ctx.Done():
			return -1, string(clean), ctx.Err()
		}
	}
}

func tail(b []byte) string {
	const n = 120
	if len(b) > n {
		return "..." + string(b[len(b)-n:])
	}
	return string(b)
}
