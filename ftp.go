// Package ftp implements a FTP client as described in RFC 959.
//
// A textproto.Error is returned for errors at the protocol level.
package ftp

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// EntryType describes the different types of an Entry.
type EntryType int

// The differents types of an Entry
const (
	EntryTypeFile EntryType = iota
	EntryTypeFolder
	EntryTypeLink
)

// ServerConn represents the connection to a remote FTP server.
// A single connection only supports one in-flight data connection.
// It is not safe to be called concurrently.
type ServerConn struct {
	options *dialOptions
	conn    *textproto.Conn
	host    string

	// Server capabilities discovered at runtime
	features      map[string]string
	skipEPSV      bool
	mlstSupported bool
	usePRET       bool
}

// DialOption represents an option to start a new connection with Dial
type DialOption struct {
	setup func(do *dialOptions)
}

// dialOptions contains all the options set by DialOption.setup
type dialOptions struct {
	context     context.Context
	dialer      net.Dialer
	tlsConfig   *tls.Config
	explicitTLS bool
	conn        net.Conn
	disableEPSV bool
	disableUTF8 bool
	disableMLSD bool
	location    *time.Location
	debugOutput io.Writer
	dialFunc    func(network, address string) (net.Conn, error)
}

// Entry describes a file and is returned by List().
type Entry struct {
	Name   string
	Target string // target of symbolic link
	Type   EntryType
	Size   uint64
	Time   time.Time
}

// Response represents a data-connection
type Response struct {
	conn   net.Conn
	c      *ServerConn
	closed bool
}

// Dial connects to the specified address with optional options
func Dial(addr string, options ...DialOption) (*ServerConn, error) {
	do := &dialOptions{}
	for _, option := range options {
		option.setup(do)
	}

	if do.location == nil {
		do.location = time.UTC
	}

	tconn := do.conn
	if tconn == nil {
		var err error

		if do.dialFunc != nil {
			tconn, err = do.dialFunc("tcp", addr)
		} else if do.tlsConfig != nil && !do.explicitTLS {
			tconn, err = tls.DialWithDialer(&do.dialer, "tcp", addr, do.tlsConfig)
		} else {
			ctx := do.context

			if ctx == nil {
				ctx = context.Background()
			}

			tconn, err = do.dialer.DialContext(ctx, "tcp", addr)
		}

		if err != nil {
			return nil, err
		}
	}

	// Use the resolved IP address in case addr contains a domain name
	// If we use the domain name, we might not resolve to the same IP.
	remoteAddr := tconn.RemoteAddr().(*net.TCPAddr)

	c := &ServerConn{
		options:  do,
		features: make(map[string]string),
		conn:     textproto.NewConn(do.wrapConn(tconn)),
		host:     remoteAddr.IP.String(),
	}

	_, _, err := c.conn.ReadResponse(StatusReady)
	if err != nil {
		_ = c.Quit()
		return nil, err
	}

	if do.explicitTLS {
		if err := c.authTLS(); err != nil {
			_ = c.Quit()
			return nil, err
		}
		tconn = tls.Client(tconn, do.tlsConfig)
		c.conn = textproto.NewConn(do.wrapConn(tconn))
	}

	return c, nil
}

// DialWithTimeout returns a DialOption that configures the ServerConn with specified timeout
func DialWithTimeout(timeout time.Duration) DialOption {
	return DialOption{func(do *dialOptions) {
		do.dialer.Timeout = timeout
	}}
}

// DialWithDialer returns a DialOption that configures the ServerConn with specified net.Dialer
func DialWithDialer(dialer net.Dialer) DialOption {
	return DialOption{func(do *dialOptions) {
		do.dialer = dialer
	}}
}

// DialWithNetConn returns a DialOption that configures the ServerConn with the underlying net.Conn
func DialWithNetConn(conn net.Conn) DialOption {
	return DialOption{func(do *dialOptions) {
		do.conn = conn
	}}
}

// DialWithDisabledEPSV returns a DialOption that configures the ServerConn with EPSV disabled
// Note that EPSV is only used when advertised in the server features.
func DialWithDisabledEPSV(disabled bool) DialOption {
	return DialOption{func(do *dialOptions) {
		do.disableEPSV = disabled
	}}
}

