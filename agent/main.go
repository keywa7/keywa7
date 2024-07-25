package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jessevdk/go-flags"
	"io"
	"keywa7/shared"
	"net"
	"os"
	"slices"
	"time"
)

const MODE = "AGENT"
const DefaultConnInterval = 100

var OPTIONS struct {
	VERBOSE bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	QUITE   bool `short:"q" long:"quite" description:"Quite mode. [overrides verbose]"`

	LHOST string `long:"lhost" description:"Local address to listen for connections" required:"true"`
	LPORT int    `long:"lport" description:"Local port to listen for connections" required:"true"`

	RHOST string `long:"rhost" description:"Remote address to forward connections to" required:"true"`
	RPORT int    `long:"rport" description:"Remote port to forward connections to" required:"true"`

	INTERVAL int `long:"interval" description:"Time to wait before initiating a new connection for a session [milliseconds] [default: 100] [WARNING: LOW VALUE WILL CAUSE OVERFLOW OF CONNECTIONS ON THE HOST]"`
	PAD      int `long:"pad" description:"Bytes to transfer per packet. [default: 3500] [WARNING: HIGH VALUE WILL TRIGGER CISCO TO BLOCK THE PACKET]"`
}

func validateArgs() bool {
	if net.ParseIP(OPTIONS.LHOST) == nil {
		shared.LogStatus(MODE, "Invalid LHOST", true, false)
		return false
	}
	if net.ParseIP(OPTIONS.RHOST) == nil {
		shared.LogStatus(MODE, "Invalid RHOST", true, false)
		return false
	}

	if OPTIONS.LPORT < 0 || OPTIONS.LPORT > 65535 {
		shared.LogStatus(MODE, "Invalid LPORT", true, false)
		return false
	}
	if OPTIONS.RPORT < 0 || OPTIONS.RPORT > 65535 {
		shared.LogStatus(MODE, "Invalid RPORT", true, false)
		return false
	}

	if OPTIONS.INTERVAL == 0 {
		OPTIONS.INTERVAL = DefaultConnInterval

	}

	if OPTIONS.PAD == 0 {
		OPTIONS.PAD = shared.DefaultDataTransferPad
	}
	return true
}

func main() {
	_, err := flags.ParseArgs(&OPTIONS, os.Args)
	if err != nil {
		if !slices.Contains(os.Args, "-h") && !slices.Contains(os.Args, "--help") {
			fmt.Println("Invalid args. ", fmt.Sprintf("%s -h", os.Args[0]))
		}
		return
	}
	if !validateArgs() {
		return
	}

	shared.VerboseOutput = OPTIONS.VERBOSE
	shared.QuiteMode = OPTIONS.QUITE

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{
		IP:   net.ParseIP(OPTIONS.LHOST),
		Port: OPTIONS.LPORT,
	})
	if err != nil {
		shared.LogStatus(MODE, fmt.Sprintf("failed to set up local listener at %s:%d: %v", OPTIONS.LHOST, OPTIONS.LPORT, err), true, false)
		return
	}
	defer func() {
		err = listener.Close()
		if err != nil {
			shared.LogStatus(MODE, fmt.Sprintf("failed to close listener: %s:%d", OPTIONS.LHOST, OPTIONS.LPORT), true, false)
		}
	}()
	shared.LogStatus(MODE, fmt.Sprintf("started local listener at: %s:%d", OPTIONS.LHOST, OPTIONS.LPORT), false, false)

	for {
		conn, err := listener.AcceptTCP()
		if err != nil {
			shared.LogStatus(MODE, fmt.Sprintf("error accepting connection"), true, false)
			continue
		}
		go handleRequest(conn)
	}
}

