package simdhttp

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/sebishogun/simdhttp/http1"
)

// The optional error-returning handler shape. A handler that manages its own
// errors never touches this: Wrap is opt-in, and nothing else in the package
// wraps a ResponseWriter.
//
// The reason it must wrap one is narrow: once a response has started there is
// no status left to send, and writing a second one corrupts the stream. The
// adapter has to know which case it is in, and net/http exposes no way to ask.

// ErrorMapper turns a handler's error into a status and a client-visible
// message. A mapper is the seam for an application's own taxonomy; the default
// covers the parser and framing sentinels.
type ErrorMapper func(error) (status int, message string)

// ErrorAdapter converts error-returning handlers into http.Handlers. The zero
// value uses DefaultErrorMapper and the standard logger.
type ErrorAdapter struct {
	// Map converts an error to a status. nil uses DefaultErrorMapper.
	Map ErrorMapper
	// Log records an error that arrived too late to become a status. nil logs
	// through the standard logger.
	Log func(*http.Request, error)
}

// Wrap adapts an error-returning handler using the default adapter.
func Wrap(h func(http.ResponseWriter, *http.Request) error) http.Handler {
	return ErrorAdapter{}.Wrap(h)
}

// Wrap adapts an error-returning handler.
func (a ErrorAdapter) Wrap(h func(http.ResponseWriter, *http.Request) error) http.Handler {
	mapper := a.Map
	if mapper == nil {
		mapper = DefaultErrorMapper
	}
	logf := a.Log
	if logf == nil {
		logf = func(req *http.Request, err error) {
			log.Printf("simdhttp: %s %s: %v (response already started)", req.Method, req.URL, err)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rec := &startTracker{ResponseWriter: w}
		err := h(trackerFor(rec), req)
		if err == nil {
			return
		}
		if rec.started {
			// The client already has a status and some bytes. Sending another
			// status would put two responses on one connection, which is the
			// shape a smuggled request exploits, so the connection is aborted
			// instead. ErrAbortHandler closes it without a stack trace.
			logf(req, err)
			panic(http.ErrAbortHandler)
		}
		status, msg := mapper(err)
		http.Error(w, msg, status)
	})
}

// DefaultErrorMapper maps the parser and framing sentinels to the statuses RFC
// 9110 gives them, and everything else to 500 with no detail: an unexpected
// error's text can name a host, a file, or a query, and the client asked for
// none of them.
func DefaultErrorMapper(err error) (int, string) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, http1.ErrMissingHost),
		errors.Is(err, http1.ErrMalformed),
		errors.Is(err, http1.ErrIncomplete),
		errors.Is(err, http1.ErrAmbiguousFraming),
		errors.Is(err, http1.ErrBadContentLength),
		errors.Is(err, http1.ErrBadChunk):
		status = http.StatusBadRequest
	case errors.Is(err, http1.ErrBadTransferEncoding):
		// RFC 9110: a transfer coding the server does not support is 501, not
		// a malformed request -- the message was well formed.
		status = http.StatusNotImplemented
	case errors.Is(err, http1.ErrBodyTooLarge),
		errors.Is(err, http1.ErrDrainTooLarge),
		errors.Is(err, http1.ErrChunkLineTooLong),
		errors.Is(err, http1.ErrChunkExtensionTooLong),
		errors.Is(err, http1.ErrTooManyTrailers):
		status = http.StatusRequestEntityTooLarge
	case errors.Is(err, http1.ErrHeadTooLarge),
		errors.Is(err, http1.ErrTooManyHeaders),
		errors.Is(err, http1.ErrValueTooLarge):
		status = http.StatusRequestHeaderFieldsTooLarge
	case errors.Is(err, http1.ErrRequestLineTooLarge):
		status = http.StatusRequestURITooLong
	}
	return status, http.StatusText(status)
}

// ---- response-start tracking ----

// startTracker records whether anything has been written. It forwards
// everything else and exposes Unwrap, so http.ResponseController reaches the
// real writer.
type startTracker struct {
	http.ResponseWriter
	started bool
}

func (s *startTracker) WriteHeader(code int) {
	s.started = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *startTracker) Write(b []byte) (int, error) {
	s.started = true
	return s.ResponseWriter.Write(b)
}

func (s *startTracker) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// ReadFrom is implemented unconditionally. io.ReaderFrom is an optimization
// hint rather than a capability a handler branches on, so presenting it costs
// nothing even when the underlying writer lacks it -- the fallback copy has
// the same meaning.
func (s *startTracker) ReadFrom(r io.Reader) (int64, error) {
	s.started = true
	if rf, ok := s.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(s.ResponseWriter, r)
}

// Flusher and Hijacker are different: a handler asks whether they are there
// and takes another path when they are not. Claiming either over a writer that
// lacks it would send a streaming or WebSocket handler down a road the server
// cannot follow, so the wrapper is assembled to present exactly the pair the
// underlying writer has.
type trackFlusher struct{ *startTracker }

func (s trackFlusher) Flush() {
	s.started = true
	s.ResponseWriter.(http.Flusher).Flush()
}

type trackHijacker struct{ *startTracker }

func (s trackHijacker) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	s.started = true // the connection is the caller's now; nothing may be written
	return s.ResponseWriter.(http.Hijacker).Hijack()
}

type trackFlushHijack struct{ *startTracker }

func (s trackFlushHijack) Flush() {
	s.started = true
	s.ResponseWriter.(http.Flusher).Flush()
}

func (s trackFlushHijack) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	s.started = true
	return s.ResponseWriter.(http.Hijacker).Hijack()
}

func trackerFor(s *startTracker) http.ResponseWriter {
	_, f := s.ResponseWriter.(http.Flusher)
	_, h := s.ResponseWriter.(http.Hijacker)
	switch {
	case f && h:
		return trackFlushHijack{s}
	case f:
		return trackFlusher{s}
	case h:
		return trackHijacker{s}
	default:
		return s
	}
}