// DialWithDisabledUTF8 returns a DialOption that configures the ServerConn with UTF8 option disabled
func DialWithDisabledUTF8(disabled bool) DialOption {
	return DialOption{func(do *dialOptions) {
		do.disableUTF8 = disabled
	}}
}

// DialWithDisabledMLSD returns a DialOption that configures the ServerConn with MLSD option disabled
//
// This is useful for servers which advertise MLSD (eg some versions
// of Serv-U) but don't support it properly.
func DialWithDisabledMLSD(disabled bool) DialOption {
	return DialOption{func(do *dialOptions) {
		do.disableMLSD = disabled
	}}
}

// DialWithLocation returns a DialOption that configures the ServerConn with specified time.Location
// The location is used to parse the dates sent by the server which are in server's timezone
func DialWithLocation(location *time.Location) DialOption {
	return DialOption{func(do *dialOptions) {
		do.location = location
	}}
}

// DialWithContext returns a DialOption that configures the ServerConn with specified context
// The context will be used for the initial connection setup
func DialWithContext(ctx context.Context) DialOption {
	return DialOption{func(do *dialOptions) {
		do.context = ctx
	}}
}

// DialWithTLS returns a DialOption that configures the ServerConn with specified TLS config
//
// If called together with the DialWithDialFunc option, the DialWithDialFunc function
// will be used when dialing new connections but regardless of the function,
// the connection will be treated as a TLS connection.
func DialWithTLS(tlsConfig *tls.Config) DialOption {
	return DialOption{func(do *dialOptions) {
		do.tlsConfig = tlsConfig
	}}
}

// DialWithExplicitTLS returns a DialOption that configures the ServerConn to be upgraded to TLS
// See DialWithTLS for general TLS documentation
func DialWithExplicitTLS(tlsConfig *tls.Config) DialOption {
	return DialOption{func(do *dialOptions) {
		do.explicitTLS = true
		do.tlsConfig = tlsConfig
	}}
}

// DialWithDebugOutput returns a DialOption that configures the ServerConn to write to the Writer
// everything it reads from the server
func DialWithDebugOutput(w io.Writer) DialOption {
	return DialOption{func(do *dialOptions) {
		do.debugOutput = w
	}}
}

// DialWithDialFunc returns a DialOption that configures the ServerConn to use the
// specified function to establish both control and data connections
//
// If used together with the DialWithNetConn option, the DialWithNetConn
// takes precedence for the control connection, while data connections will
// be established using function specified with the DialWithDialFunc option
func DialWithDialFunc(f func(network, address string) (net.Conn, error)) DialOption {
	return DialOption{func(do *dialOptions) {
		do.dialFunc = f
	}}
}

func (o *dialOptions) wrapConn(netConn net.Conn) io.ReadWriteCloser {
	if o.debugOutput == nil {
		return netConn
	}

	return newDebugWrapper(netConn, o.debugOutput)
}

// Connect is an alias to Dial, for backward compatibility
func Connect(addr string) (*ServerConn, error) {
	return Dial(addr)
}

// DialTimeout initializes the connection to the specified ftp server address.
//
// It is generally followed by a call to Login() as most FTP commands require
// an authenticated user.
func DialTimeout(addr string, timeout time.Duration) (*ServerConn, error) {
	return Dial(addr, DialWithTimeout(timeout))
}

func (c *ServerConn) Auth(user, password string) (code int, err error) {
	code, _, err = c.cmd(-1, "USER %s", user)
	if err != nil {
		return 0, err
	}

	switch code {
	case StatusLoggedIn:
		return code, nil
	case StatusUserOK:
		code, _, err = c.cmd(-1, "PASS %s", password)
		if err != nil {
			return 0, err
		}
		return code, nil
	default:
		return code, nil
	}
}

