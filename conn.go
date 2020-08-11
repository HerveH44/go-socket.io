package socketio

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"

	engineio "github.com/googollee/go-engine.io"

	"github.com/herveh44/go-socket.io/parser"
)

// Conn is a connection in go-socket.io
type Conn interface {
	// ID returns session id
	ID() string
	Close() error
	URL() url.URL
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	RemoteHeader() http.Header

	// Context of this connection. You can save one context for one
	// connection, and share it between all handlers. The handlers
	// is called in one goroutine, so no need to lock context if it
	// only be accessed in one connection.
	Context() interface{}
	SetContext(v interface{})
	Namespace() string
	Emit(msg string, v ...interface{})

	// Broadcast server side apis
	Join(room string)
	Leave(room string)
	LeaveAll()
	Rooms() []string
}

type errorMessage struct {
	namespace string
	error
}

type writePacket struct {
	data []interface{}
}

type conn struct {
	engineio.Conn
	encoder    *parser.Encoder
	decoder    *parser.Decoder
	errorChan  chan errorMessage
	writeChan  chan writePacket
	quitChan   chan struct{}
	handlers   map[string]*namespaceHandler
	namespaces map[string]*namespaceConn
	closeOnce  sync.Once
	id         uint64
}

func newConn(c engineio.Conn, handlers map[string]*namespaceHandler) (*conn, error) {
	ret := &conn{
		Conn:       c,
		encoder:    parser.NewEncoder(c),
		decoder:    parser.NewDecoder(c),
		errorChan:  make(chan errorMessage),
		writeChan:  make(chan writePacket),
		quitChan:   make(chan struct{}),
		handlers:   handlers,
		namespaces: make(map[string]*namespaceConn),
	}
	if err := ret.connect(); err != nil {
		ret.Close()
		return nil, err
	}
	return ret, nil
}

func (c *conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		// For each namespace, leave all rooms, and call the disconnect handler.
		for ns, nc := range c.namespaces {
			nc.LeaveAll()
			if nh := c.handlers[ns]; nh != nil && nh.onDisconnect != nil {
				nh.onDisconnect(nc, "client namespace disconnect")
			}
		}
		err = c.Conn.Close()
		close(c.quitChan)
	})
	return err
}

func (c *conn) connect() error {
	rootHandler, ok := c.handlers[""]
	if !ok {
		return errors.New("root ('/') doesn't have a namespace handler")
	}
	root := newNamespaceConn(c, "/", rootHandler.broadcast)
	c.namespaces[""] = root
	root.Join(root.ID())

	for _, ns := range c.namespaces {
		ns.SetContext(c.Conn.Context())
	}
	go c.serveError()
	go c.serveWrite()
	go c.serveRead()

	rootHandler.dispatch(root, "", nil)

	return nil
}

func (c *conn) nextID() uint64 {
	c.id++
	return c.id
}

func (c *conn) write(args []reflect.Value) {
	data := make([]interface{}, len(args))
	for i := range data {
		data[i] = args[i].Interface()
	}
	pkg := writePacket{
		data: data,
	}
	select {
	case c.writeChan <- pkg:
	case <-c.quitChan:
		return
	}
}

func (c *conn) onError(namespace string, err error) {
	onErr := errorMessage{
		namespace: namespace,
		error:     err,
	}
	select {
	case c.errorChan <- onErr:
	case <-c.quitChan:
		return
	}
}

func (c *conn) parseArgs(types []reflect.Type) ([]reflect.Value, error) {
	return c.decoder.DecodeArgs(types)
}

func (c *conn) serveError() {
	defer c.Close()
	for {
		select {
		case <-c.quitChan:
			return
		case msg := <-c.errorChan:
			if handler := c.namespace(msg.namespace); handler != nil {
				if handler.onError != nil {
					handler.onError(c.namespaces[msg.namespace], msg.error)
				}
			}
		}
	}
}

func (c *conn) serveWrite() {
	defer c.Close()
	for {
		select {
		case <-c.quitChan:
			return
		case pkg := <-c.writeChan:
			if err := c.encoder.Encode(pkg.data); err != nil {
				c.onError("pkg.header.Namespace", err)
			}
		}
	}
}

func (c *conn) serveRead() {
	defer c.Close()
	var event string
	for {
		conn, ok := c.namespaces[""]
		if !ok {
			c.decoder.DiscardLast()
			continue
		}
		handler, ok := c.handlers[""]
		if !ok {
			c.decoder.DiscardLast()
			continue
		}
		types := handler.getTypes(event)
		args, err := c.decoder.DecodeArgs(types)
		if err != nil {
			c.onError("header.Namespace", err)
			return
		}
		ret, err := handler.dispatch(conn, event, args)
		if err != nil {
			c.onError("header.Namespace", err)
			return
		}
		if len(ret) > 0 {
			c.write(ret)
		}
	}
}

func (c *conn) namespace(nsp string) *namespaceHandler {
	return c.handlers[nsp]
}
