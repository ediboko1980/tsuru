// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"unicode"

	"github.com/tsuru/tsuru/api/context"
	"github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/event"
	"github.com/tsuru/tsuru/permission"
	"github.com/tsuru/tsuru/provision"
	"golang.org/x/crypto/ssh/terminal"
	"golang.org/x/net/websocket"
)

var _ io.ReadWriteCloser = &cmdLogger{}

type cmdLogger struct {
	sync.Mutex
	base        io.ReadWriteCloser
	term        io.Writer
	addComplete bool
}

func (l *cmdLogger) Read(p []byte) (n int, err error) {
	// XXX(cezarsa): this way of capturing executed commands is ugly, unreliable
	// and error prone. It's here as a proof of concept and it's probably better
	// than nothing. I will think about a better approach to this in the future.
	// For now, I already spent too much time tweaking this code.
	n, err = l.base.Read(p)
	if err != nil || n == 0 {
		return
	}
	l.term.Write(p[:n])
	l.Lock()
	defer l.Unlock()
	l.addComplete = p[n-1] == '\t'
	return
}

func (l *cmdLogger) Write(p []byte) (n int, err error) {
	n, err = l.base.Write(p)
	l.Lock()
	defer l.Unlock()
	if l.addComplete {
		for _, c := range string(p) {
			if unicode.IsPrint(c) {
				l.term.Write([]byte(string(c)))
			}
		}
		if len(p) == 0 || p[len(p)-1] != '\a' {
			l.addComplete = false
		}
	}
	return
}

func (l *cmdLogger) Close() error {
	return l.base.Close()
}

type optionalWriterCloser struct {
	bytes.Buffer
	disableWrite bool
}

func (l *optionalWriterCloser) Write(p []byte) (int, error) {
	if l.disableWrite {
		return len(p), nil
	}
	return l.Buffer.Write(p)
}

func (l *optionalWriterCloser) Close() error {
	return nil
}

func remoteShellHandler(ws *websocket.Conn) {
	var httpErr *errors.HTTP
	defer func() {
		defer ws.Close()
		if httpErr != nil {
			var msg string
			switch httpErr.Code {
			case http.StatusUnauthorized:
				msg = "no token provided or session expired, please login again\n"
			default:
				msg = httpErr.Message + "\n"
			}
			ws.Write([]byte("Error: " + msg))
		}
	}()
	r := ws.Request()
	token := context.GetAuthToken(r)
	if token == nil {
		httpErr = &errors.HTTP{
			Code:    http.StatusUnauthorized,
			Message: "no token provided",
		}
		return
	}
	appName := r.URL.Query().Get(":appname")
	a, err := getAppFromContext(appName, r)
	if err != nil {
		if herr, ok := err.(*errors.HTTP); ok {
			httpErr = herr
		} else {
			httpErr = &errors.HTTP{
				Code:    http.StatusInternalServerError,
				Message: err.Error(),
			}
		}
		return
	}
	allowed := permission.Check(token, permission.PermAppRunShell,
		append(permission.Contexts(permission.CtxTeam, a.Teams),
			permission.Context(permission.CtxApp, a.Name),
			permission.Context(permission.CtxPool, a.Pool),
		)...,
	)
	if !allowed {
		httpErr = permission.ErrUnauthorized
		return
	}
	buf := &optionalWriterCloser{}
	var term *terminal.Terminal
	unitID := r.URL.Query().Get("unit")
	width, _ := strconv.Atoi(r.URL.Query().Get("width"))
	height, _ := strconv.Atoi(r.URL.Query().Get("height"))
	clientTerm := r.URL.Query().Get("term")
	evt, err := event.New(&event.Opts{
		Target:      appTarget(appName),
		Kind:        permission.PermAppRunShell,
		Owner:       token,
		CustomData:  formToEvents(r.Form),
		DisableLock: true,
	})
	if err != nil {
		httpErr = &errors.HTTP{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}
		return
	}
	defer func() {
		var finalErr error
		if httpErr != nil {
			finalErr = httpErr
		}
		for term != nil {
			buf.disableWrite = true
			var line string
			line, err = term.ReadLine()
			if err != nil {
				break
			}
			fmt.Fprintf(evt, "> %s\n", line)
		}
		evt.Done(finalErr)
	}()
	term = terminal.NewTerminal(buf, "")
	opts := provision.ShellOptions{
		Conn:   &cmdLogger{base: ws, term: term},
		Width:  width,
		Height: height,
		Unit:   unitID,
		Term:   clientTerm,
	}
	err = a.Shell(opts)
	if err != nil {
		httpErr = &errors.HTTP{
			Code:    http.StatusInternalServerError,
			Message: err.Error(),
		}
	}
}