func (c *ServerConn) AfterAuth() error {
	// Probe features
	err := c.feat()
	if err != nil {
		return err
	}
	if _, mlstSupported := c.features["MLST"]; mlstSupported && !c.options.disableMLSD {
		c.mlstSupported = true
	}
	if _, usePRET := c.features["PRET"]; usePRET {
		c.usePRET = true
	}

	// Switch to binary mode
	if _, _, err = c.cmd(StatusCommandOK, "TYPE I"); err != nil {
		return err
	}

	// Switch to UTF-8
	if !c.options.disableUTF8 {
		err = c.setUTF8()
	}

	// If using implicit TLS, make data connections also use TLS
	if c.options.tlsConfig != nil {
		if _, _, err = c.cmd(StatusCommandOK, "PBSZ 0"); err != nil {
			return err
		}
		if _, _, err = c.cmd(StatusCommandOK, "PROT P"); err != nil {
			return err
		}
	}

	return err
}

// authTLS upgrades the connection to use TLS
func (c *ServerConn) authTLS() error {
	_, _, err := c.cmd(StatusAuthOK, "AUTH TLS")
	return err
}

// feat issues a FEAT FTP command to list the additional commands supported by
// the remote FTP server.
// FEAT is described in RFC 2389
func (c *ServerConn) feat() error {
	code, message, err := c.cmd(-1, "FEAT")
	if err != nil {
		return err
	}

	if code != StatusSystem {
		// The server does not support the FEAT command. This is not an
		// error: we consider that there is no additional feature.
		return nil
	}

	lines := strings.Split(message, "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, " ") {
			continue
		}

		line = strings.TrimSpace(line)
		featureElements := strings.SplitN(line, " ", 2)

		command := featureElements[0]

		var commandDesc string
		if len(featureElements) == 2 {
			commandDesc = featureElements[1]
		}

		c.features[command] = commandDesc
	}

	return nil
}

// setUTF8 issues an "OPTS UTF8 ON" command.
func (c *ServerConn) setUTF8() error {
	if _, ok := c.features["UTF8"]; !ok {
		return nil
	}

	code, message, err := c.cmd(-1, "OPTS UTF8 ON")
	if err != nil {
		return err
	}

	// Workaround for FTP servers, that does not support this option.
	if code == StatusBadArguments || code == StatusNotImplementedParameter {
		return nil
	}

	// The ftpd "filezilla-server" has FEAT support for UTF8, but always returns
	// "202 UTF8 mode is always enabled. No need to send this command." when
	// trying to use it. That's OK
	if code == StatusCommandNotImplemented {
		return nil
	}

	if code != StatusCommandOK {
		return errors.New(message)
	}

	return nil
}

// epsv issues an "EPSV" command to get a port number for a data connection.
func (c *ServerConn) epsv() (port int, err error) {
	_, line, err := c.cmd(StatusExtendedPassiveMode, "EPSV")
	if err != nil {
		return 0, err
	}

	start := strings.Index(line, "|||")
	end := strings.LastIndex(line, "|")
	if start == -1 || end == -1 {
		return 0, errors.New("invalid EPSV response format")
	}
	port, err = strconv.Atoi(line[start+3 : end])
	return port, err
}

// pasv issues a "PASV" command to get a port number for a data connection.
func (c *ServerConn) pasv() (host string, port int, err error) {
	_, line, err := c.cmd(StatusPassiveMode, "PASV")
	if err != nil {
		return "", 0, err
	}

	// PASV response format : 227 Entering Passive Mode (h1,h2,h3,h4,p1,p2).
	start := strings.Index(line, "(")
	end := strings.LastIndex(line, ")")
	if start == -1 || end == -1 {
		return "", 0, errors.New("invalid PASV response format")
	}

	// We have to split the response string
	pasvData := strings.Split(line[start+1:end], ",")

	if len(pasvData) < 6 {
		return "", 0, errors.New("invalid PASV response format")
	}

	// Let's compute the port number
	portPart1, err := strconv.Atoi(pasvData[4])
	if err != nil {
		return "", 0, err
	}

	portPart2, err := strconv.Atoi(pasvData[5])
	if err != nil {
		return "", 0, err
	}

	// Recompose port
	port = portPart1*256 + portPart2

	// Make the IP address to connect to
	host = strings.Join(pasvData[0:4], ".")
	return host, port, nil
}

