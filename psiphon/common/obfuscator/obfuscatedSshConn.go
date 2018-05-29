/*
 * Copyright (c) 2015, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package obfuscator

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"

	"github.com/Psiphon-Labs/psiphon-tunnel-core/psiphon/common"
)

const (
	SSH_MAX_SERVER_LINE_LENGTH = 1024
	SSH_PACKET_PREFIX_LENGTH   = 5          // uint32 + byte
	SSH_MAX_PACKET_LENGTH      = 256 * 1024 // OpenSSH max packet length
	SSH_MSG_NEWKEYS            = 21
	SSH_MAX_PADDING_LENGTH     = 255 // RFC 4253 sec. 6
	SSH_PADDING_MULTIPLE       = 16  // Default cipher block size
)

// ObfuscatedSshConn wraps a Conn and applies the obfuscated SSH protocol
// to the traffic on the connection:
// https://github.com/brl/obfuscated-openssh/blob/master/README.obfuscation
//
// ObfuscatedSshConn is used to add obfuscation to golang's stock ssh
// client and server without modification to that standard library code.
// The underlying connection must be used for SSH traffic. This code
// injects the obfuscated seed message, applies obfuscated stream cipher
// transformations, and performs minimal parsing of the SSH protocol to
// determine when to stop obfuscation (after the first SSH_MSG_NEWKEYS is
// sent and received).
//
// WARNING: doesn't fully conform to net.Conn concurrency semantics: there's
// no synchronization of access to the read/writeBuffers, so concurrent
// calls to one of Read or Write will result in undefined behavior.
//
type ObfuscatedSshConn struct {
	net.Conn
	mode            ObfuscatedSshConnMode
	obfuscator      *Obfuscator
	readDeobfuscate func([]byte)
	writeObfuscate  func([]byte)
	readState       ObfuscatedSshReadState
	writeState      ObfuscatedSshWriteState
	readBuffer      *bytes.Buffer
	writeBuffer     *bytes.Buffer
	transformBuffer *bytes.Buffer
	legacyPadding   bool
}

type ObfuscatedSshConnMode int

const (
	OBFUSCATION_CONN_MODE_CLIENT = iota
	OBFUSCATION_CONN_MODE_SERVER
)

type ObfuscatedSshReadState int

const (
	OBFUSCATION_READ_STATE_IDENTIFICATION_LINES = iota
	OBFUSCATION_READ_STATE_KEX_PACKETS
	OBFUSCATION_READ_STATE_FLUSH
	OBFUSCATION_READ_STATE_FINISHED
)

type ObfuscatedSshWriteState int

const (
	OBFUSCATION_WRITE_STATE_CLIENT_SEND_SEED_MESSAGE = iota
	OBFUSCATION_WRITE_STATE_SERVER_SEND_IDENTIFICATION_LINE_PADDING
	OBFUSCATION_WRITE_STATE_IDENTIFICATION_LINE
	OBFUSCATION_WRITE_STATE_KEX_PACKETS
	OBFUSCATION_WRITE_STATE_FINISHED
)

// NewObfuscatedSshConn creates a new ObfuscatedSshConn.
// The underlying conn must be used for SSH traffic and must have
// transferred no traffic.
//
// In client mode, NewObfuscatedSshConn does not block or initiate network
// I/O. The obfuscation seed message is sent when Write() is first called.
//
// In server mode, NewObfuscatedSshConn cannot completely initialize itself
// without the seed message from the client to derive obfuscation keys. So
// NewObfuscatedSshConn blocks on reading the client seed message from the
// underlying conn.
//
func NewObfuscatedSshConn(
	mode ObfuscatedSshConnMode,
	conn net.Conn,
	obfuscationKeyword string,
	minPadding, maxPadding *int) (*ObfuscatedSshConn, error) {

	var err error
	var obfuscator *Obfuscator
	var readDeobfuscate, writeObfuscate func([]byte)
	var writeState ObfuscatedSshWriteState

	if mode == OBFUSCATION_CONN_MODE_CLIENT {
		obfuscator, err = NewClientObfuscator(
			&ObfuscatorConfig{
				Keyword:    obfuscationKeyword,
				MinPadding: minPadding,
				MaxPadding: maxPadding,
			})
		if err != nil {
			return nil, common.ContextError(err)
		}
		readDeobfuscate = obfuscator.ObfuscateServerToClient
		writeObfuscate = obfuscator.ObfuscateClientToServer
		writeState = OBFUSCATION_WRITE_STATE_CLIENT_SEND_SEED_MESSAGE
	} else {
		// NewServerObfuscator reads a seed message from conn
		obfuscator, err = NewServerObfuscator(
			conn, &ObfuscatorConfig{Keyword: obfuscationKeyword})
		if err != nil {
			// TODO: readForver() equivalent
			return nil, common.ContextError(err)
		}
		readDeobfuscate = obfuscator.ObfuscateClientToServer
		writeObfuscate = obfuscator.ObfuscateServerToClient
		writeState = OBFUSCATION_WRITE_STATE_SERVER_SEND_IDENTIFICATION_LINE_PADDING
	}

	return &ObfuscatedSshConn{
		Conn:            conn,
		mode:            mode,
		obfuscator:      obfuscator,
		readDeobfuscate: readDeobfuscate,
		writeObfuscate:  writeObfuscate,
		readState:       OBFUSCATION_READ_STATE_IDENTIFICATION_LINES,
		writeState:      writeState,
		readBuffer:      new(bytes.Buffer),
		writeBuffer:     new(bytes.Buffer),
		transformBuffer: new(bytes.Buffer),
	}, nil
}

// Read wraps standard Read, transparently applying the obfuscation
// transformations.
func (conn *ObfuscatedSshConn) Read(buffer []byte) (int, error) {
	if conn.readState == OBFUSCATION_READ_STATE_FINISHED {
		return conn.Conn.Read(buffer)
	}
	n, err := conn.readAndTransform(buffer)
	if err != nil {
		err = common.ContextError(err)
	}
	return n, err
}

// Write wraps standard Write, transparently applying the obfuscation
// transformations.
func (conn *ObfuscatedSshConn) Write(buffer []byte) (int, error) {
	if conn.writeState == OBFUSCATION_WRITE_STATE_FINISHED {
		return conn.Conn.Write(buffer)
	}
	err := conn.transformAndWrite(buffer)
	if err != nil {
		return 0, common.ContextError(err)
	}
	// Reports that we wrote all the bytes
	// (although we may have buffered some or all)
	return len(buffer), nil
}

// readAndTransform reads and transforms the downstream bytes stream
// while in an obfucation state. It parses the stream of bytes read
// looking for the first SSH_MSG_NEWKEYS packet sent from the peer,
// after which obfuscation is turned off. Since readAndTransform may
// read in more bytes that the higher-level conn.Read() can consume,
// read bytes are buffered and may be returned in subsequent calls.
//
// readAndTransform also implements a workaround for issues with
// ssh/transport.go exchangeVersions/readVersion and Psiphon's openssh
// server.
//
// Psiphon's server sends extra lines before the version line, as
// permitted by http://www.ietf.org/rfc/rfc4253.txt sec 4.2:
//   The server MAY send other lines of data before sending the
//   version string. [...] Clients MUST be able to process such lines.
//
// A comment in exchangeVersions explains that the golang code doesn't
// support this:
//   Contrary to the RFC, we do not ignore lines that don't
//   start with "SSH-2.0-" to make the library usable with
//   nonconforming servers.
//
// In addition, Psiphon's server sends up to 512 characters per extra
// line. It's not clear that the 255 max string size in sec 4.2 refers
// to the extra lines as well, but in any case golang's code only
// supports 255 character lines.
//
// State OBFUSCATION_READ_STATE_IDENTIFICATION_LINES: in this
// state, extra lines are read and discarded. Once the peer's
// identification string line is read, it is buffered and returned
// as per the requested read buffer size.
//
// State OBFUSCATION_READ_STATE_KEX_PACKETS: reads, deobfuscates,
// and buffers full SSH packets, checking for SSH_MSG_NEWKEYS. Packet
// data is returned as per the requested read buffer size.
//
// State OBFUSCATION_READ_STATE_FLUSH: after SSH_MSG_NEWKEYS, no more
// packets are read by this function, but bytes from the SSH_MSG_NEWKEYS
// packet may need to be buffered due to partial reading.
func (conn *ObfuscatedSshConn) readAndTransform(buffer []byte) (int, error) {

	nextState := conn.readState

	switch conn.readState {
	case OBFUSCATION_READ_STATE_IDENTIFICATION_LINES:
		// TODO: only client should accept multiple lines?
		if conn.readBuffer.Len() == 0 {
			for {
				err := readSshIdentificationLine(
					conn.Conn, conn.readDeobfuscate, conn.readBuffer)
				if err != nil {
					return 0, common.ContextError(err)
				}
				if bytes.HasPrefix(conn.readBuffer.Bytes(), []byte("SSH-")) {
					if bytes.Contains(conn.readBuffer.Bytes(), []byte("Ganymed")) {
						conn.legacyPadding = true
					}
					break
				}
				// Discard extra line
				conn.readBuffer.Truncate(0)
			}
		}
		nextState = OBFUSCATION_READ_STATE_KEX_PACKETS

	case OBFUSCATION_READ_STATE_KEX_PACKETS:
		if conn.readBuffer.Len() == 0 {
			isMsgNewKeys, err := readSshPacket(
				conn.Conn, conn.readDeobfuscate, conn.readBuffer)
			if err != nil {
				return 0, common.ContextError(err)
			}
			if isMsgNewKeys {
				nextState = OBFUSCATION_READ_STATE_FLUSH
			}
		}

	case OBFUSCATION_READ_STATE_FLUSH:
		nextState = OBFUSCATION_READ_STATE_FINISHED

	case OBFUSCATION_READ_STATE_FINISHED:
		return 0, common.ContextError(errors.New("invalid read state"))
	}

	n, err := conn.readBuffer.Read(buffer)
	if err == io.EOF {
		err = nil
	}
	if err != nil {
		return n, common.ContextError(err)
	}
	if conn.readBuffer.Len() == 0 {
		conn.readState = nextState
		if conn.readState == OBFUSCATION_READ_STATE_FINISHED {
			// The buffer memory is no longer used
			conn.readBuffer = nil
		}
	}
	return n, nil
}

// transformAndWrite transforms the upstream bytes stream while in an
// obfucation state, buffers bytes as necessary for parsing, and writes
// transformed bytes to the network connection. Bytes are obfuscated until
// after the first SSH_MSG_NEWKEYS packet is sent.
//
// There are two mode-specific states:
//
// State OBFUSCATION_WRITE_STATE_CLIENT_SEND_SEED_MESSAGE: the initial
// state, when the client has not sent any data. In this state, the seed message
// is injected into the client output stream.
//
// State OBFUSCATION_WRITE_STATE_SERVER_SEND_IDENTIFICATION_LINE_PADDING: the
// initial state, when the server has not sent any data. In this state, the
// additional lines of padding are injected into the server output stream.
// This padding is a partial defense against traffic analysis against the
// otherwise-fixed size server version line. This makes use of the
// "other lines of data" allowance, before the version line, which clients
// will ignore (http://tools.ietf.org/html/rfc4253#section-4.2).
//
// State OBFUSCATION_WRITE_STATE_IDENTIFICATION_LINE: before
// packets are sent, the ssh peer sends an identification line terminated by CRLF:
// http://www.ietf.org/rfc/rfc4253.txt sec 4.2.
// In this state, the CRLF terminator is used to parse message boundaries.
//
// State OBFUSCATION_WRITE_STATE_KEX_PACKETS: follows the binary
// packet protocol, parsing each packet until the first SSH_MSG_NEWKEYS.
// http://www.ietf.org/rfc/rfc4253.txt sec 6:
//     uint32    packet_length
//     byte      padding_length
//     byte[n1]  payload; n1 = packet_length - padding_length - 1
//     byte[n2]  random padding; n2 = padding_length
//     byte[m]   mac (Message Authentication Code - MAC); m = mac_length
// m is 0 as no MAC ha yet been negotiated.
// http://www.ietf.org/rfc/rfc4253.txt sec 7.3, 12:
// The payload for SSH_MSG_NEWKEYS is one byte, the packet type, value 21.
//
// SSH packet padding values are transformed to achieve random, variable length
// padding during the KEX phase as a partial defense against traffic analysis.
// (The transformer can do this since only the payload and not the padding of
// these packets is authenticated in the "exchange hash").
func (conn *ObfuscatedSshConn) transformAndWrite(buffer []byte) error {

	// The seed message (client) and identification line padding (server)
	// are injected before any standard SSH traffic.
	if conn.writeState == OBFUSCATION_WRITE_STATE_CLIENT_SEND_SEED_MESSAGE {
		_, err := conn.Conn.Write(conn.obfuscator.SendSeedMessage())
		if err != nil {
			return common.ContextError(err)
		}
		conn.writeState = OBFUSCATION_WRITE_STATE_IDENTIFICATION_LINE
	} else if conn.writeState == OBFUSCATION_WRITE_STATE_SERVER_SEND_IDENTIFICATION_LINE_PADDING {
		padding, err := makeServerIdentificationLinePadding()
		if err != nil {
			return common.ContextError(err)
		}
		conn.writeObfuscate(padding)
		_, err = conn.Conn.Write(padding)
		if err != nil {
			return common.ContextError(err)
		}
		conn.writeState = OBFUSCATION_WRITE_STATE_IDENTIFICATION_LINE
	}

	// writeBuffer is used to buffer bytes received from Write() until a
	// complete SSH message is received. transformBuffer is used as a scratch
	// buffer for size-changing tranformations, including padding transforms.
	// All data flows as follows:
	// conn.Write() -> writeBuffer -> transformBuffer -> conn.Conn.Write()

	conn.writeBuffer.Write(buffer)

	switch conn.writeState {
	case OBFUSCATION_WRITE_STATE_IDENTIFICATION_LINE:
		hasIdentificationLine := extractSshIdentificationLine(
			conn.writeBuffer, conn.transformBuffer)
		if hasIdentificationLine {
			conn.writeState = OBFUSCATION_WRITE_STATE_KEX_PACKETS
		}

	case OBFUSCATION_WRITE_STATE_KEX_PACKETS:
		hasMsgNewKeys, err := extractSshPackets(
			conn.legacyPadding, conn.writeBuffer, conn.transformBuffer)
		if err != nil {
			return common.ContextError(err)
		}
		if hasMsgNewKeys {
			conn.writeState = OBFUSCATION_WRITE_STATE_FINISHED
		}

	case OBFUSCATION_WRITE_STATE_FINISHED:
		return common.ContextError(errors.New("invalid write state"))
	}

	if conn.transformBuffer.Len() > 0 {
		sendData := conn.transformBuffer.Next(conn.transformBuffer.Len())
		conn.writeObfuscate(sendData)
		_, err := conn.Conn.Write(sendData)
		if err != nil {
			return common.ContextError(err)
		}
	}

	if conn.writeState == OBFUSCATION_WRITE_STATE_FINISHED {
		if conn.writeBuffer.Len() > 0 {
			// After SSH_MSG_NEWKEYS, any remaining bytes are un-obfuscated
			_, err := conn.Conn.Write(conn.writeBuffer.Bytes())
			if err != nil {
				return common.ContextError(err)
			}
		}
		// The buffer memory is no longer used
		conn.writeBuffer = nil
		conn.transformBuffer = nil
	}
	return nil
}

func readSshIdentificationLine(
	conn net.Conn,
	deobfuscate func([]byte),
	readBuffer *bytes.Buffer) error {

	// TODO: less redundant string searching?
	var oneByte [1]byte
	var validLine = false
	readBuffer.Grow(SSH_MAX_SERVER_LINE_LENGTH)
	for i := 0; i < SSH_MAX_SERVER_LINE_LENGTH; i++ {
		_, err := io.ReadFull(conn, oneByte[:])
		if err != nil {
			return common.ContextError(err)
		}
		deobfuscate(oneByte[:])
		readBuffer.WriteByte(oneByte[0])
		if bytes.HasSuffix(readBuffer.Bytes(), []byte("\r\n")) {
			validLine = true
			break
		}
	}
	if !validLine {
		return common.ContextError(errors.New("invalid identification line"))
	}
	return nil
}

func readSshPacket(
	conn net.Conn,
	deobfuscate func([]byte),
	readBuffer *bytes.Buffer) (bool, error) {

	prefixOffset := readBuffer.Len()

	readBuffer.Grow(SSH_PACKET_PREFIX_LENGTH)
	n, err := readBuffer.ReadFrom(io.LimitReader(conn, SSH_PACKET_PREFIX_LENGTH))
	if err == nil && n != SSH_PACKET_PREFIX_LENGTH {
		err = errors.New("unxpected number of bytes read")
	}
	if err != nil {
		return false, common.ContextError(err)
	}

	prefix := readBuffer.Bytes()[prefixOffset : prefixOffset+SSH_PACKET_PREFIX_LENGTH]
	deobfuscate(prefix)

	_, _, payloadLength, messageLength, err := getSshPacketPrefix(prefix)
	if err != nil {
		return false, common.ContextError(err)
	}

	remainingReadLength := messageLength - SSH_PACKET_PREFIX_LENGTH
	readBuffer.Grow(remainingReadLength)
	n, err = readBuffer.ReadFrom(io.LimitReader(conn, int64(remainingReadLength)))
	if err == nil && n != int64(remainingReadLength) {
		err = errors.New("unxpected number of bytes read")
	}
	if err != nil {
		return false, common.ContextError(err)
	}

	remainingBytes := readBuffer.Bytes()[prefixOffset+SSH_PACKET_PREFIX_LENGTH:]
	deobfuscate(remainingBytes)

	isMsgNewKeys := false
	if payloadLength > 0 {
		packetType := int(readBuffer.Bytes()[prefixOffset+SSH_PACKET_PREFIX_LENGTH])
		if packetType == SSH_MSG_NEWKEYS {
			isMsgNewKeys = true
		}
	}
	return isMsgNewKeys, nil
}

// From the original patch to sshd.c:
// https://bitbucket.org/psiphon/psiphon-circumvention-system/commits/f40865ce624b680be840dc2432283c8137bd896d
func makeServerIdentificationLinePadding() ([]byte, error) {
	paddingLength, err := common.MakeSecureRandomInt(OBFUSCATE_MAX_PADDING - 2) // 2 = CRLF
	if err != nil {
		return nil, common.ContextError(err)
	}
	paddingLength += 2
	padding := make([]byte, paddingLength)

	// For backwards compatibility with some clients, send no more than 512 characters
	// per line (including CRLF). To keep the padding distribution between 0 and OBFUSCATE_MAX_PADDING
	// characters, we send lines that add up to padding_length characters including all CRLFs.

	minLineLength := 2
	maxLineLength := 512
	lineStartIndex := 0
	for paddingLength > 0 {
		lineLength := paddingLength
		if lineLength > maxLineLength {
			lineLength = maxLineLength
		}
		// Leave enough padding allowance to send a full CRLF on the last line
		if paddingLength-lineLength > 0 &&
			paddingLength-lineLength < minLineLength {
			lineLength -= minLineLength - (paddingLength - lineLength)
		}
		padding[lineStartIndex+lineLength-2] = '\r'
		padding[lineStartIndex+lineLength-1] = '\n'
		lineStartIndex += lineLength
		paddingLength -= lineLength
	}

	return padding, nil
}

func extractSshIdentificationLine(writeBuffer, transformBuffer *bytes.Buffer) bool {
	index := bytes.Index(writeBuffer.Bytes(), []byte("\r\n"))
	if index != -1 {
		lineLength := index + 2 // + 2 for \r\n
		transformBuffer.Write(writeBuffer.Next(lineLength))
		return true
	}
	return false
}

func extractSshPackets(
	legacyPadding bool, writeBuffer, transformBuffer *bytes.Buffer) (bool, error) {

	hasMsgNewKeys := false
	for writeBuffer.Len() >= SSH_PACKET_PREFIX_LENGTH {

		packetLength, paddingLength, payloadLength, messageLength, err := getSshPacketPrefix(
			writeBuffer.Bytes()[:SSH_PACKET_PREFIX_LENGTH])
		if err != nil {
			return false, common.ContextError(err)
		}

		if writeBuffer.Len() < messageLength {
			// We don't have the complete packet yet
			break
		}

		packet := writeBuffer.Next(messageLength)

		if payloadLength > 0 {
			packetType := int(packet[SSH_PACKET_PREFIX_LENGTH])
			if packetType == SSH_MSG_NEWKEYS {
				hasMsgNewKeys = true
			}
		}

		transformedPacketOffset := transformBuffer.Len()
		transformBuffer.Write(packet)
		transformedPacket := transformBuffer.Bytes()[transformedPacketOffset:]

		// Padding transformation

		extraPaddingLength := 0

		if !legacyPadding {
			// This does not satisfy RFC 4253 sec. 6 constraints:
			// - The goal is to vary packet sizes as much as possible.
			// - We implement both the client and server sides and both sides accept
			//   less constrained paddings (for plaintext packets).
			possibleExtraPaddingLength := (SSH_MAX_PADDING_LENGTH - paddingLength)
			if possibleExtraPaddingLength > 0 {

				// TODO: proceed without padding if MakeSecureRandom* fails?

				// extraPaddingLength is integer in range [0, possiblePadding + 1)
				extraPaddingLength, err = common.MakeSecureRandomInt(
					possibleExtraPaddingLength + 1)
				if err != nil {
					return false, common.ContextError(err)
				}
			}
		} else {
			// See RFC 4253 sec. 6 for constraints
			possiblePaddings := (SSH_MAX_PADDING_LENGTH - paddingLength) / SSH_PADDING_MULTIPLE
			if possiblePaddings > 0 {

				// selectedPadding is integer in range [0, possiblePaddings)
				selectedPadding, err := common.MakeSecureRandomInt(possiblePaddings)
				if err != nil {
					return false, common.ContextError(err)
				}
				extraPaddingLength = selectedPadding * SSH_PADDING_MULTIPLE
			}
		}

		extraPadding, err := common.MakeSecureRandomBytes(extraPaddingLength)
		if err != nil {
			return false, common.ContextError(err)
		}

		setSshPacketPrefix(
			transformedPacket,
			packetLength+extraPaddingLength,
			paddingLength+extraPaddingLength)

		transformBuffer.Write(extraPadding)
	}

	return hasMsgNewKeys, nil
}

func getSshPacketPrefix(buffer []byte) (int, int, int, int, error) {

	packetLength := int(binary.BigEndian.Uint32(buffer[0 : SSH_PACKET_PREFIX_LENGTH-1]))

	if packetLength < 1 || packetLength > SSH_MAX_PACKET_LENGTH {
		return 0, 0, 0, 0, common.ContextError(errors.New("invalid ssh packet length"))
	}

	paddingLength := int(buffer[SSH_PACKET_PREFIX_LENGTH-1])
	payloadLength := packetLength - paddingLength - 1
	messageLength := SSH_PACKET_PREFIX_LENGTH + packetLength - 1

	return packetLength, paddingLength, payloadLength, messageLength, nil
}

func setSshPacketPrefix(buffer []byte, packetLength, paddingLength int) {
	binary.BigEndian.PutUint32(buffer, uint32(packetLength))
	buffer[SSH_PACKET_PREFIX_LENGTH-1] = byte(paddingLength)
}