package gami

import (
	"bytes"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"sync"
	"strings"
)

const (
	_LINE_TERM    = "\r\n"            // packet line separator
	_KEY_VAL_TERM = ":"               // header value separator
	_READ_BUF     = 1024              // buffer size for socket reader
	_CMD_END      = "--END COMMAND--" // Asterisk command data end
	_HOST         = "gami"            // default host value
	ORIG_TMOUT    = 30000             // Originate timeout
)

var (
	_PT_BYTES = []byte(_LINE_TERM + _LINE_TERM) // packet separator
)

// basic Asterisk message
type Message map[string]string

// Nice output with sorted keys
func (m Message) String() string {
	res := []string{"\n"}
	keys := []string{}
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := m[k]
		res = append(res, fmt.Sprintf("\t%s:%s%s", k, strings.Repeat(" ", 20 - len(k)), v))
	}
	return strings.Join(res, "\n")
}

// action id generator
type Aid struct {
	host string
	id   int
	mu   *sync.RWMutex
}

// NewAid, Aid factory
func NewAid() *Aid {

	var err error
	a := &Aid{}
	a.mu = &sync.RWMutex{}

	if a.host, err = os.Hostname(); err != nil {
		a.host = _HOST
	}

	return a
}

// GenerateId, generate new action id
func (a *Aid) Generate() string {

	a.mu.Lock()
	defer a.mu.Unlock()
	a.id++
	return a.host + "-" + fmt.Sprint(a.id)
}

// callback function storage
type cbList struct {
	mu *sync.RWMutex
	f  map[string]*func(Message)
	sd map[string]bool // callback will self delete (used for multi-message responses)
}

// set, setting handle function for specific action id|event (will overwrite current if present)
func (cbl *cbList) set(key string, f *func(Message), sd bool) {

	cbl.mu.Lock()
	defer cbl.mu.Unlock()
	cbl.f[key] = f
	cbl.sd[key] = sd
}

// del, deleting callback for specific action id|event
func (cbl *cbList) del(key string) {

	cbl.mu.Lock()
	defer cbl.mu.Unlock()
	delete(cbl.f, key)
	delete(cbl.sd, key)
}

// get, returns function for specific action id/event
func (cbl *cbList) get(key string) (*func(Message), bool) {

	cbl.mu.RLock()
	defer cbl.mu.RUnlock()
	return cbl.f[key], cbl.sd[key]
}

// Originate, struct used in Originate command
// if pointed Context and Application, Context has higher priority
type Originate struct {
	Channel  string // channel to which originate
	Context  string // context to move after originate success
	Exten    string // exten to move after originate success
	Priority string // priority to move after originate success
	Timeout  int    // originate timeout ms
	CallerID string // caller identification string
	Account  string // used for CDR

	Application string // application to execute after successful originate
	Data        string // data passed to application

	Async bool // asynchronous call
}

// NewOriginate, Originate default values constructor (to context)
func NewOriginate(channel, context, exten, priority string) *Originate {
	return &Originate{
		Channel:  channel,
		Context:  context,
		Exten:    exten,
		Priority: priority,
		Timeout:  ORIG_TMOUT,
		Async:    false,
	}
}

// NewOriginateApp, constructor for originate to application
func NewOriginateApp(channel, app, data string) *Originate {
	return &Originate{
		Channel:     channel,
		Timeout:     ORIG_TMOUT,
		Application: app,
		Data:        data,
		Async:       false,
	}
}

// main working entity
type Asterisk struct {
	address        string         // string adress to host
	login          string         // login for AMI
	password       string         // password for AMI
	conn           *net.TCPConn   // network connection to Asterisk
	actionHandlers *cbList        // action response handle functions
	eventHandlers  *cbList        // event handle functions
	defaultHandler *func(Message) // default handler for all Asterisk messages, useful for debugging
	netErrHandler  *func(error)   // network error handle function
	aid            *Aid           // action id
	authorized     bool           // is successful logined to AMI
}