// getDataConnPort returns a host, port for a new data connection
// it uses the best available method to do so
func (c *ServerConn) getDataConnPort() (string, int, error) {
	if !c.options.disableEPSV && !c.skipEPSV {
		if port, err := c.epsv(); err == nil {
			return c.host, port, nil
		}

		// if there is an error, skip EPSV for the next attempts
		c.skipEPSV = true
	}

	return c.pasv()
}

// openDataConn creates a new FTP data connection.
func (c *ServerConn) openDataConn() (net.Conn, error) {
	host, port, err := c.getDataConnPort()
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	if c.options.dialFunc != nil {
		return c.options.dialFunc("tcp", addr)
	}

	if c.options.tlsConfig != nil {
		conn, err := c.options.dialer.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return tls.Client(conn, c.options.tlsConfig), err
	}

	return c.options.dialer.Dial("tcp", addr)
}

// cmd is a helper function to execute a command and check for the expected FTP
// return code
func (c *ServerConn) cmd(expected int, format string, args ...interface{}) (int, string, error) {
	_, err := c.conn.Cmd(format, args...)
	if err != nil {
		return 0, "", err
	}

	return c.conn.ReadResponse(expected)
}

// cmdDataConnFrom executes a command which require a FTP data connection.
// Issues a REST FTP command to specify the number of bytes to skip for the transfer.
func (c *ServerConn) cmdDataConnFrom(offset uint64, format string, args ...interface{}) (net.Conn, error) {
	// If server requires PRET send the PRET command to warm it up
	// See: https://tools.ietf.org/html/draft-dd-pret-00
	if c.usePRET {
		_, _, err := c.cmd(-1, "PRET "+format, args...)
		if err != nil {
			return nil, err
		}
	}

	conn, err := c.openDataConn()
	if err != nil {
		return nil, err
	}

	if offset != 0 {
		_, _, err = c.cmd(StatusRequestFilePending, "REST %d", offset)
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
	}

	_, err = c.conn.Cmd(format, args...)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	code, msg, err := c.conn.ReadResponse(-1)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if code != StatusAlreadyOpen && code != StatusAboutToSend {
		_ = conn.Close()
		return nil, &textproto.Error{Code: code, Msg: msg}
	}

	return conn, nil
}

// NameList issues an NLST FTP command.
func (c *ServerConn) NameList(path string) (entries []string, err error) {
	space := " "
	if path == "" {
		space = ""
	}
	conn, err := c.cmdDataConnFrom(0, "NLST%s%s", space, path)
	if err != nil {
		return nil, err
	}

	r := &Response{conn: conn, c: c}
	defer func() {
		errClose := r.Close()
		if err == nil {
			err = errClose
		}
	}()

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		entries = append(entries, scanner.Text())
	}

	err = scanner.Err()
	return entries, err
}

// List issues a LIST FTP command.
func (c *ServerConn) List(path string) (entries []*Entry, err error) {
	var cmd string
	var parser parseFunc

	if c.mlstSupported {
		cmd = "MLSD"
		parser = parseRFC3659ListLine
	} else {
		cmd = "LIST"
		parser = parseListLine
	}

	space := " "
	if path == "" {
		space = ""
	}
	conn, err := c.cmdDataConnFrom(0, "%s%s%s", cmd, space, path)
	if err != nil {
		return nil, err
	}

	r := &Response{conn: conn, c: c}
	defer func() {
		errClose := r.Close()
		if err == nil {
			err = errClose
		}
	}()

	scanner := bufio.NewScanner(r)
	now := time.Now()
	for scanner.Scan() {
		entry, errParse := parser(scanner.Text(), now, c.options.location)
		if errParse == nil {
			entries = append(entries, entry)
		}
	}

	err = scanner.Err()
	return entries, err
}

// ChangeDir issues a CWD FTP command, which changes the current directory to
// the specified path.
func (c *ServerConn) ChangeDir(path string) error {
	_, _, err := c.cmd(StatusRequestedFileActionOK, "CWD %s", path)
	return err
}

