package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Lean HTTP/1.1 frontend (round 4). Round 2b's profile put 85–89% of this
// server's CPU inside net/http's request parsing (readRequest /
// readMIMEHeader); the handlers themselves run in microseconds. This
// frontend parses just enough of the request itself, hands the SAME
// handler an ordinary *http.Request, and buffers the handler's response
// so it leaves in a single Write. Interleaved full-bench matrices
// (PROGRESS.md, Round 4) measured it ahead of the net/http build on
// every workload. Semantics mirrored from net/http where a client can
// observe them: keep-alive rules per protocol version, Content-Length
// framing, chunked request bodies, 100-continue, Content-Type sniffing
// when the handler did not set one, HEAD body suppression.
//
// FORGE_STD_HTTP=1 at startup serves through net/http instead — an
// operational fallback, checked once here and never per request.

const (
	rawMaxLine    = 65536
	rawMaxHeaders = 200
	rawMaxBody    = 1 << 26
)

var rawBufPool = sync.Pool{New: func() any { return new(bytes.Buffer) }}

func startServer(ln net.Listener, h http.Handler) func(context.Context) error {
	if os.Getenv("FORGE_STD_HTTP") != "" {
		srv := &http.Server{Handler: h}
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				fmt.Fprintln(os.Stderr, "[forge] serve:", err)
			}
		}()
		return srv.Shutdown
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveRawConn(c, h)
		}
	}()
	return func(context.Context) error { return ln.Close() }
}

// rawResponse is a buffering http.ResponseWriter.
type rawResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func (r *rawResponse) Header() http.Header { return r.header }

func (r *rawResponse) WriteHeader(code int) {
	if r.wrote {
		return
	}
	r.wrote = true
	r.status = code
}

func (r *rawResponse) Write(p []byte) (int, error) {
	if !r.wrote {
		r.WriteHeader(http.StatusOK)
	}
	return r.body.Write(p)
}