// NewAsterisk, Asterisk factory
func NewAsterisk(address, login, password string) *Asterisk {
	return &Asterisk{
		address:  address,
		login:    login,
		password: password,
		actionHandlers: &cbList{
			&sync.RWMutex{},
			make(map[string]*func(Message)),
			make(map[string]bool),
		},
		eventHandlers: &cbList{
			&sync.RWMutex{},
			make(map[string]*func(Message)),
			make(map[string]bool),
		},
		aid: NewAid(),
	}
}

// send, send Message to socket
func (a *Asterisk) send(m Message) error {

	buf := bytes.NewBufferString("")

	for k, v := range m {
		buf.Write([]byte(k))
		buf.Write([]byte(_KEY_VAL_TERM))
		buf.Write([]byte(v))
		buf.Write([]byte(_LINE_TERM))
	}
	buf.Write([]byte(_LINE_TERM))

	if wrb, err := (*a.conn).Write(buf.Bytes()); wrb != buf.Len() || err != nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("Not fully writed packet to output stream\n")
	}

	return nil
}

func (a *Asterisk) read(pbuf *bytes.Buffer, buf *[]byte) error {
	rc, err := (*a.conn).Read(*buf)
	if err != nil { // network error
		return err
	}

	wb, err := pbuf.Write((*buf)[:rc])

	if err != nil || wb != rc { // can't write to data buffer, just skip
		return nil
	}

	// while has end of packet symbols in buffer
	initial := bytes.Index(pbuf.Bytes(), _PT_BYTES)
	for pos := initial; pos != -1; pos = bytes.Index(pbuf.Bytes(), _PT_BYTES) {
		bp := make([]byte, pos+len(_PT_BYTES))
		r, err := pbuf.Read(bp)                    // reading packet to separate puffer
		if err != nil || r != pos+len(_PT_BYTES) { // reading problems, just skip
			continue
		}

		m := make(Message)

		// splitting packet by line separator
		for _, line := range bytes.Split(bp, []byte(_LINE_TERM)) {
			// empty line
			if len(line) == 0 {
				continue
			}
			kvl := bytes.SplitN(line, []byte(_KEY_VAL_TERM), 2)

			// not standard header
			if len(kvl) == 1 {
				if string(line) != _CMD_END {
					m["CmdData"] += string(line)
				}
				continue
			}

			k := bytes.TrimSpace(kvl[0])
			v := bytes.TrimSpace(kvl[1])
			m[string(k)] = string(v)
		}
		// if has ActionID and has callback run it and delete
		if v, vok := m["ActionID"]; vok {
			if f, sd := a.actionHandlers.get(v); f != nil {
				go (*f)(m)
				if !sd { // will never remove "self-delete" callbacks
					a.actionHandlers.del(v)
				}
			}
		}

		// if Event and has callback run it
		if v, vok := m["Event"]; vok {
			if f, _ := a.eventHandlers.get(v); f != nil {
				go (*f)(m)
			}
		}

		// run default handler if not nil
		if a.defaultHandler != nil {
			go (*a.defaultHandler)(m)
		}
	}
	buf = nil
	return nil
}

// readDispatcher, reads data from socket and builds messages
func (a *Asterisk) readDispatcher(finishChann <-chan struct{}) {
	pbuf := bytes.NewBufferString("") // data buffer
	buf := make([]byte, _READ_BUF)    // read buffer
	for {
		select {
		case <-finishChann:
			log.Println("Finalizing ami read events")
			a.conn.Close()
			return
		default:
			if err := a.read(pbuf, &buf); err != nil {
				log.Println("Error reading from socket:", err)
				a.authorized = false        // unauth
				if a.netErrHandler != nil { // run network error callback
					(*a.netErrHandler)(err)
				}
				return
			}
		}
	}
}
