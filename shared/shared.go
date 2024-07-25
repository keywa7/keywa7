package shared

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
)

const SessCommandOpen = 0x01
const SessCommandCont = 0x02
const SessCommandClose = 0x03

const DataLocalPad = 512
const SessionCommandAck = 0x69
const NoData = 0x96

const DefaultDataTransferPad = 3500

var VerboseOutput bool
var QuiteMode bool

type AgentSession struct {
	Command  int
	DestAddr string
	DestPort int
	SessId   string
}

func LogStatus(modeName string, text string, isBad bool, isVerbose bool) {
	if QuiteMode || isVerbose && !VerboseOutput {
		return
	}
	status := "output"
	if isBad {
		status = "error"
	}
	var callerName = "UnknownFunction"
	if info, _, _, ok := runtime.Caller(1); ok {
		details := runtime.FuncForPC(info)
		if details != nil {
			callerName = details.Name()
		}
	}
	callerNameSplit := strings.Split(callerName, ".")
	mode := fmt.Sprintf("[%s]", modeName)
	prefix := fmt.Sprintf("[%s]", "+")
	if isBad {
		prefix = fmt.Sprintf("[%s]", "-")
	}
	log := fmt.Sprintf("%s %s %s %s: %s", mode, prefix, status, callerNameSplit[len(callerNameSplit)-1], text)
	fmt.Println(log)
}

func ReadConnection(conn net.Conn) ([]byte, error) {
	var result []byte
	buffer := make([]byte, DataLocalPad)

	for {
		n, err := conn.Read(buffer)
		result = append(result, buffer[:n]...)
		if err != nil {
			return result, err
		}
		if n < len(buffer) {
			break
		}
	}

	return result, nil
}

func ConnectionEndReader(conn *net.TCPConn, readBuffer *bytes.Buffer, sourceStatus *bool, mode string) {
	for {
		data, err := ReadConnection(conn)
		if err != nil {
			if err != io.EOF && !IsErrForceClose(err) {
				LogStatus(mode, fmt.Sprintf("failed to read client connection: %v", err), true, false)
			}
			*sourceStatus = false
			return
		}
		if len(data) > 0 {
			n, _ := readBuffer.Write(data)
			LogStatus(mode, fmt.Sprintf("bytes read from %s %s: %d", lookupEnd(mode), conn.RemoteAddr().String(), n), false, true)
		}
	}
}

func DataExchangeSender(sourceBuffer *bytes.Buffer, destConn *net.TCPConn, mode string, pad int) {
	sourceBytes := sourceBuffer.Bytes()
	if len(sourceBytes) > pad {
		sourceBytes = sourceBytes[:pad]
	}
	if len(sourceBytes) == 0 {
		noData := []byte{NoData}
		_, err := destConn.Write(noData)
		if err != nil {
			LogStatus(mode, fmt.Sprintf("failed to send NoData to %s: %v", lookupTunnel(mode), err), true, false)
			return
		}
	} else {
		dataLen := len(sourceBytes)
		dataLenBytes := make([]byte, 8)
		binary.LittleEndian.PutUint64(dataLenBytes, uint64(dataLen))
		transferData := append(dataLenBytes, sourceBytes...)
		_, err := destConn.Write(transferData)
		if err != nil {
			LogStatus(mode, fmt.Sprintf("failed to forward data to %s: %v", lookupTunnel(mode), err), true, false)
			return
		}
		LogStatus(mode, fmt.Sprintf("bytes forwarded to %s: %d", lookupTunnel(mode), dataLen), false, true)
		sourceBuffer.Next(dataLen)
	}
}

func DataExchangeReceiver(sourceConn *net.TCPConn, destConn *net.TCPConn, mode string) {
	serverResponse, err := ReadConnection(sourceConn)
	if err != nil {
		if !IsErrForceClose(err) && err != io.EOF {
			LogStatus(mode, fmt.Sprintf("failed to read %s response: %v", lookupTunnel(mode), err), true, false)
		}
		return
	}
	if !(len(serverResponse) == 1 && serverResponse[0] == NoData) {
		if len(serverResponse) < 8 {
			return
		}
		dataLenBytes := serverResponse[:8]
		dataLen := int(binary.LittleEndian.Uint64(dataLenBytes))
		transferData := serverResponse[8:]
		for {
			if len(transferData) != dataLen {
				newResponseDataChunk, err := ReadConnection(sourceConn)
				if err != nil {
					if err != io.EOF {
						LogStatus(mode, fmt.Sprintf("failed to read new %s response chunk: %v", lookupTunnel(mode), err), true, false)
					}
					break
				}
				transferData = append(transferData, newResponseDataChunk...)
			} else {
				break
			}
		}
		LogStatus(mode, fmt.Sprintf("response bytes received from %s: %d", lookupTunnel(mode), len(transferData)), false, true)
		n, err := destConn.Write(transferData)
		if err != nil {
			LogStatus(mode, fmt.Sprintf("failed to forward response to %s: %v", lookupEnd(mode), err), true, false)
			return
		}
		LogStatus(mode, fmt.Sprintf("response bytes forwarded to %s: %d", lookupEnd(mode), n), false, true)
	}
}

func IsErrForceClose(err error) bool {
	return strings.Contains(err.Error(), "connection was forcibly closed by the remote host.") ||
		strings.Contains(err.Error(), "connection was aborted by the software in your host machine") ||
		strings.Contains(err.Error(), "use of closed network connection")
}

func lookupTunnel(mode string) string {
	if mode == "SERVER" {
		return "agent"
	}
	return "server"
}

func lookupEnd(mode string) string {
	if mode == "SERVER" {
		return "final destination"
	}
	return "client"
}