func serveRawConn(c net.Conn, h http.Handler) {
	defer c.Close()
	br := bufio.NewReaderSize(c, 65536)
	for {
		req, keepAlive, err := readRawRequest(c, br)
		if err != nil {
			return
		}
		rec := &rawResponse{header: make(http.Header), status: http.StatusOK}
		h.ServeHTTP(rec, req)
		if !writeRawResponse(c, rec, req, keepAlive) {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// readRawLine reads one CRLF/LF-terminated line, bounded.
func readRawLine(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		frag, err := br.ReadSlice('\n')
		sb.Write(frag)
		if sb.Len() > rawMaxLine {
			return "", io.ErrUnexpectedEOF
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			return "", err
		}
		return strings.TrimRight(sb.String(), "\r\n"), nil
	}
}

func readRawRequest(c net.Conn, br *bufio.Reader) (*http.Request, bool, error) {
	line, err := readRawLine(br)
	if err != nil {
		return nil, false, err
	}
	if line == "" { // tolerate a stray CRLF before the request line
		line, err = readRawLine(br)
		if err != nil {
			return nil, false, err
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, io.ErrUnexpectedEOF
	}
	method, rawURI, proto := parts[0], parts[1], parts[2]
	major, minor := 1, 1
	if strings.HasPrefix(proto, "HTTP/") {
		v := proto[5:]
		if dot := strings.IndexByte(v, '.'); dot >= 0 {
			major, _ = strconv.Atoi(v[:dot])
			minor, _ = strconv.Atoi(v[dot+1:])
		}
	}

	header := make(http.Header)
	var contentLength int64 = -1
	chunked := false
	expectContinue := false
	connTokens := ""
	host := ""
	for i := 0; i < rawMaxHeaders; i++ {
		hl, err := readRawLine(br)
		if err != nil {
			return nil, false, err
		}
		if hl == "" {
			break
		}
		colon := strings.IndexByte(hl, ':')
		if colon < 0 {
			return nil, false, io.ErrUnexpectedEOF
		}
		name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(hl[:colon]))
		value := strings.TrimSpace(hl[colon+1:])
		header[name] = append(header[name], value)
		switch name {
		case "Content-Length":
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n >= 0 {
				contentLength = n
			}
		case "Transfer-Encoding":
			if strings.Contains(strings.ToLower(value), "chunked") {
				chunked = true
			}
		case "Expect":
			if strings.EqualFold(value, "100-continue") {
				expectContinue = true
			}
		case "Connection":
			connTokens = strings.ToLower(value)
		case "Host":
			host = value
		}
	}

	if expectContinue {
		if _, err := io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n"); err != nil {
			return nil, false, err
		}
	}

	var body []byte
	switch {
	case chunked:
		body, err = readRawChunked(br)
		if err != nil {
			return nil, false, err
		}
		contentLength = int64(len(body))
	case contentLength > 0:
		if contentLength > rawMaxBody {
			return nil, false, io.ErrUnexpectedEOF
		}
		body = make([]byte, contentLength)
		if _, err := io.ReadFull(br, body); err != nil {
			return nil, false, err
		}
	case contentLength == 0:
		body = nil
	}

	u, err := url.ParseRequestURI(rawURI)
	if err != nil {
		return nil, false, err
	}
	req := &http.Request{
		Method:        method,
		RequestURI:    rawURI,
		URL:           u,
		Proto:         proto,
		ProtoMajor:    major,
		ProtoMinor:    minor,
		Header:        header,
		Host:          host,
		RemoteAddr:    c.RemoteAddr().String(),
		ContentLength: contentLength,
		Body:          io.NopCloser(bytes.NewReader(body)),
	}
	if chunked {
		req.TransferEncoding = []string{"chunked"}
	}

	keepAlive := false
	if major > 1 || (major == 1 && minor >= 1) {
		keepAlive = !strings.Contains(connTokens, "close")
	} else {
		keepAlive = strings.Contains(connTokens, "keep-alive")
	}
	return req, keepAlive, nil
}

func readRawChunked(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := readRawLine(br)
		if err != nil {
			return nil, err
		}
		sizeText := line
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		n, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || n < 0 || n > rawMaxBody {
			return nil, io.ErrUnexpectedEOF
		}
		if n == 0 {
			// trailers until the empty line
			for {
				tl, err := readRawLine(br)
				if err != nil {
					return nil, err
				}
				if tl == "" {
					return buf.Bytes(), nil
				}
			}
		}
		if _, err := io.CopyN(&buf, br, n); err != nil {
			return nil, err
		}
		// trailing CRLF after each chunk
		cr, err := br.ReadByte()
		if err != nil {
			return nil, err
		}
		if cr == '\r' {
			if _, err := br.ReadByte(); err != nil {
				return nil, err
			}
		}
	}
}

func writeRawResponse(c net.Conn, rec *rawResponse, req *http.Request, keepAlive bool) bool {
	body := rec.body.Bytes()
	status := rec.status
	bodyless := status == http.StatusNoContent || status == http.StatusNotModified ||
		(status >= 100 && status < 200)
	if bodyless {
		body = nil
	}
	out := rawBufPool.Get().(*bytes.Buffer)
	out.Reset()
	defer rawBufPool.Put(out)
	out.WriteString("HTTP/1.1 ")
	out.WriteString(strconv.Itoa(status))
	out.WriteByte(' ')
	out.WriteString(http.StatusText(status))
	out.WriteString("\r\n")
	for name, values := range rec.header {
		if name == "Content-Length" || name == "Transfer-Encoding" || name == "Connection" {
			continue
		}
		for _, v := range values {
			out.WriteString(name)
			out.WriteString(": ")
			out.WriteString(v)
			out.WriteString("\r\n")
		}
	}
	if _, hasCT := rec.header["Content-Type"]; !hasCT && len(body) > 0 {
		sniff := body
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		out.WriteString("Content-Type: ")
		out.WriteString(http.DetectContentType(sniff))
		out.WriteString("\r\n")
	}
	if !bodyless {
		out.WriteString("Content-Length: ")
		out.WriteString(strconv.Itoa(len(body)))
		out.WriteString("\r\n")
	}
	if !keepAlive {
		out.WriteString("Connection: close\r\n")
	} else if req.ProtoMajor == 1 && req.ProtoMinor == 0 {
		out.WriteString("Connection: keep-alive\r\n")
	}
	out.WriteString("\r\n")
	if req.Method != http.MethodHead {
		out.Write(body)
	}
	_, err := c.Write(out.Bytes())
	return err == nil
}