func handleRequest(clientConn *net.TCPConn) {
	var isClientAlive = true
	sessionCommandOpen, sessionCommandCont, sessionCommandClose, err := handleNewConn(clientConn)
	if err != nil {
		shared.LogStatus(MODE, fmt.Sprintf("failed to handle new client connection from %s: %v", clientConn.RemoteAddr().String(), err), true, false)
		return
	}
	shared.LogStatus(MODE, fmt.Sprintf("accepted new client connection from %s", clientConn.RemoteAddr().String()), false, false)

	serverConn, err := openTunnel(sessionCommandOpen)
	if err != nil {
		shared.LogStatus(MODE, fmt.Sprintf("failed to open tunnel for the connection: %v", err), true, false)
		return
	}
	shared.LogStatus(MODE, fmt.Sprintf("opened tunnel for the connection"), false, true)

	if _, err = clientConn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}); err != nil {
		shared.LogStatus(MODE, fmt.Sprintf("failed to write success message to client: %v", err), true, false)
		return
	}
	shared.LogStatus(MODE, fmt.Sprintf("wrote success message to client tunnel for the connection"), false, true)

	shared.LogStatus(MODE, fmt.Sprintf("started data transfer"), false, false)

	var clientDataBuffer bytes.Buffer
	go shared.ConnectionEndReader(clientConn, &clientDataBuffer, &isClientAlive, MODE)

	for {
		serverConn, err = reconnect(serverConn)
		if err != nil {
			shared.LogStatus(MODE, fmt.Sprintf("failed to reconnect to server: %v", err), true, false)
			continue
		}
		shared.LogStatus(MODE, fmt.Sprintf("reconnected to server"), false, false)

		if !isClientAlive {
			err = commandSession(serverConn, sessionCommandClose)
			shared.LogStatus(MODE, fmt.Sprintf("sent close command to server"), false, true)
			if err != nil {
				shared.LogStatus(MODE, fmt.Sprintf("failed to send close command to server: %v", err), true, false)
			}
			break
		}

		err = commandSession(serverConn, sessionCommandCont)
		if err != nil {
			shared.LogStatus(MODE, fmt.Sprintf("failed to send continue command to server: %v", err), true, false)
			break
		}
		shared.LogStatus(MODE, fmt.Sprintf("sent continue command to server"), false, true)

		shared.DataExchangeSender(&clientDataBuffer, serverConn, MODE, OPTIONS.PAD)
		shared.DataExchangeReceiver(serverConn, clientConn, MODE)
		time.Sleep(time.Duration(OPTIONS.INTERVAL) * time.Millisecond)
	}
	if serverConn != nil {
		if err = serverConn.Close(); err != nil && !shared.IsErrForceClose(err) {
			shared.LogStatus(MODE, err.Error(), true, false)
		}
	}
	if clientConn != nil {
		if err = clientConn.Close(); err != nil && !shared.IsErrForceClose(err) {
			shared.LogStatus(MODE, err.Error(), true, false)
		}
	}
}

func handleNewConn(clientConn *net.TCPConn) ([]byte, []byte, []byte, error) {
	err := validateSocks(clientConn)
	if err != nil {
		return []byte{}, []byte{}, []byte{}, errors.New(fmt.Sprintf("failed to validate SOCKS5 traffic: %v", err))
	}

	destType := make([]byte, 2)
	if _, err = io.ReadFull(clientConn, destType); err != nil {
		return []byte{}, []byte{}, []byte{}, errors.New("failed to read destination type")
	}

	var destAddr net.IP
	var destPort int
	switch destType[1] {
	case 0x01: // IPv4 address
		ipv4 := make([]byte, 4)
		if _, err = io.ReadFull(clientConn, ipv4); err != nil {
			return []byte{}, []byte{}, []byte{}, fmt.Errorf("error reading IPv4 address: %v", err)
		}
		destAddr = ipv4
	case 0x03: // Domain name
		domainLen := make([]byte, 1)
		if _, err = io.ReadFull(clientConn, domainLen); err != nil {
			return []byte{}, []byte{}, []byte{}, fmt.Errorf("error reading domain length: %v", err)
		}
		domain := make([]byte, domainLen[0])
		if _, err = io.ReadFull(clientConn, domain); err != nil {
			return []byte{}, []byte{}, []byte{}, fmt.Errorf("error reading domain: %v", err)
		}
		resolvedAddr, err := net.ResolveIPAddr("ip", string(domain))
		if err != nil {
			return []byte{}, []byte{}, []byte{}, fmt.Errorf("error resolving domain: %v", err)
		}
		destAddr = resolvedAddr.IP
	case 0x04: // IPv6 address (not supported in this example)
		return []byte{}, []byte{}, []byte{}, fmt.Errorf("IPv6 addresses are not supported")
	default:
		return []byte{}, []byte{}, []byte{}, fmt.Errorf("unsupported address type: %v", destType[1])
	}

	port := make([]byte, 2)
	if _, err = io.ReadFull(clientConn, port); err != nil {
		return []byte{}, []byte{}, []byte{}, fmt.Errorf("error reading destination port: %v", err)
	}
	destPort = int(port[0])<<8 | int(port[1])

	sessId := uuid.New()
	sessionDataOpen, sessionDataCont, sessionDataClose := getSessionCommands(destAddr.String(), destPort, sessId.String())

	sessionDataOpenFull := append([]byte{byte(len(sessionDataOpen))}, sessionDataOpen...)
	sessionDataContFull := append([]byte{byte(len(sessionDataCont))}, sessionDataCont...)
	sessionDataCloseFUll := append([]byte{byte(len(sessionDataClose))}, sessionDataClose...)

	return sessionDataOpenFull, sessionDataContFull, sessionDataCloseFUll, nil
}

