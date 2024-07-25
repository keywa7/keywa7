package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/jessevdk/go-flags"
	"io"
	"keywa7/shared"
	"net"
	"os"
	"slices"
)

type Session struct {
	DestConn     *net.TCPConn
	DestBuffer   bytes.Buffer
	AgentSession shared.AgentSession
	IsDestAlive  bool
	LastActive   int64
}

var SESSIONS []*Session

const MODE = "SERVER"

var OPTIONS struct {
	VERBOSE bool `short:"v" long:"verbose" description:"Show verbose debug information"`
	QUITE   bool `short:"q" long:"quite" description:"Quite mode. [overrides verbose]"`

	LHOST string `long:"lhost" description:"Local address to listen for connections" required:"true"`
	LPORT int    `long:"lport" description:"Local port to listen for connections" required:"true"`
	PAD   int    `long:"pad" description:"Bytes to transfer per packet. [default: 3500] [WARNING: HIGH VALUE WILL TRIGGER CISCO TO BLOCK THE PACKET]"`
}

func validateArgs() bool {
	if net.ParseIP(OPTIONS.LHOST) == nil {
		shared.LogStatus(MODE, "Invalid LHOST", true, false)
		return false
	}

	if OPTIONS.LPORT < 0 || OPTIONS.LPORT > 65535 {
		shared.LogStatus(MODE, "Invalid LPORT", true, false)
		return false
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

func handleRequest(agentConn *net.TCPConn) {
	defer func() {
		if agentConn != nil {
			if err := agentConn.Close(); err != nil && !shared.IsErrForceClose(err) {
				shared.LogStatus(MODE, err.Error(), true, false)
			}
		}
	}()
	session, err := handleSession(agentConn)
	if err != nil {
		shared.LogStatus(MODE, fmt.Sprintf("error handling session: %v", err), true, false)
		return
	}
	shared.LogStatus(MODE, fmt.Sprintf("accepted new agent connection"), false, false)

	_, err = agentConn.Write([]byte{shared.SessionCommandAck})
	if err != nil {
		shared.LogStatus(MODE, fmt.Sprintf("error sending session command ack: %v", err), true, false)
		return
	}
	shared.LogStatus(MODE, fmt.Sprintf("retuned session command ack"), false, true)

	if session == nil {
		return
	}

	shared.DataExchangeReceiver(agentConn, session.DestConn, MODE)
	shared.DataExchangeSender(&session.DestBuffer, agentConn, MODE, OPTIONS.PAD)
}

func handleSession(agentConn *net.TCPConn) (*Session, error) {
	sessionDataLen := make([]byte, 1)
	if _, err := io.ReadFull(agentConn, sessionDataLen); err != nil {
		return nil, fmt.Errorf("error reading session data length: %v", err)
	}

	sessionData := make([]byte, int(sessionDataLen[0]))
	if _, err := io.ReadFull(agentConn, sessionData); err != nil {
		return nil, fmt.Errorf("error reading session data: %v", err)
	}

	agentSession, err := unloadSession(sessionData)
	if err != nil {
		return nil, fmt.Errorf("error unloading session data: %v", err)
	}

	switch agentSession.Command {
	case shared.SessCommandOpen:
		tunnelConn, err := openTunnel(agentSession.DestAddr, agentSession.DestPort)
		if err != nil {
			return nil, fmt.Errorf("error opening tunnel: %v", err)
		}
		var newSession Session
		newSession.AgentSession = agentSession
		newSession.DestConn = tunnelConn
		go shared.ConnectionEndReader(newSession.DestConn, &newSession.DestBuffer, &newSession.IsDestAlive, MODE)
		SESSIONS = append(SESSIONS, &newSession)
		return nil, nil
	case shared.SessCommandCont:
		oldSession := lookupSession(agentSession.SessId)
		if oldSession != nil && oldSession.DestConn == nil {
			return nil, fmt.Errorf("error retrieving old connection for the session")
		}
		return oldSession, nil
	case shared.SessCommandClose:
		oldSession := lookupSession(agentSession.SessId)
		if oldSession != nil {
			if oldSession.DestConn != nil {
				if err = oldSession.DestConn.Close(); err != nil && !shared.IsErrForceClose(err) {
					shared.LogStatus(MODE, err.Error(), true, false)
				}
			}
		}
		removeSession(agentSession.SessId)
	}
	return nil, nil
}

func unloadSession(sessionData []byte) (shared.AgentSession, error) {
	var newSession shared.AgentSession
	err := json.Unmarshal(sessionData, &newSession)
	if err != nil {
		return shared.AgentSession{}, err
	}
	return newSession, nil
}

func openTunnel(ip string, port int) (*net.TCPConn, error) {
	destConn, err := net.DialTCP("tcp", nil, &net.TCPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
	if err != nil {
		return nil, err
	}
	return destConn, nil
}

func removeSession(sessId string) {
	for i, session := range SESSIONS {
		if session.AgentSession.SessId == sessId {
			SESSIONS = append(SESSIONS[:i], SESSIONS[i+1:]...)
			return
		}
	}
}

func lookupSession(sessId string) *Session {
	for _, eachSession := range SESSIONS {
		if eachSession.AgentSession.SessId == sessId {
			return eachSession
		}
	}
	return nil
}