// ChangeDirToParent issues a CDUP FTP command, which changes the current
// directory to the parent directory.  This is similar to a call to ChangeDir
// with a path set to "..".
func (c *ServerConn) ChangeDirToParent() error {
	_, _, err := c.cmd(StatusRequestedFileActionOK, "CDUP")
	return err
}

// CurrentDir issues a PWD FTP command, which Returns the path of the current
// directory.
func (c *ServerConn) CurrentDir() (string, error) {
	_, msg, err := c.cmd(StatusPathCreated, "PWD")
	if err != nil {
		return "", err
	}

	start := strings.Index(msg, "\"")
	end := strings.LastIndex(msg, "\"")

	if start == -1 || end == -1 {
		return "", errors.New("unsuported PWD response format")
	}

	return msg[start+1 : end], nil
}

// FileSize issues a SIZE FTP command, which Returns the size of the file
func (c *ServerConn) FileSize(path string) (int64, error) {
	_, msg, err := c.cmd(StatusFile, "SIZE %s", path)
	if err != nil {
		return 0, err
	}

	return strconv.ParseInt(msg, 10, 64)
}

// Retr issues a RETR FTP command to fetch the specified file from the remote
// FTP server.
//
// The returned ReadCloser must be closed to cleanup the FTP data connection.
func (c *ServerConn) Retr(path string) (*Response, error) {
	return c.RetrFrom(path, 0)
}

// RetrFrom issues a RETR FTP command to fetch the specified file from the remote
// FTP server, the server will not send the offset first bytes of the file.
//
// The returned ReadCloser must be closed to cleanup the FTP data connection.
func (c *ServerConn) RetrFrom(path string, offset uint64) (*Response, error) {
	conn, err := c.cmdDataConnFrom(offset, "RETR %s", path)
	if err != nil {
		return nil, err
	}

	return &Response{conn: conn, c: c}, nil
}

// Stor issues a STOR FTP command to store a file to the remote FTP server.
// Stor creates the specified file with the content of the io.Reader.
//
// Hint: io.Pipe() can be used if an io.Writer is required.
func (c *ServerConn) Stor(path string, r io.Reader) error {
	return c.StorFrom(path, r, 0)
}

// StorFrom issues a STOR FTP command to store a file to the remote FTP server.
// Stor creates the specified file with the content of the io.Reader, writing
// on the server will start at the given file offset.
//
// Hint: io.Pipe() can be used if an io.Writer is required.
func (c *ServerConn) StorFrom(path string, r io.Reader, offset uint64) error {
	conn, err := c.cmdDataConnFrom(offset, "STOR %s", path)
	if err != nil {
		return err
	}

	// if the upload fails we still need to try to read the server
	// response otherwise if the failure is not due to a connection problem,
	// for example the server denied the upload for quota limits, we miss
	// the response and we cannot use the connection to send other commands.
	// So we don't check io.Copy error and we return the error from
	// ReadResponse so the user can see the real error
	var n int64
	n, err = io.Copy(conn, r)

	// If we wrote no bytes but got no error, make sure we call
	// tls.Handshake on the connection as it won't get called
	// unless Write() is called.
	//
	// ProFTP doesn't like this and returns "Unable to build data
	// connection: Operation not permitted" when trying to upload
	// an empty file without this.
	if n == 0 && err == nil {
		if do, ok := conn.(interface{ Handshake() error }); ok {
			err = do.Handshake()
		}
	}

	// Use io.Copy or Handshake error in preference to this one
	closeErr := conn.Close()
	if err == nil {
		err = closeErr
	}

	// Read the response and use this error in preference to
	// previous errors
	_, _, respErr := c.conn.ReadResponse(StatusClosingDataConnection)
	if respErr != nil {
		err = respErr
	}
	return err
}

// Append issues a APPE FTP command to store a file to the remote FTP server.
// If a file already exists with the given path, then the content of the
// io.Reader is appended. Otherwise, a new file is created with that content.
//
// Hint: io.Pipe() can be used if an io.Writer is required.
func (c *ServerConn) Append(path string, r io.Reader) error {
	conn, err := c.cmdDataConnFrom(0, "APPE %s", path)
	if err != nil {
		return err
	}

	// see the comment for StorFrom above
	_, err = io.Copy(conn, r)
	errClose := conn.Close()

	_, _, respErr := c.conn.ReadResponse(StatusClosingDataConnection)
	if respErr != nil {
		err = respErr
	}

	if err == nil {
		err = errClose
	}

	return err
}