func validateSocks(clientConn *net.TCPConn) error {
	version := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, version); err != nil {
		return fmt.Errorf("error reading socks version: %v", err)
	}

	if version[0] != 0x05 {
		return errors.New("unsupported traffic version. only SOCKS5 is supported")
	}

	numMethods := int(version[1])
	authMethods := make([]byte, numMethods)
	if _, err := io.ReadFull(clientConn, authMethods); err != nil {
		return fmt.Errorf("error reading authentication methods: %v", err)
	}

	if _, err := clientConn.Write([]byte{0x05, 0x00}); err != nil {
		return fmt.Errorf("error sending server selection message: %v", err)

	}

	command := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, command); err != nil {
		return fmt.Errorf("error reading client request: %v", err)
	}

	if command[0] != 0x05 || command[1] != 0x01 {
		return fmt.Errorf("unsupported command: %v", command[1])
	}
	return nil
}

func getSessionCommands(addr string, port int, session string) ([]byte, []byte, []byte) {
	var newSession shared.AgentSession
	newSession.Command = shared.SessCommandOpen
	newSession.DestAddr = addr
	newSession.DestPort = port
	newSession.SessId = session
	commandOpen, _ := json.Marshal(newSession)
	newSession.Command = shared.SessCommandCont
	commandCont, _ := json.Marshal(newSession)
	newSession.Command = shared.SessCommandClose
	commandClose, _ := json.Marshal(newSession)
	return commandOpen, commandCont, commandClose
}

func openTunnel(sessionDataOpen []byte) (*net.TCPConn, error) {
	serverConn, err := net.DialTCP("tcp", nil, &net.TCPAddr{
		IP:   net.ParseIP(OPTIONS.RHOST),
		Port: OPTIONS.RPORT,
	})

	if err != nil {
		return nil, fmt.Errorf("error connecting to server: %v", err)
	}
	err = commandSession(serverConn, sessionDataOpen)
	if err != nil {
		return nil, fmt.Errorf("error opening session: %v", err)
	}
	return serverConn, nil
}

func commandSession(serverConn *net.TCPConn, command []byte) error {
	if _, err := serverConn.Write(command); err != nil {
		return fmt.Errorf("error writing command to session %v", err)
	}
	SessionCommandAck := make([]byte, 1)
	if _, err := io.ReadFull(serverConn, SessionCommandAck); err != nil {
		return fmt.Errorf("error reading session command ack: %v", err)
	}
	if SessionCommandAck[0] != shared.SessionCommandAck {
		return fmt.Errorf("no session command ack from server")
	}
	shared.LogStatus(MODE, "session command ack received", false, true)
	return nil
}

func reconnect(conn *net.TCPConn) (*net.TCPConn, error) {
	if conn != nil {
		err := conn.Close()
		if err != nil {
			return nil, fmt.Errorf("error closing server connection: %v", err)
		}
	}
	serverConn, err := net.DialTCP("tcp", nil, &net.TCPAddr{
		IP:   net.ParseIP(OPTIONS.RHOST),
		Port: OPTIONS.RPORT,
	})
	if err != nil {
		return nil, fmt.Errorf("error re-opening server connection: %v", err)
	}

	return serverConn, nil
}