// Rename renames a file on the remote FTP server.
func (c *ServerConn) Rename(from, to string) error {
	_, _, err := c.cmd(StatusRequestFilePending, "RNFR %s", from)
	if err != nil {
		return err
	}

	_, _, err = c.cmd(StatusRequestedFileActionOK, "RNTO %s", to)
	return err
}

// Delete issues a DELE FTP command to delete the specified file from the
// remote FTP server.
func (c *ServerConn) Delete(path string) error {
	_, _, err := c.cmd(StatusRequestedFileActionOK, "DELE %s", path)
	return err
}

// RemoveDirRecur deletes a non-empty folder recursively using
// RemoveDir and Delete
func (c *ServerConn) RemoveDirRecur(path string) error {
	err := c.ChangeDir(path)
	if err != nil {
		return err
	}
	currentDir, err := c.CurrentDir()
	if err != nil {
		return err
	}

	entries, err := c.List(currentDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.Name != ".." && entry.Name != "." {
			if entry.Type == EntryTypeFolder {
				err = c.RemoveDirRecur(currentDir + "/" + entry.Name)
				if err != nil {
					return err
				}
			} else {
				err = c.Delete(entry.Name)
				if err != nil {
					return err
				}
			}
		}
	}
	err = c.ChangeDirToParent()
	if err != nil {
		return err
	}
	err = c.RemoveDir(currentDir)
	return err
}

// MakeDir issues a MKD FTP command to create the specified directory on the
// remote FTP server.
func (c *ServerConn) MakeDir(path string) error {
	_, _, err := c.cmd(StatusPathCreated, "MKD %s", path)
	return err
}

// RemoveDir issues a RMD FTP command to remove the specified directory from
// the remote FTP server.
func (c *ServerConn) RemoveDir(path string) error {
	_, _, err := c.cmd(StatusRequestedFileActionOK, "RMD %s", path)
	return err
}

//Walk prepares the internal walk function so that the caller can begin traversing the directory
func (c *ServerConn) Walk(root string) *Walker {
	w := new(Walker)
	w.serverConn = c

	if !strings.HasSuffix(root, "/") {
		root += "/"
	}

	w.root = root
	w.descend = true

	return w
}

// NoOp issues a NOOP FTP command.
// NOOP has no effects and is usually used to prevent the remote FTP server to
// close the otherwise idle connection.
func (c *ServerConn) NoOp() error {
	_, _, err := c.cmd(StatusCommandOK, "NOOP")
	return err
}

// Logout issues a REIN FTP command to logout the current user.
func (c *ServerConn) Logout() error {
	_, _, err := c.cmd(StatusReady, "REIN")
	return err
}

// Quit issues a QUIT FTP command to properly close the connection from the
// remote FTP server.
func (c *ServerConn) Quit() error {
	_, errQuit := c.conn.Cmd("QUIT")
	err := c.conn.Close()

	if errQuit != nil {
		if err != nil {
			return fmt.Errorf("error while quitting: %s: %w", errQuit, err)
		}
		return errQuit
	}

	return err
}

// Read implements the io.Reader interface on a FTP data connection.
func (r *Response) Read(buf []byte) (int, error) {
	return r.conn.Read(buf)
}

// Close implements the io.Closer interface on a FTP data connection.
// After the first call, Close will do nothing and return nil.
func (r *Response) Close() error {
	if r.closed {
		return nil
	}
	err := r.conn.Close()
	_, _, err2 := r.c.conn.ReadResponse(StatusClosingDataConnection)
	if err2 != nil {
		err = err2
	}
	r.closed = true
	return err
}

// SetDeadline sets the deadlines associated with the connection.
func (r *Response) SetDeadline(t time.Time) error {
	return r.conn.SetDeadline(t)
}

// String returns the string representation of EntryType t.
func (t EntryType) String() string {
	return [...]string{"file", "folder", "link"}[t]
}
